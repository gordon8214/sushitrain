// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// Config is where the service keeps its state and what it says about itself
type Config struct {
	// The index (files.sqlite) and, once the mount can be written, edits not committed yet. Nothing here can be
	// rebuilt from a gateway.
	StateDir string
	// Verified content and partial downloads, which can be fetched again
	CacheDir string
	// Where the credentials for gateways are kept
	Credentials CredentialStore
	// Bytes of content the cache may keep, beyond what is kept on this device on purpose
	CacheBudget int64
	// The name and platform gateways list this client under
	ClientName string
	Platform   string
	// Allow pairing with a gateway on this machine (for tests: a gateway is what this machine has instead of files)
	AllowLocalGateway bool
	Logger            *slog.Logger
	// Told about what someone should know of: a change that can't be sent, a gateway out of reach
	Notify func(Notice)
	// Called at points where a crash would leave work half done, for tests that stop there (an error stops)
	Hook func(point string) error
	// How long a folder must be quiet before its changes are sent (DefaultSettleDelay when 0)
	SettleDelay time.Duration
	// The freedesktop thumbnail cache, filled from the gateways' thumbnails ($XDG_CACHE_HOME/thumbnails when empty)
	ThumbnailDir string
}

// Notice is something to tell the person using the machine
type Notice struct {
	// "review" (changes can't be sent: export or discard them), "unreachable" (a gateway is out of reach)
	Kind    string
	Message string
}

// DefaultCacheBudget is how much content the cache keeps by default, as on the other platforms
const DefaultCacheBudget = 5 << 30

// Service is a client of gateways with the folders added from them
type Service struct {
	cfg        Config
	store      *Store
	log        *slog.Logger
	cache      *cache
	thumbnails *thumbnailer

	previewMutex sync.Mutex
	mutex        sync.Mutex
	clients      map[string]*remoteclient.Client
	loops        map[int64]*folderLoop
	// Folders being removed: their loops aren't started again, by a restart that overlaps the removal
	removing     map[int64]bool
	invalidator  func(Changes)
	work         map[uint64]*workingCopy
	settleTimers map[int64]*time.Timer
	busySince    map[int64]time.Time
	// Since when a folder's changes are held back by one that keeps changing
	heldSince map[int64]time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// Settling and collecting staged content take turns
	settleMutex sync.Mutex
	outboxWake  chan struct{}
}

// Open opens the service's index and cache; Start starts following the gateways
func Open(cfg Config) (*Service, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CacheBudget <= 0 {
		cfg.CacheBudget = DefaultCacheBudget
	}
	if cfg.Platform == "" {
		cfg.Platform = "Linux"
	}
	if cfg.SettleDelay <= 0 {
		cfg.SettleDelay = DefaultSettleDelay
	}
	if cfg.ClientName == "" {
		cfg.ClientName, _ = os.Hostname()
	}
	for _, dir := range []string{cfg.StateDir, cfg.CacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	store, err := OpenStore(filepath.Join(cfg.StateDir, "files.sqlite"))
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, store: store, log: cfg.Logger, clients: map[string]*remoteclient.Client{},
		loops: map[int64]*folderLoop{}, removing: map[int64]bool{}, work: map[uint64]*workingCopy{}, settleTimers: map[int64]*time.Timer{},
		busySince: map[int64]time.Time{}, heldSince: map[int64]time.Time{}, outboxWake: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if s.cache, err = openCache(s); err != nil {
		store.Close()
		return nil, err
	}
	s.thumbnails = newThumbnailer(s)
	if err := s.recover(); err != nil {
		store.Close()
		return nil, fmt.Errorf("recovering changes: %w", err)
	}
	return s, nil
}

func (s *Service) hook(point string) error {
	if s.cfg.Hook != nil {
		return s.cfg.Hook(point)
	}
	return nil
}

func (s *Service) notify(n Notice) {
	if s.cfg.Notify != nil {
		s.cfg.Notify(n)
	}
}

// reviewNeeded tells that changes can't be sent
func (s *Service) reviewNeeded() {
	s.notify(Notice{Kind: "review", Message: "Some changes can't be sent to their gateway. Export or discard them " +
		"with synctrain-files pending, or in Synctrain's window."})
}

// SetInvalidator sets what is told about changes to the tree once they are stored (the mount, which drops what the
// kernel cached of them)
func (s *Service) SetInvalidator(fn func(Changes)) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.invalidator = fn
}

