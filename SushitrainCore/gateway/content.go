// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"golang.org/x/sys/unix"
)

// A lease binds a client to one content identity of an item. Its content is served from a representation: a spool
// file holding the bytes of exactly that block list, filled with verified blocks from the local file (cloned where
// the filesystem can) or from peers. Representations are shared by content identity, so concurrent and repeated
// reads of the same version prepare it once.

const (
	leaseLifetime      = 24 * time.Hour
	leaseRenewal       = time.Minute
	maxManifestPage    = 1024
	preparationWorkers = 4
	// Blocks prepared at once across all representations, and representations kept (each holds a file open)
	maxPreparing        = 16
	maxRepresentations  = 512
	unpinnedRetention   = 7 * 24 * time.Hour
	blockWaitMinimum    = 60 * time.Second
	blockMinimumRate    = 32 * 1024
	contentCopyBuffer   = 1 << 20
	metaTokenKey        = "token_key"
	blockStatePending   = 0
	blockStateFetching  = 1
	blockStateReady     = 2
	retryAfterPreparing = 1
)

var errContentUnavailable = newAPIError(http.StatusServiceUnavailable, codeContentUnavailable,
	"no device can provide this content right now").withRetryAfter(30)

type contentManager struct {
	s   *Service
	dir string

	mutex    sync.Mutex
	reps     map[string]*representation
	reserved int64
	// Limits preparation work across representations
	preparing chan struct{}
}

func newContentManager(s *Service, dir string) (*contentManager, error) {
	// Representations are rebuilt on demand after a restart
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &contentManager{s: s, dir: dir, reps: make(map[string]*representation),
		preparing: make(chan struct{}, maxPreparing)}, nil
}

type representation struct {
	m         *contentManager
	id        string
	folder    string
	file      protocol.FileInfo
	localPath string
	spoolPath string

	mutex      sync.Mutex
	changed    chan struct{}
	state      []uint8
	priority   []int
	readyBytes int64
	ready      int
	failure    error
	lastUsed   time.Time
	spool      *os.File
	cloned     bool
	// Whether to read blocks from the local file; turned off when its bytes don't match
	useLocal bool
	workers  int
	scanned  bool
}

// tokenFor derives the scoped token of a lease or upload, so that repeating a request returns the same token without
// the service storing it
func (m *contentManager) tokenFor(prefix, resource string) (string, error) {
	return deriveToken(m.s.store, prefix, resource)
}

func deriveToken(st *store, prefix, resource string) (string, error) {
	key, err := metaValue(st.db, metaTokenKey)
	if errors.Is(err, sql.ErrNoRows) {
		err = st.tx(context.Background(), func(tx *sql.Tx) error {
			if key, err = metaValue(tx, metaTokenKey); err == nil {
				return nil
			}
			key = b64.EncodeToString(randomBytes(32))
			return setMeta(tx, metaTokenKey, key)
		})
	}
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(prefix + resource))
	return prefix + b64.EncodeToString(mac.Sum(nil)), nil
}

// get returns the representation of a file's content, starting its preparation if needed
func (m *contentManager) get(folder FolderInfo, file protocol.FileInfo) (*representation, error) {
	id := contentIDOf(file)
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if rep, ok := m.reps[id]; ok {
		rep.touch()
		return rep, nil
	}
	// Every path to content comes here; preparation relies on Syncthing's block layout
	if err := checkBlockLayout(file); err != nil {
		m.s.log.Warn("Index entry has an unexpected block layout", "folder", folder.ID, "error", err)
		return nil, errContentUnavailable
	}
	if err := m.reserveLocked(file.Size); err != nil {
		return nil, err
	}
	rep := &representation{
		m:         m,
		id:        id,
		folder:    folder.ID,
		file:      file,
		spoolPath: filepath.Join(m.dir, b64.EncodeToString(blocksHashOf(file))[:32]+"-"+strconv.FormatInt(file.Size, 10)),
		changed:   make(chan struct{}),
		state:     make([]uint8, len(file.Blocks)),
		lastUsed:  time.Now(),
	}
	if folder.NativePath != "" {
		rep.localPath = filepath.Join(folder.NativePath, osutil.NativeFilename(file.Name))
	}
	if err := rep.open(); err != nil {
		m.reserved -= file.Size
		return nil, err
	}
	m.reps[id] = rep
	rep.start()
	return rep, nil
}

