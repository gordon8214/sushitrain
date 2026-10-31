// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

// How a request authenticated
type principalKind int

const (
	principalCredential principalKind = iota
	principalLease
	principalUpload
	principalPairingPoll
)

type principal struct {
	clientID string
	kind     principalKind
	// The lease, upload or pairing request a scoped token is for
	resource string
}

const lastSeenResolution = time.Minute

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// authenticate identifies the client of a request from its bearer token. Revoked credentials, and tokens of revoked
// credentials, are rejected.
func (s *Service) authenticate(r *http.Request) (principal, error) {
	token, ok := bearerToken(r)
	if !ok {
		return principal{}, errUnauthenticated
	}
	hash := secretHash(token)
	var p principal
	var err error
	switch {
	case strings.HasPrefix(token, prefixCredential):
		p.kind = principalCredential
		var lastSeen int64
		err = s.store.db.QueryRow(`SELECT client_id, last_seen FROM clients WHERE credential_hash = ? AND revoked_at IS NULL`,
			hash).Scan(&p.clientID, &lastSeen)
		if err == nil && s.store.now().Unix()-lastSeen > int64(lastSeenResolution/time.Second) {
			_, _ = s.store.db.Exec(`UPDATE clients SET last_seen = ? WHERE client_id = ?`, s.store.now().Unix(), p.clientID)
		}
	case strings.HasPrefix(token, prefixLeaseToken):
		p.kind = principalLease
		err = s.store.db.QueryRow(`SELECT l.client_id, l.lease_id FROM leases l JOIN clients c ON c.client_id = l.client_id
			WHERE l.token_hash = ? AND c.revoked_at IS NULL`, hash).Scan(&p.clientID, &p.resource)
	case strings.HasPrefix(token, prefixUploadTok):
		p.kind = principalUpload
		err = s.store.db.QueryRow(`SELECT u.client_id, u.upload_id FROM uploads u JOIN clients c ON c.client_id = u.client_id
			WHERE u.token_hash = ? AND c.revoked_at IS NULL`, hash).Scan(&p.clientID, &p.resource)
	case strings.HasPrefix(token, prefixPollToken):
		p.kind = principalPairingPoll
		err = s.store.db.QueryRow(`SELECT client_id, request_id FROM pairing_requests WHERE poll_hash = ?`, hash).Scan(
			&p.clientID, &p.resource)
	default:
		return principal{}, errUnauthenticated
	}
	if errors.Is(err, sql.ErrNoRows) {
		return principal{}, errUnauthenticated
	}
	return p, err
}

// requireCredential rejects scoped tokens where only the pairing credential may be used
func requireCredential(p principal) error {
	if p.kind != principalCredential {
		return errUnauthenticated
	}
	return nil
}

// folderAccess is a client's effective access to an exported folder
type folderAccess struct {
	info  FolderInfo
	read  bool
	write bool
}

// access returns a client's current permissions on a folder: those it was granted, limited by what the folder's
// export and configuration allow. Writable folders must be native, send-receive, and complete replicas or on demand.
func (s *Service) access(clientID, folderID string) (folderAccess, error) {
	var allowWrite, canWrite bool
	err := s.store.db.QueryRow(`SELECT e.allow_write, p.can_write FROM exports e
		JOIN permissions p ON p.folder_id = e.folder_id WHERE e.folder_id = ? AND p.client_id = ?`, folderID,
		clientID).Scan(&allowWrite, &canWrite)
	if errors.Is(err, sql.ErrNoRows) {
		return folderAccess{}, errForbidden
	}
	if err != nil {
		return folderAccess{}, err
	}
	info, ok := s.backend.Folder(folderID)
	if !ok || info.Paused || info.Type == config.FolderTypeReceiveEncrypted.String() || info.NativePath == "" {
		return folderAccess{}, errNotFound
	}
	access := folderAccess{info: info, read: true}
	access.write = allowWrite && canWrite && writableFolder(info) == nil
	return access, nil
}

// writableFolder reports why the service cannot write into a folder, if it can't
func writableFolder(info FolderInfo) error {
	switch {
	case info.Type != config.FolderTypeSendReceive.String():
		return errors.New("only send-receive folders can be written")
	case info.Selective && !info.OnDemand:
		return errors.New("folders this device holds only a selection of cannot be written")
	case info.NativePath == "":
		return errors.New("only folders on the native filesystem can be written")
	}
	return nil
}

func (a folderAccess) requireWrite() error {
	if !a.write {
		return newAPIError(http.StatusForbidden, codeFolderReadOnly, "this client may not change the folder")
	}
	return nil
}

// Exports

type Export struct {
	FolderID   string `json:"folderID"`
	AllowWrite bool   `json:"allowWrite"`
}

func (s *Service) Exports() ([]Export, error) {
	rows, err := s.store.db.Query(`SELECT folder_id, allow_write FROM exports ORDER BY folder_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Export
	for rows.Next() {
		var e Export
		if err := rows.Scan(&e.FolderID, &e.AllowWrite); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// SetExport makes a folder available to paired clients (each still needs permission). A writable export must be a
// folder the service can write into, and not one whose deletions need the system Trash where none is available.
func (s *Service) SetExport(folderID string, allowWrite bool) error {
	info, ok := s.backend.Folder(folderID)
	if !ok {
		return errors.New("no such folder")
	}
	if info.Type == config.FolderTypeReceiveEncrypted.String() || info.NativePath == "" {
		return errors.New("only unencrypted folders on the native filesystem can be exported")
	}
	if allowWrite {
		if err := writableFolder(info); err != nil {
			return err
		}
		if info.FilesystemType == "basic-trash" && !s.cfg.TrashAvailable {
			return errors.New("folders that move deletions to the system Trash cannot be written here; use Syncthing versioning instead")
		}
	}
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO exports (folder_id, allow_write) VALUES (?, ?)
			ON CONFLICT (folder_id) DO UPDATE SET allow_write = excluded.allow_write`, folderID, allowWrite)
		return err
	})
	if err == nil {
		// The reconciler brings the folder's change log up to date
		select {
		case s.exportsChanged <- struct{}{}:
		default:
		}
	}
	return err
}