func (s *Service) changed(c Changes) {
	if c.Empty() {
		return
	}
	s.mutex.Lock()
	fn := s.invalidator
	s.mutex.Unlock()
	if fn != nil {
		fn(c)
	}
}

// Start follows the change feeds of all folders
func (s *Service) Start() error {
	folders, err := s.folderRefs()
	if err != nil {
		return err
	}
	for _, f := range folders {
		s.startLoop(f)
	}
	s.wg.Add(4)
	go func() {
		defer s.wg.Done()
		s.cache.run(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.runPruning(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		if s.thumbnails != nil {
			s.thumbnails.run(s.ctx)
		}
	}()
	go func() {
		defer s.wg.Done()
		s.runOutbox(s.ctx)
	}()
	// Changes from before a restart that weren't settled
	rows, err := s.store.db.Query(`SELECT DISTINCT folder FROM items WHERE local & ? != 0 AND local & ? = 0`,
		localCreated|localContent|localMoved|localDeleted, localOnly|localFolded)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var folder int64
		if err := rows.Scan(&folder); err != nil {
			return err
		}
		s.settleLater(folder)
	}
	return rows.Err()
}

// Close stops everything and closes the index
func (s *Service) Close() error {
	s.cancel()
	s.mutex.Lock()
	for _, t := range s.settleTimers {
		t.Stop()
	}
	s.mutex.Unlock()
	// A settle in progress finishes
	s.settleMutex.Lock()
	s.settleMutex.Unlock()
	s.wg.Wait()
	s.cache.close()
	s.mutex.Lock()
	for _, c := range s.clients {
		c.Close()
	}
	for _, wc := range s.work {
		wc.mutex.Lock()
		if wc.file != nil {
			wc.file.Close()
		}
		wc.mutex.Unlock()
	}
	s.mutex.Unlock()
	return s.store.Close()
}

type gatewayRef struct {
	serviceID  string
	host       string
	port       int
	pins       []string
	name       string
	generation int64
}

func (s *Service) gatewayRef(serviceID string) (gatewayRef, error) {
	g := gatewayRef{serviceID: serviceID}
	var pins string
	var generation sql.NullInt64
	err := s.store.db.QueryRow(`SELECT host, port, pins, name, authority_generation FROM gateways WHERE service_id = ?`,
		serviceID).Scan(&g.host, &g.port, &pins, &g.name, &generation)
	g.pins, g.generation = strings.Fields(pins), generation.Int64
	return g, err
}

// client is the client of a gateway, shared by everything that talks to it
func (s *Service) client(serviceID string) (*remoteclient.Client, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if c, ok := s.clients[serviceID]; ok {
		return c, nil
	}
	g, err := s.gatewayRef(serviceID)
	if err != nil {
		return nil, err
	}
	c := remoteclient.New(remoteclient.Endpoint{ServiceID: serviceID, Host: g.host, Port: g.port, Pins: g.pins},
		func() (string, error) { return s.cfg.Credentials.Credential(serviceID) })
	c.UserAgent = "synctrain-files"
	s.clients[serviceID] = c
	return c, nil
}

// dropClient forgets a gateway's client (after its pins or address changed)
func (s *Service) dropClient(serviceID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if c, ok := s.clients[serviceID]; ok {
		c.Close()
		delete(s.clients, serviceID)
	}
}

// Gateway is a paired gateway, for listing
type Gateway struct {
	ServiceID string `json:"serviceID"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	// Host and port as people write them (host:port, [fd00::1]:port)
	Address string `json:"address"`
	Pinned  bool   `json:"pinned"`
	State   string `json:"state,omitempty"`
	Folders int    `json:"folders"`
}

// Gateways lists the paired gateways
func (s *Service) Gateways() ([]Gateway, error) {
	rows, err := s.store.db.Query(`SELECT g.service_id, g.name, g.host, g.port, g.pins, g.state,
		(SELECT COUNT(*) FROM folders AS f WHERE f.service_id = g.service_id) FROM gateways AS g ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Gateway
	for rows.Next() {
		var g Gateway
		var pins string
		if err := rows.Scan(&g.ServiceID, &g.Name, &g.Host, &g.Port, &pins, &g.State, &g.Folders); err != nil {
			return nil, err
		}
		g.Pinned = pins != ""
		g.Address = remoteclient.FormatAddress(g.Host, g.Port)
		result = append(result, g)
	}
	return result, rows.Err()
}

// ErrLocalGateway refuses pairing with a gateway on this machine
var ErrLocalGateway = errors.New("the gateway is on this machine: Files mode shows the folders of another system")

// Pair pairs with the gateway of a pairing link, waiting until the request is approved there. requested, if not nil,
// is called once the gateway has the request.
func (s *Service) Pair(ctx context.Context, link string, requested func()) (Gateway, error) {
	inv, err := remoteclient.ParseInvitation(link)
	if err != nil {
		return Gateway{}, err
	}
	if !s.cfg.AllowLocalGateway && isLocalHost(ctx, inv.Host) {
		return Gateway{}, ErrLocalGateway
	}
	clientID, err := s.store.ClientID()
	if err != nil {
		return Gateway{}, err
	}
	credential, err := remoteclient.Pair(ctx, inv, remoteclient.Pairing{ClientID: clientID,
		ClientName: s.cfg.ClientName, Platform: s.cfg.Platform, Requested: func(remoteapi.PairingPending) {
			if requested != nil {
				requested()
			}
		}})
	if err != nil {
		return Gateway{}, err
	}
	if err := s.cfg.Credentials.SetCredential(inv.ServiceID, credential); err != nil {
		return Gateway{}, fmt.Errorf("keeping the credential: %w", err)
	}
	endpoint := inv.Endpoint()
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO gateways (service_id, host, port, pins, name, added_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (service_id) DO UPDATE SET host = excluded.host, port = excluded.port, pins = excluded.pins,
				state = ''`, inv.ServiceID, inv.Host, inv.Port, joinWords(endpoint.Pins), inv.Host, nowUnix())
		return err
	})
	if err != nil {
		return Gateway{}, err
	}
	s.dropClient(inv.ServiceID)
	g, err := s.gatewayRef(inv.ServiceID)
	if err != nil {
		return Gateway{}, err
	}
	client, err := s.client(inv.ServiceID)
	if err != nil {
		return Gateway{}, err
	}
	if err := s.checkCapabilities(ctx, g, client); err != nil {
		return Gateway{}, err
	}
	// Folders of a gateway paired again pick up where they were
	folders, err := s.folderRefs()
	if err != nil {
		return Gateway{}, err
	}
	for _, f := range folders {
		if f.serviceID == inv.ServiceID {
			s.restartLoop(f)
		}
	}
	list, err := s.Gateways()
	if err != nil {
		return Gateway{}, err
	}
	for _, g := range list {
		if g.ServiceID == inv.ServiceID {
			return g, nil
		}
	}
	return Gateway{}, errors.New("the gateway was not stored")
}

// How long isLocalHost waits for the name to resolve
const localHostLookupTimeout = 5 * time.Second

// isLocalHost reports whether host is this machine. A name that doesn't resolve in time isn't.
func isLocalHost(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, localHostLookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false
	}
	addrs, _ := net.InterfaceAddrs()
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsUnspecified() {
			return true
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip.IP) {
				return true
			}
		}
	}
	return false
}

// Forget forgets a gateway and its credential. It is refused while folders of it are here.
func (s *Service) Forget(serviceID string) error {
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM folders WHERE service_id = ?`, serviceID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%d folders of the gateway are here: remove them first", count)
	}
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM gateways WHERE service_id = ?`, serviceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("no such gateway")
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.dropClient(serviceID)
	return s.cfg.Credentials.DeleteCredential(serviceID)
}

// AddressChange is a gateway with its new address, and whether it answered there
type AddressChange struct {
	Gateway Gateway `json:"gateway"`
	// Why the gateway couldn't be used at the new address just now (empty when it answered). The address is kept
	// either way: one that works only away from the gateway's network can't be checked from inside it.
	Problem string `json:"problem,omitempty"`
}

// How long changing an address waits for the gateway to answer there
const addressCheckTimeout = 15 * time.Second

// ErrUnpinnedHost refuses another host for a gateway paired without a pinned key
var ErrUnpinnedHost = errors.New("this gateway is trusted by the name in its certificate, not by a pinned key: pair " +
	"with it again from a pairing link with the new name (only its port can be changed here)")

// SetGatewayAddress changes where this machine reaches a paired gateway (host or host:port; a port left out keeps the
// current one), as when another name reaches it from outside its network. The pairing stays: the credential, and the
// pins, which a gateway at the new address has to match. A gateway paired without a pin (its certificate is trusted by
// the system, for its name) only gets another port: with another host, its credential would go to whatever has a valid
// certificate for that name, so that takes pairing again (ErrUnpinnedHost).
func (s *Service) SetGatewayAddress(ctx context.Context, serviceID, address string) (AddressChange, error) {
	a, err := remoteclient.ParseAddress(address)
	if err != nil {
		return AddressChange{}, err
	}
	g, err := s.gatewayRef(serviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return AddressChange{}, errors.New("no such gateway")
	} else if err != nil {
		return AddressChange{}, err
	}
	if a.Port == 0 {
		a.Port = g.port
	}
	if len(g.pins) == 0 && !strings.EqualFold(a.Host, g.host) {
		return AddressChange{}, ErrUnpinnedHost
	}
	if !s.cfg.AllowLocalGateway && isLocalHost(ctx, a.Host) {
		return AddressChange{}, ErrLocalGateway
	}
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE gateways SET host = ?, port = ?, state = '' WHERE service_id = ?`, a.Host, a.Port,
			serviceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("no such gateway")
		}
		// The changes waiting for the gateway try the new address now, rather than after the backoff the old one gave
		_, err = tx.Exec(`UPDATE outbox SET not_before = 0, attempts = 0 WHERE state IN `+activeStates+`
			AND folder IN (SELECT id FROM folders WHERE service_id = ?)`, serviceID)
		return err
	})
	if err != nil {
		return AddressChange{}, err
	}
	s.log.Info("A gateway is reached at another address now", "gateway", g.name, "address",
		remoteclient.FormatAddress(a.Host, a.Port))
	s.dropClient(serviceID)

	var change AddressChange
	if g, err = s.gatewayRef(serviceID); err == nil {
		var client *remoteclient.Client
		if client, err = s.client(serviceID); err == nil {
			checkCtx, cancel := context.WithTimeout(ctx, addressCheckTimeout)
			err = s.checkCapabilities(checkCtx, g, client)
			cancel()
		}
	}
	change.Problem = addressProblem(err)
	switch remoteclient.KindOf(err) {
	case remoteclient.KindUntrusted:
		s.setGatewayState(serviceID, "untrusted")
	case remoteclient.KindUnauthenticated:
		s.setGatewayState(serviceID, "unauthenticated")
	}

	// Its folders try the new address now rather than after their backoff (the outbox's waiting changes too, above)
	folders, err := s.folderRefs()
	if err != nil {
		return AddressChange{}, err
	}
	for _, f := range folders {
		if f.serviceID == serviceID {
			s.restartLoop(f)
		}
	}
	s.wakeOutbox()
	list, err := s.Gateways()
	if err != nil {
		return AddressChange{}, err
	}
	for _, g := range list {
		if g.ServiceID == serviceID {
			change.Gateway = g
			return change, nil
		}
	}
	return AddressChange{}, errors.New("no such gateway")
}