// reserveLocked accounts for a new representation, evicting unpinned ones to make room
func (m *contentManager) reserveLocked(size int64) error {
	quota := m.s.cfg.SpoolQuota
	if size > quota {
		return newAPIError(http.StatusInsufficientStorage, codeInsufficientStorage, "the file is larger than the content cache")
	}
	if m.reserved+size > quota || len(m.reps) >= maxRepresentations {
		pinned := m.pinnedContentIDs()
		candidates := make([]*representation, 0, len(m.reps))
		for _, rep := range m.reps {
			if _, ok := pinned[rep.id]; !ok {
				candidates = append(candidates, rep)
			}
		}
		slices.SortFunc(candidates, func(a, b *representation) int { return a.usedAt().Compare(b.usedAt()) })
		for _, rep := range candidates {
			if m.reserved+size <= quota && len(m.reps) < maxRepresentations {
				break
			}
			m.removeLocked(rep)
		}
	}
	if m.reserved+size > quota || len(m.reps) >= maxRepresentations {
		return newAPIError(http.StatusInsufficientStorage, codeInsufficientStorage,
			"the content cache is full of files being read")
	}
	m.reserved += size
	return nil
}

func (m *contentManager) pinnedContentIDs() map[string]struct{} {
	pinned := make(map[string]struct{})
	rows, err := m.s.store.db.Query(`SELECT DISTINCT content_id FROM leases WHERE expires_at > ?`, m.s.store.now().Unix())
	if err != nil {
		// Treat everything as pinned rather than evict content in use
		for id := range m.reps {
			pinned[id] = struct{}{}
		}
		return pinned
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			pinned[id] = struct{}{}
		}
	}
	return pinned
}

func (m *contentManager) removeLocked(rep *representation) {
	delete(m.reps, rep.id)
	m.reserved -= rep.file.Size
	rep.close()
}