func (s *Service) RemoveExport(folderID string) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM exports WHERE folder_id = ?`, folderID)
		return err
	})
}

func (s *Service) isExported(folderID string) bool {
	var one int
	return s.store.db.QueryRow(`SELECT 1 FROM exports WHERE folder_id = ?`, folderID).Scan(&one) == nil
}

func (s *Service) exportedFolderIDs() []string {
	exports, err := s.Exports()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(exports))
	for _, e := range exports {
		ids = append(ids, e.FolderID)
	}
	return ids
}

// Clients

type Client struct {
	ClientID    string            `json:"clientID"`
	Name        string            `json:"name"`
	Platform    string            `json:"platform"`
	Created     time.Time         `json:"created"`
	LastSeen    time.Time         `json:"lastSeen"`
	Revoked     bool              `json:"revoked"`
	Permissions map[string]string `json:"permissions"`
}

func (s *Service) Clients() ([]Client, error) {
	rows, err := s.store.db.Query(`SELECT client_id, name, platform, created_at, last_seen, revoked_at IS NOT NULL
		FROM clients ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var clients []Client
	for rows.Next() {
		var c Client
		var created, seen int64
		if err := rows.Scan(&c.ClientID, &c.Name, &c.Platform, &created, &seen, &c.Revoked); err != nil {
			rows.Close()
			return nil, err
		}
		c.Created = time.Unix(created, 0)
		if seen > 0 {
			c.LastSeen = time.Unix(seen, 0)
		}
		clients = append(clients, c)
	}
	rows.Close()
	for i := range clients {
		clients[i].Permissions = map[string]string{}
		perms, err := s.store.db.Query(`SELECT folder_id, can_write FROM permissions WHERE client_id = ?`,
			clients[i].ClientID)
		if err != nil {
			return nil, err
		}
		for perms.Next() {
			var folder string
			var write bool
			if err := perms.Scan(&folder, &write); err != nil {
				perms.Close()
				return nil, err
			}
			clients[i].Permissions[folder] = map[bool]string{true: "write", false: "read"}[write]
		}
		perms.Close()
	}
	return clients, nil
}

// SetPermission grants a client read (or read and write) access to a folder, or removes it with access ""
func (s *Service) SetPermission(clientID, folderID, access string) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM clients WHERE client_id = ?`, clientID).Scan(&one); err != nil {
			return errors.New("no such client")
		}
		switch access {
		case "":
			_, err := tx.Exec(`DELETE FROM permissions WHERE client_id = ? AND folder_id = ?`, clientID, folderID)
			return err
		case "read", "write":
			_, err := tx.Exec(`INSERT INTO permissions (client_id, folder_id, can_write) VALUES (?, ?, ?)
				ON CONFLICT (client_id, folder_id) DO UPDATE SET can_write = excluded.can_write`, clientID, folderID,
				access == "write")
			return err
		}
		return errors.New(`access must be "read", "write" or ""`)
	})
}

// RevokeClient invalidates a client's credential and every token issued with it. Its operation receipts are kept for
// deduplication, and its staged uploads are removed unless an operation still needs them.
func (s *Service) RevokeClient(clientID string) error {
	var uploads []string
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE clients SET revoked_at = ?, credential_hash = NULL WHERE client_id = ?`,
			s.store.now().Unix(), clientID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("no such client")
		}
		if _, err := tx.Exec(`DELETE FROM leases WHERE client_id = ?`, clientID); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT upload_id FROM uploads WHERE client_id = ? AND pinned_by IS NULL`, clientID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			uploads = append(uploads, id)
		}
		rows.Close()
		if _, err := tx.Exec(`DELETE FROM uploads WHERE client_id = ? AND pinned_by IS NULL`, clientID); err != nil {
			return err
		}
		// Pinned uploads stay for their operation, but their tokens no longer work
		_, err = tx.Exec(`UPDATE uploads SET token_hash = randomblob(32) WHERE client_id = ?`, clientID)
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range uploads {
		s.uploads.removeFile(id)
	}
	s.audit(clientID, "revoke", "", "", "ok")
	return nil
}

// audit records what a client did, by IDs only (no file names), in the service's audit log
func (s *Service) audit(clientID, action, operation, itemID, outcome string) {
	_, err := s.store.db.Exec(`INSERT INTO audit (at, client_id, action, operation, item_id, outcome) VALUES (?, ?, ?, ?, ?, ?)`,
		s.store.now().Unix(), clientID, action, operation, itemID, outcome)
	if err != nil {
		s.log.Warn("Could not write the audit log", "error", err)
	}
}

// pruneAudit removes audit entries older than the configured retention
func (s *Service) pruneAudit(ctx context.Context) error {
	if s.cfg.AuditRetention <= 0 {
		return nil
	}
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM audit WHERE at < ?`, s.store.now().Add(-s.cfg.AuditRetention).Unix())
		return err
	})
}