// addressProblem says why a gateway couldn't be used at a new address, from its capabilities check ("" when it could)
func addressProblem(err error) string {
	var e *remoteclient.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errOtherService):
		return "another gateway answers there"
	case remoteclient.KindOf(err) == remoteclient.KindUntrusted:
		return "what answers there doesn't have the certificate this machine paired with"
	case remoteclient.KindOf(err) == remoteclient.KindUnauthenticated:
		return "the gateway there doesn't accept this machine: pair with it again"
	case remoteclient.KindOf(err) == remoteclient.KindUnreachable:
		return "it can't be reached there from this machine now (" + err.Error() + ")"
	case errors.As(err, &e) && (e.Kind == remoteclient.KindProtocol || e.Kind == remoteclient.KindNotFound),
		errors.Is(err, errAPIVersion):
		return "something that isn't this gateway answers there (" + err.Error() + ")"
	default:
		return "it couldn't be checked there (" + err.Error() + ")"
	}
}

// RemoteFolders lists the folders a gateway lets this client access
func (s *Service) RemoteFolders(ctx context.Context, serviceID string) ([]remoteapi.Folder, error) {
	client, err := s.client(serviceID)
	if err != nil {
		return nil, err
	}
	return client.Folders(ctx)
}

type folderRef struct {
	id        int64
	serviceID string
	remoteID  string
	dirName   string
	rootItem  int64
}