// expire removes expired leases and representations nobody used for a long time
func (m *contentManager) expire(ctx context.Context) {
	err := m.s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM leases WHERE expires_at <= ?`, m.s.store.now().Unix())
		return err
	})
	if err != nil {
		m.s.log.Warn("Could not expire leases", "error", err)
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	pinned := m.pinnedContentIDs()
	for _, rep := range m.reps {
		if _, ok := pinned[rep.id]; !ok && time.Since(rep.usedAt()) > unpinnedRetention {
			m.removeLocked(rep)
		}
	}
}

func (rep *representation) touch() {
	rep.mutex.Lock()
	rep.lastUsed = time.Now()
	rep.mutex.Unlock()
}

func (rep *representation) usedAt() time.Time {
	rep.mutex.Lock()
	defer rep.mutex.Unlock()
	return rep.lastUsed
}

func (rep *representation) open() error {
	_ = os.Remove(rep.spoolPath)
	// Try a clone of the local file first: it takes no time or space, and its blocks are then verified in place
	if rep.localPath != "" && rep.localMatches() {
		if err := cloneFile(rep.localPath, rep.spoolPath); err == nil {
			f, err := os.OpenFile(rep.spoolPath, os.O_RDWR, 0)
			if err == nil {
				if stat, err := f.Stat(); err == nil && stat.Size() == rep.file.Size {
					rep.spool, rep.cloned = f, true
					return nil
				}
				f.Close()
			}
			_ = os.Remove(rep.spoolPath)
		}
	}
	f, err := os.OpenFile(rep.spoolPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(rep.file.Size); err != nil {
		f.Close()
		_ = os.Remove(rep.spoolPath)
		return err
	}
	rep.spool = f
	rep.useLocal = rep.localPath != "" && rep.localMatches()
	return nil
}

// localMatches reports whether the local file looks like the version being prepared
func (rep *representation) localMatches() bool {
	local, ok, err := rep.m.s.backend.LocalFile(rep.folder, rep.file.Name)
	if err != nil || !ok || local.Deleted || !local.Version.Equal(rep.file.Version) {
		return false
	}
	stat, err := os.Lstat(rep.localPath)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != rep.file.Size {
		return false
	}
	info, _ := rep.m.s.backend.Folder(rep.folder)
	return stat.ModTime().Sub(rep.file.ModTime()).Abs() <= info.ModTimeWindow
}

func (rep *representation) close() {
	rep.mutex.Lock()
	defer rep.mutex.Unlock()
	if rep.failure == nil {
		rep.failure = errors.New("representation removed")
	}
	if rep.spool != nil {
		rep.spool.Close()
		rep.spool = nil
	}
	_ = os.Remove(rep.spoolPath)
	rep.broadcastLocked()
}

func (rep *representation) broadcastLocked() {
	close(rep.changed)
	rep.changed = make(chan struct{})
}

func (rep *representation) start() {
	rep.mutex.Lock()
	defer rep.mutex.Unlock()
	if rep.ready == len(rep.state) {
		return
	}
	for rep.workers < preparationWorkers {
		rep.workers++
		go rep.work()
	}
}

// next picks the block a worker should prepare: one a reader waits for, otherwise the first one not started
func (rep *representation) next() (int, bool) {
	rep.mutex.Lock()
	defer rep.mutex.Unlock()
	if rep.failure != nil {
		rep.workers--
		return 0, false
	}
	for len(rep.priority) > 0 {
		index := rep.priority[0]
		rep.priority = rep.priority[1:]
		if rep.state[index] == blockStatePending {
			rep.state[index] = blockStateFetching
			return index, true
		}
	}
	for index, state := range rep.state {
		if state == blockStatePending {
			rep.state[index] = blockStateFetching
			return index, true
		}
	}
	rep.workers--
	return 0, false
}

func (rep *representation) work() {
	ctx := rep.m.s.ctx
	for {
		index, ok := rep.next()
		if !ok {
			return
		}
		var err error
		select {
		case rep.m.preparing <- struct{}{}:
			err = rep.prepareBlock(ctx, index)
			<-rep.m.preparing
		case <-ctx.Done():
			err = ctx.Err()
		}
		rep.mutex.Lock()
		if err != nil {
			// Let readers retry: back to pending, and the reader that waits gets an error. This worker stops; a
			// reader starts workers again.
			rep.state[index] = blockStatePending
			if rep.failure == nil {
				rep.failure = err
			}
			rep.workers--
		} else {
			rep.state[index] = blockStateReady
			rep.ready++
			for rep.readyBytes < rep.file.Size {
				i := int(rep.readyBytes / int64(rep.file.BlockSize()))
				if i >= len(rep.state) || rep.state[i] != blockStateReady {
					break
				}
				rep.readyBytes = min(rep.file.Size, rep.file.Blocks[i].Offset+int64(rep.file.Blocks[i].Size))
			}
		}
		rep.broadcastLocked()
		rep.mutex.Unlock()
		if err != nil {
			return
		}
	}
}

// prepareBlock puts a verified copy of a block in the spool file
func (rep *representation) prepareBlock(ctx context.Context, index int) error {
	block := rep.file.Blocks[index]
	rep.mutex.Lock()
	spool, cloned, useLocal := rep.spool, rep.cloned, rep.useLocal
	rep.mutex.Unlock()
	if spool == nil {
		return errors.New("representation removed")
	}
	if cloned {
		buf := make([]byte, block.Size)
		if _, err := spool.ReadAt(buf, block.Offset); err == nil && verifyBlock(buf, block) == nil {
			return nil
		}
		rep.localFailed()
	} else if useLocal {
		// Not following symbolic links, and not blocking on a FIFO put in the file's place
		if f, err := os.OpenFile(rep.localPath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0); err == nil {
			buf := make([]byte, block.Size)
			_, err := f.ReadAt(buf, block.Offset)
			f.Close()
			if err == nil && verifyBlock(buf, block) == nil {
				_, err := spool.WriteAt(buf, block.Offset)
				return err
			}
		}
		rep.localFailed()
	}
	timeout := max(blockWaitMinimum, time.Duration(block.Size)*time.Second/blockMinimumRate)
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := rep.m.s.backend.FetchBlock(fetchCtx, rep.folder, rep.file, index)
	if err != nil {
		return errContentUnavailable
	}
	_, err = spool.WriteAt(data, block.Offset)
	return err
}

// localFailed handles local bytes that don't match the index: the file changed or is damaged, so it is scanned, and
// the block comes from a peer instead
func (rep *representation) localFailed() {
	rep.mutex.Lock()
	first := !rep.scanned
	rep.scanned = true
	rep.useLocal = false
	rep.mutex.Unlock()
	if first {
		rep.m.s.log.Info("Local content did not match its index entry; scanning it", "folder", rep.folder)
		rep.m.s.backend.ScanPaths(rep.folder, []string{rep.file.Name})
	}
}

// waitFor waits until the block is ready, raising its priority, and returns an error when it cannot be prepared
func (rep *representation) waitFor(ctx context.Context, index int) error {
	deadline := time.Now().Add(max(blockWaitMinimum,
		time.Duration(rep.file.Blocks[index].Size)*time.Second/blockMinimumRate))
	retried := false
	for {
		rep.mutex.Lock()
		state := rep.state[index]
		failure := rep.failure
		if state == blockStateReady {
			rep.mutex.Unlock()
			return nil
		}
		if failure != nil {
			if retried {
				rep.mutex.Unlock()
				return failure
			}
			// Give preparation one more chance for this reader, as a peer may have connected since
			retried = true
			rep.failure = nil
		}
		if state == blockStatePending && !slices.Contains(rep.priority, index) {
			rep.priority = append([]int{index}, rep.priority...)
		}
		changed := rep.changed
		rep.mutex.Unlock()
		rep.start()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(deadline)):
			return errContentUnavailable
		}
		if time.Now().After(deadline) {
			return errContentUnavailable
		}
	}
}

func (rep *representation) preparation() Preparation {
	rep.mutex.Lock()
	defer rep.mutex.Unlock()
	switch {
	case rep.ready == len(rep.state):
		return Preparation{State: preparationReady, ReadyBytes: rep.file.Size}
	case rep.failure != nil && rep.workers == 0:
		return Preparation{State: preparationFailed, ReadyBytes: rep.readyBytes, RetryAfter: 30}
	}
	return Preparation{State: preparationPreparing, ReadyBytes: rep.readyBytes, RetryAfter: retryAfterPreparing}
}

// Handlers

func (s *Service) handleCreateLease(w http.ResponseWriter, r *http.Request, p principal) error {
	if err := requireCredential(p); err != nil {
		return err
	}
	var req ReadLeaseRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	row, access, err := s.itemForClient(p.clientID, r.PathValue("itemID"))
	if err != nil {
		return err
	}
	if row.deleted || row.kind != protocol.FileInfoTypeFile {
		return errNotFound
	}
	file, ok, err := s.backend.GlobalFile(row.folder, row.path)
	if err != nil {
		return err
	}
	if !ok || file.Deleted || file.IsInvalid() {
		return errNotFound
	}
	current := revisionOf(file)
	if current != req.Revision || row.revision != req.Revision {
		item := row.toItem(access.write)
		if current != row.revision {
			// The change log hasn't caught up yet; describe the item as the index has it
			fresh := row
			fresh.setFromFileInfo(file, access.info.IgnorePerms)
			item = fresh.toItem(access.write)
		}
		return newAPIError(http.StatusPreconditionFailed, codeRevisionMismatch, "the item changed").withItem(&item)
	}
	if file.Size > 0 {
		if err := checkBlockLayout(file); err != nil {
			s.log.Warn("Index entry has an unexpected block layout", "folder", row.folder, "error", err)
			return errContentUnavailable
		}
	}

	contentID := contentIDOf(file)
	now := s.store.now()
	leaseID := newID(prefixLease)
	status := http.StatusCreated
	expires := now.Add(leaseLifetime)
	newToken, err := s.content.tokenFor(prefixLeaseToken, leaseID)
	if err != nil {
		return err
	}
	err = s.store.tx(r.Context(), func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRow(`SELECT lease_id FROM leases WHERE client_id = ? AND item_id = ? AND revision = ? AND expires_at > ?`,
			p.clientID, row.id, req.Revision, now.Unix()).Scan(&existing)
		if err == nil {
			leaseID, status = existing, http.StatusOK
			_, err = tx.Exec(`UPDATE leases SET expires_at = ? WHERE lease_id = ?`, expires.Unix(), leaseID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		token := newToken
		if _, err := tx.Exec(`DELETE FROM leases WHERE client_id = ? AND item_id = ? AND revision = ?`, p.clientID, row.id,
			req.Revision); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO leases (lease_id, token_hash, client_id, folder_id, item_id, revision, content_id, path,
			created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, leaseID, secretHash(token), p.clientID,
			row.folder, row.id, req.Revision, contentID, row.path, now.Unix(), expires.Unix())
		return err
	})
	if err != nil {
		return err
	}
	token, err := s.content.tokenFor(prefixLeaseToken, leaseID)
	if err != nil {
		return err
	}
	lease := ReadLease{
		Lease:      leaseID,
		Token:      token,
		ItemID:     row.id,
		Revision:   req.Revision,
		ContentID:  contentID,
		Size:       file.Size,
		BlockSize:  file.BlockSize(),
		BlockCount: len(file.Blocks),
		Expires:    expires.UTC().Format(time.RFC3339),
	}
	if file.Size == 0 {
		lease.Preparation = Preparation{State: preparationReady}
	} else {
		rep, err := s.content.get(access.info, file)
		if err != nil {
			return err
		}
		lease.Preparation = rep.preparation()
	}
	if status == http.StatusCreated {
		s.audit(p.clientID, "lease", "", row.id, "ok")
	}
	return writeJSON(w, status, lease)
}

type leaseRow struct {
	id, clientID, folder, itemID, revision, contentID, path string
	expires                                                 int64
}

// leaseForRequest looks up the lease of a content request, checking that the client may still read it
func (s *Service) leaseForRequest(r *http.Request, p principal) (leaseRow, folderAccess, error) {
	leaseID := r.PathValue("leaseID")
	if p.kind == principalLease && p.resource != leaseID || p.kind != principalLease && p.kind != principalCredential {
		return leaseRow{}, folderAccess{}, errUnauthenticated
	}
	var l leaseRow
	err := s.store.db.QueryRow(`SELECT lease_id, client_id, folder_id, item_id, revision, content_id, path, expires_at
		FROM leases WHERE lease_id = ?`, leaseID).Scan(&l.id, &l.clientID, &l.folder, &l.itemID, &l.revision, &l.contentID,
		&l.path, &l.expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && l.clientID != p.clientID {
		return leaseRow{}, folderAccess{}, newAPIError(http.StatusGone, codeLeaseExpired, "unknown or expired lease")
	}
	if err != nil {
		return leaseRow{}, folderAccess{}, err
	}
	now := s.store.now()
	if l.expires <= now.Unix() {
		return leaseRow{}, folderAccess{}, newAPIError(http.StatusGone, codeLeaseExpired, "the lease has expired")
	}
	access, err := s.access(p.clientID, l.folder)
	if err != nil {
		return leaseRow{}, folderAccess{}, err
	}
	// Renew on use
	if renewed := now.Add(leaseLifetime).Unix(); renewed-l.expires > int64(leaseRenewal/time.Second) {
		_, _ = s.store.db.Exec(`UPDATE leases SET expires_at = ? WHERE lease_id = ?`, renewed, l.id)
	}
	return l, access, nil
}

// representationFor returns the representation of a lease's content. After a restart, it is prepared again if the
// index still has the leased content.
func (s *Service) representationFor(l leaseRow, access folderAccess) (*representation, protocol.FileInfo, error) {
	s.content.mutex.Lock()
	rep, ok := s.content.reps[l.contentID]
	s.content.mutex.Unlock()
	if ok {
		rep.touch()
		return rep, rep.file, nil
	}
	file, ok, err := s.backend.GlobalFile(l.folder, l.path)
	if err != nil {
		return nil, protocol.FileInfo{}, err
	}
	if !ok || contentIDOf(file) != l.contentID {
		return nil, protocol.FileInfo{}, newAPIError(http.StatusGone, codeLeaseExpired,
			"the leased content is no longer available")
	}
	if file.Size == 0 {
		return nil, file, nil
	}
	rep, err = s.content.get(access.info, file)
	return rep, file, err
}

func (s *Service) handleManifest(w http.ResponseWriter, r *http.Request, p principal) error {
	l, access, err := s.leaseForRequest(r, p)
	if err != nil {
		return err
	}
	_, file, err := s.representationFor(l, access)
	if err != nil {
		return err
	}
	from, limit := 0, maxManifestPage
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = strconv.Atoi(v); err != nil || from < 0 {
			return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid from")
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return newAPIError(http.StatusBadRequest, codeInvalidRequest, "invalid limit")
		}
		limit = min(limit, maxManifestPage)
	}
	page := ManifestPage{ContentID: l.contentID, BlockSize: file.BlockSize(), Blocks: []ManifestBlock{}}
	from = min(from, len(file.Blocks))
	end := min(len(file.Blocks), from+limit)
	for i := from; i < end; i++ {
		b := file.Blocks[i]
		page.Blocks = append(page.Blocks, ManifestBlock{Offset: b.Offset, Size: b.Size, Hash: b64.EncodeToString(b.Hash)})
	}
	if end < len(file.Blocks) {
		page.Next = &end
	}
	return writeJSON(w, http.StatusOK, page)
}

func (s *Service) handleContent(w http.ResponseWriter, r *http.Request, p principal) error {
	l, access, err := s.leaseForRequest(r, p)
	if err != nil {
		return err
	}
	rep, file, err := s.representationFor(l, access)
	if err != nil {
		return err
	}
	start, end, status, err := contentRange(w, r, `"`+l.contentID+`"`, file.Size)
	if err != nil {
		return err
	}
	if r.Method == http.MethodHead || end == start {
		w.WriteHeader(status)
		return nil
	}

	// Wait for the first block before committing to a status, so that an unavailable file gets a proper error
	blockSize := int64(file.BlockSize())
	first := int(start / blockSize)
	if err := rep.waitFor(r.Context(), first); err != nil {
		return err
	}
	w.WriteHeader(status)
	buf := make([]byte, min(contentCopyBuffer, end-start))
	for offset := start; offset < end; {
		index := int(offset / blockSize)
		if err := rep.waitFor(r.Context(), index); err != nil {
			// The status is sent: end the transfer early, so the client keeps only what it can verify
			panic(http.ErrAbortHandler)
		}
		block := file.Blocks[index]
		blockEnd := min(end, block.Offset+int64(block.Size))
		for offset < blockEnd {
			n := min(int64(len(buf)), blockEnd-offset)
			rep.mutex.Lock()
			spool := rep.spool
			rep.mutex.Unlock()
			if spool == nil {
				panic(http.ErrAbortHandler)
			}
			read, err := spool.ReadAt(buf[:n], offset)
			if err != nil && !(errors.Is(err, io.EOF) && int64(read) == n) {
				panic(http.ErrAbortHandler)
			}
			if _, err := w.Write(buf[:read]); err != nil {
				return nil
			}
			offset += int64(read)
		}
	}
	return nil
}

// contentRange sets the headers of a response with content of the given size and strong validator, and returns the
// byte range [start, end) it sends (the requested range, or all of it) and the status
func contentRange(w http.ResponseWriter, r *http.Request, etag string, size int64) (int64, int64, int, error) {
	header := w.Header()
	header.Set("ETag", etag)
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Type", "application/octet-stream")

	start, end := int64(0), size
	status := http.StatusOK
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && size > 0 {
		ifRange := r.Header.Get("If-Range")
		if ifRange == "" || ifRange == etag {
			s, e, ok := parseRange(rangeHeader, size)
			if !ok {
				header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
				return 0, 0, 0, newAPIError(http.StatusRequestedRangeNotSatisfiable, codeInvalidRequest,
					"unsatisfiable range")
			}
			start, end, status = s, e, http.StatusPartialContent
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
		}
	}
	header.Set("Content-Length", strconv.FormatInt(end-start, 10))
	return start, end, status, nil
}

// parseRange parses a single byte range ("bytes=a-b", "bytes=a-" or "bytes=-n") and returns [start, end)
func parseRange(header string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size
	if last != "" {
		e, err := strconv.ParseInt(last, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		// e+1 would overflow for the largest end
		if e < size-1 {
			end = e + 1
		}
	}
	return start, end, true
}

// checkBlockLayout checks that a file's blocks are laid out as Syncthing lays them out: each as large as the block
// size and starting where the previous one ended, except the last, which ends at the end of the file
func checkBlockLayout(info protocol.FileInfo) error {
	blockSize := int64(info.BlockSize())
	if blockSize <= 0 || blockSize > protocol.MaxBlockSize {
		return fmt.Errorf("invalid block size %d", blockSize)
	}
	if info.Size <= 0 {
		return fmt.Errorf("invalid size %d", info.Size)
	}
	expected := (info.Size + blockSize - 1) / blockSize
	if int64(len(info.Blocks)) != expected {
		return fmt.Errorf("file of size %d has %d blocks, expected %d", info.Size, len(info.Blocks), expected)
	}
	for index, block := range info.Blocks {
		offset := int64(index) * blockSize
		if block.Offset != offset || int64(block.Size) != min(blockSize, info.Size-offset) {
			return fmt.Errorf("block %d at offset %d with size %d is out of place", index, block.Offset, block.Size)
		}
	}
	return nil
}