func (s *Service) folderRefs() ([]folderRef, error) {
	rows, err := s.store.db.Query(`SELECT id, service_id, remote_id, dir_name, root_item FROM folders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []folderRef
	for rows.Next() {
		var f folderRef
		if err := rows.Scan(&f.id, &f.serviceID, &f.remoteID, &f.dirName, &f.rootItem); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// Folder is a folder added from a gateway, for listing
type Folder struct {
	ID        int64  `json:"id"`
	ServiceID string `json:"serviceID"`
	Gateway   string `json:"gateway"`
	RemoteID  string `json:"remoteID"`
	Label     string `json:"label"`
	DirName   string `json:"dirName"`
	CanWrite  bool   `json:"canWrite"`
	State     string `json:"state,omitempty"`
	Listed    bool   `json:"listed"`
}

// Folders lists the folders added from gateways
func (s *Service) Folders() ([]Folder, error) {
	rows, err := s.store.db.Query(`SELECT f.id, f.service_id, g.name, f.remote_id, f.label, f.dir_name, f.can_write,
		f.state, EXISTS (SELECT 1 FROM cursors AS c WHERE c.folder = f.id)
		FROM folders AS f JOIN gateways AS g ON g.service_id = f.service_id ORDER BY f.dir_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.ServiceID, &f.Gateway, &f.RemoteID, &f.Label, &f.DirName, &f.CanWrite, &f.State,
			&f.Listed); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// AddFolder adds a folder of a gateway to the mount, named after its label, and starts listing it
func (s *Service) AddFolder(ctx context.Context, serviceID, remoteID string) (Folder, error) {
	remote, err := s.RemoteFolders(ctx, serviceID)
	if err != nil {
		return Folder{}, err
	}
	i := slices.IndexFunc(remote, func(f remoteapi.Folder) bool { return f.ID == remoteID })
	if i < 0 {
		return Folder{}, fmt.Errorf("the gateway has no folder %q for this client", remoteID)
	}
	rf := remote[i]
	if !rf.Permissions.Read {
		return Folder{}, fmt.Errorf("this client may not read folder %q", remoteID)
	}
	var id int64
	var changes Changes
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM folders WHERE service_id = ? AND remote_id = ?`, serviceID,
			remoteID).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return fmt.Errorf("folder %q is here already", rf.Label)
		}
		dirName, err := uniqueDirName(tx, rf.Label, rf.ID)
		if err != nil {
			return err
		}
		node, err := allocate(tx, metaNextNode)
		if err != nil {
			return err
		}
		// The root's row first, so that the folder can name it
		res, err := tx.Exec(`INSERT INTO items (folder, parent, name, kind, node) VALUES (0, NULL, ?, ?, ?)`, dirName,
			kindRoot, node)
		if err != nil {
			return err
		}
		root, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// Never the ID of a removed folder, so that nothing left of one can be taken for this one's
		if id, err = allocate(tx, metaNextFolder); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO folders (id, service_id, remote_id, label, dir_name, root_item, can_read,
			can_write, added_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, serviceID, remoteID, rf.Label, dirName, root,
			rf.Permissions.Read, rf.Permissions.Write, nowUnix()); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET folder = ? WHERE id = ?`, id, root); err != nil {
			return err
		}
		changes.entry(rootNode, dirName, 0)
		return nil
	})
	if err != nil {
		return Folder{}, err
	}
	s.changed(changes)
	folders, err := s.folderRefs()
	if err != nil {
		return Folder{}, err
	}
	for _, f := range folders {
		if f.id == id {
			s.startLoop(f)
		}
	}
	list, err := s.Folders()
	if err != nil {
		return Folder{}, err
	}
	for _, f := range list {
		if f.ID == id {
			return f, nil
		}
	}
	return Folder{}, errors.New("the folder was not stored")
}

// uniqueDirName is a directory name for a folder in the mount: its label, made a valid name, and made unique
func uniqueDirName(tx *sql.Tx, label, id string) (string, error) {
	base := normalizedName(strings.ReplaceAll(strings.TrimSpace(label), "/", "-"))
	if !validName(base) || base == trackerIgnoreName {
		base = normalizedName(strings.ReplaceAll(id, "/", "-"))
	}
	// Labels and IDs come from the gateway: neither may make a name that isn't one
	if !validName(base) || base == trackerIgnoreName || len(base) > 200 {
		base = "Folder"
	}
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s (%d)", base, i)
		}
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM folders WHERE dir_name = ?`, name).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return name, nil
		}
	}
}

// ErrUnresolved refuses to remove a folder whose changes the gateway hasn't committed
var ErrUnresolved = errors.New("changes made here aren't on the gateway yet")

// RemoveFolder takes a folder out of the mount. It stays on the gateway; its cached content goes as the cache needs
// room. It is refused while changes made here aren't committed; files that are only here are moved out first.
// Returns where they went, and how many.
func (s *Service) RemoveFolder(id int64) (string, int, error) {
	n, err := s.Unresolved(id)
	if err != nil {
		return "", 0, err
	}
	if n > 0 {
		return "", 0, fmt.Errorf("%w: %d (wait for them, or export or discard them)", ErrUnresolved, n)
	}
	// Nothing new can be changed in it while its files are moved out: a change made meanwhile would go with it
	var canWrite bool
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT can_write FROM folders WHERE id = ?`, id).Scan(&canWrite); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("no such folder")
			}
			return err
		}
		_, err := tx.Exec(`UPDATE folders SET can_write = 0 WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	reopen := func() {
		_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE folders SET can_write = ? WHERE id = ?`, canWrite, id)
			return err
		})
	}
	dest, moved, err := s.leftovers(id)
	if err != nil {
		reopen()
		return "", 0, fmt.Errorf("moving files that are only here out of the folder: %w", err)
	}
	if err := s.removeFolder(id); err != nil {
		reopen()
		return "", 0, err
	}
	return dest, moved, nil
}

func (s *Service) removeFolder(id int64) error {
	// Nothing starts its loop again meanwhile (a gateway's address changing, say); if it stays, it's followed again
	s.mutex.Lock()
	s.removing[id] = true
	s.mutex.Unlock()
	running := s.stopLoop(id)
	removed := false
	defer func() {
		s.mutex.Lock()
		delete(s.removing, id)
		s.mutex.Unlock()
		if !removed && running {
			if folders, err := s.folderRefs(); err == nil {
				for _, f := range folders {
					if f.id == id {
						s.startLoop(f)
					}
				}
			}
		}
	}()
	var changes Changes
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		// A handle opened before writing stopped may have changed something since it was counted
		if n, err := unresolved(tx, id); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: %d (wait for them, or export or discard them)", ErrUnresolved, n)
		}
		var dirName string
		var node int64
		if err := tx.QueryRow(`SELECT f.dir_name, i.node FROM folders AS f JOIN items AS i ON i.id = f.root_item
			WHERE f.id = ?`, id).Scan(&dirName, &node); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("no such folder")
			}
			return err
		}
		for _, statement := range []string{
			`DELETE FROM remote_items WHERE folder = ?`,
			`DELETE FROM transfers WHERE item IN (SELECT id FROM items WHERE folder = ?)`,
			`DELETE FROM work WHERE item IN (SELECT id FROM items WHERE folder = ?)`,
			`DELETE FROM outbox WHERE folder = ?`,
			`DELETE FROM items WHERE folder = ?`,
			`DELETE FROM cursors WHERE folder = ?`,
			`DELETE FROM snapshots WHERE folder = ?`,
			`DELETE FROM folders WHERE id = ?`,
		} {
			if _, err := tx.Exec(statement, id); err != nil {
				return err
			}
		}
		changes.removed(rootNode, dirName, uint64(node))
		return nil
	})
	if err != nil {
		return err
	}
	removed = true
	s.changed(changes)
	return nil
}

// Refresh brings a folder up to date at once (for the CLI and tests); the loops do it on their own
func (s *Service) Refresh(ctx context.Context, id int64) error {
	folders, err := s.folderRefs()
	if err != nil {
		return err
	}
	for _, f := range folders {
		if f.id == id {
			client, err := s.client(f.serviceID)
			if err != nil {
				return err
			}
			return s.refreshFolder(ctx, f, client, 0)
		}
	}
	return errors.New("no such folder")
}

// folderLoop is the goroutine that keeps a folder up to date
type folderLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Service) startLoop(f folderRef) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.loops[f.id]; ok || s.removing[f.id] || s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	loop := &folderLoop{cancel: cancel, done: make(chan struct{})}
	s.loops[f.id] = loop
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(loop.done)
		s.followFolder(ctx, f)
	}()
}

// stopLoop stops a folder's loop and waits until it has, so that nothing it was doing is written afterwards (into a
// folder being removed, say). It reports whether there was one.
func (s *Service) stopLoop(id int64) bool {
	s.mutex.Lock()
	loop, ok := s.loops[id]
	delete(s.loops, id)
	s.mutex.Unlock()
	if !ok {
		return false
	}
	loop.cancel()
	select {
	case <-loop.done:
	case <-time.After(time.Minute):
		s.log.Warn("A folder's updates didn't stop", "folder", id)
	}
	return true
}

// restartLoop starts a folder's loop again, if it runs: one stopped by a removal stays stopped
func (s *Service) restartLoop(f folderRef) {
	if s.stopLoop(f.id) {
		s.startLoop(f)
	}
}

// How long the gateway may hold a request for changes
const longPoll = 60 * time.Second

// followFolder keeps a folder up to date: a refresh, then long polls, backing off while the gateway can't be reached
func (s *Service) followFolder(ctx context.Context, f folderRef) {
	backoff := time.Second
	checked := time.Time{}
	for ctx.Err() == nil {
		client, err := s.client(f.serviceID)
		if err == nil && time.Since(checked) > 5*time.Minute {
			var g gatewayRef
			if g, err = s.gatewayRef(f.serviceID); err == nil {
				if err = s.checkCapabilities(ctx, g, client); err == nil {
					checked = time.Now()
				}
			}
		}
		if err == nil {
			err = s.refreshFolder(ctx, f, client, longPoll)
		}
		if err == nil {
			backoff = time.Second
			s.setFolderState(f.id, "")
			continue
		}
		if ctx.Err() != nil {
			return
		}
		delay := backoff
		var e *remoteclient.Error
		if errors.As(err, &e) && e.RetryAfter > 0 {
			delay = max(delay, e.RetryAfter)
		}
		switch remoteclient.KindOf(err) {
		case remoteclient.KindUnauthenticated:
			s.setGatewayState(f.serviceID, "unauthenticated")
			delay = 5 * time.Minute
		case remoteclient.KindUntrusted:
			s.setGatewayState(f.serviceID, "untrusted")
			delay = 5 * time.Minute
		case remoteclient.KindForbidden, remoteclient.KindNotFound:
			// Access to the folder was taken away, or it's gone from the gateway
			s.setFolderState(f.id, "unavailable")
			delay = 5 * time.Minute
		default:
			s.setFolderState(f.id, "unreachable")
		}
		s.log.Debug("Could not refresh a folder", "folder", f.dirName, "error", err, "retry", delay)
		backoff = min(backoff*2, time.Minute)
		// Some jitter, so that folders of one gateway don't all come back at once
		delay += time.Duration(rand.Int64N(int64(time.Second)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *Service) setFolderState(id int64, state string) {
	_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE folders SET state = ? WHERE id = ? AND state != ?`, state, id, state)
		return err
	})
}

func (s *Service) setGatewayState(serviceID, state string) {
	_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE gateways SET state = ? WHERE service_id = ? AND state != ?`, state, serviceID, state)
		return err
	})
}

func nowUnix() int64 {
	return time.Now().Unix()
}

func joinWords(words []string) string {
	return strings.Join(words, " ")
}

func containsString(list []string, s string) bool {
	return slices.Contains(list, s)
}
