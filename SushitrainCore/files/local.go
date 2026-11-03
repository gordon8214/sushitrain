// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Changes made here, in items.local, until they are settled into operations
const (
	// The item isn't on the gateway (yet)
	localCreated = 1
	// Its content was changed here
	localContent = 2
	// It was renamed or moved here
	localMoved = 4
	// It was removed here: the row is a tombstone of a gateway item
	localDeleted = 8
	// It is never sent: an editor's lock or swap file, or a name the gateway ignores
	localOnly = 16
	// A removed item whose removal its directory's carries (rm -r): it goes with the directory
	localFolded = 32
)

var (
	ErrReadOnly    = errors.New("the folder can't be changed from this machine")
	ErrNotEmpty    = errors.New("the directory is not empty")
	ErrCrossFolder = errors.New("items move between folders as copies")
	ErrInvalidName = errors.New("the name can't be used in a folder of a gateway")
	ErrIsDir       = errors.New("is a directory")
	ErrNotDir      = errors.New("not a directory")
)

// localOnlyName reports whether a file of this name is kept on this machine and never sent: files that live as long
// as an editing session, which other devices have no use for. Short-lived names (temporary files of a save) need no
// list: they are gone before changes are settled.
func localOnlyName(name string) bool {
	switch {
	case strings.HasPrefix(name, ".~lock.") && strings.HasSuffix(name, "#"):
		// LibreOffice's lock file, while a document is open
		return true
	case strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#") && len(name) > 2:
		// Emacs's auto-save file
		return true
	case strings.HasPrefix(name, ".") && len(name) > 5:
		// vim's swap files: .name.swp, .name.swo, … (.sw[a-p])
		ext := name[len(name)-4:]
		return ext[:3] == ".sw" && ext[3] >= 'a' && ext[3] <= 'p'
	}
	return false
}

// conflictName is the name of a conflict copy, as the Kit and Syncthing name them, with the start of this client's ID
// where Syncthing puts a device's
func conflictName(name, clientID string, now time.Time) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	short := strings.ToUpper(strings.ReplaceAll(clientID, "-", ""))
	if len(short) > 7 {
		short = short[:7]
	}
	return fmt.Sprintf("%s.sync-conflict-%s-%s%s", stem, now.Format("20060102-150405"), short, ext)
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}

func randomName() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// localRow is an item with what local changes need of it
type localRow struct {
	id       int64
	folder   int64
	parent   sql.NullInt64
	name     string
	kind     int
	node     int64
	local    int
	deleted  bool
	gateway  sql.NullString
	writable bool
}

func loadLocalRow(q queryer, query string, args ...any) (*localRow, error) {
	var r localRow
	err := q.QueryRow(`SELECT i.id, i.folder, i.parent, i.name, i.kind, i.node, i.local, i.deleted,
		(SELECT r.gateway_id FROM remote_items AS r WHERE r.item = i.id), f.can_write
		FROM items AS i JOIN folders AS f ON f.id = i.folder WHERE `+query, args...).Scan(&r.id, &r.folder, &r.parent,
		&r.name, &r.kind, &r.node, &r.local, &r.deleted, &r.gateway, &r.writable)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func liveChild(tx *sql.Tx, parent int64, name string) (*localRow, error) {
	return loadLocalRow(tx, `i.parent = ? AND i.name = ? AND i.deleted = 0`, parent, name)
}

func liveItem(tx *sql.Tx, id int64) (*localRow, error) {
	return loadLocalRow(tx, `i.id = ? AND i.deleted = 0`, id)
}

// directory returns a live directory (or a folder's root) to change something in
func directory(tx *sql.Tx, id int64) (*localRow, error) {
	if id == 0 {
		// The mount's root holds folders, which are added from gateways
		return nil, ErrReadOnly
	}
	d, err := liveItem(tx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fs.ErrNotExist
	}
	if d.kind != kindDir && d.kind != kindRoot {
		return nil, ErrNotDir
	}
	if !d.writable {
		return nil, ErrReadOnly
	}
	return d, nil
}

// unresolved reports whether an item has operations that aren't resolved: waiting to be sent, or for the gateway, or
// kept for review
const unresolvedStates = `('queued', 'uploaded', 'submitted', 'pending', 'failed', 'unknown', 'blocked')`

func hasUnresolved(q queryer, item int64) (bool, error) {
	var exists bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM outbox WHERE item = ? AND state IN `+unresolvedStates+`)`, item).
		Scan(&exists)
	return exists, err
}

// captureBase records what the gateway has of an item as the base of the changes made to it here, when it is first
// changed: an item with changes or operations pending already has its base (its operations chain).
func captureBase(tx *sql.Tx, item int64) error {
	var local int
	if err := tx.QueryRow(`SELECT local FROM items WHERE id = ?`, item).Scan(&local); err != nil {
		return err
	}
	pending, err := hasUnresolved(tx, item)
	if err != nil || local&^localOnly != 0 || pending {
		return err
	}
	_, err = tx.Exec(`UPDATE items SET (base_revision, base_content_id, base_subtree) =
		(SELECT revision, content_id, subtree_generation FROM remote_items WHERE item = items.id) WHERE id = ?`, item)
	return err
}

// Create creates a file or a directory here; it is sent to the gateway once changes settle
func (s *Service) Create(parent int64, name string, dir bool) (Entry, error) {
	name = normalizedName(name)
	if !validName(name) || reservedName(name) {
		return Entry{}, ErrInvalidName
	}
	var id int64
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		d, err := directory(tx, parent)
		if err != nil {
			return err
		}
		if existing, err := liveChild(tx, parent, name); err != nil {
			return err
		} else if existing != nil {
			return fs.ErrExist
		}
		node, err := allocate(tx, metaNextNode)
		if err != nil {
			return err
		}
		kind, local := kindFile, localCreated
		if dir {
			kind = kindDir
		} else if localOnlyName(name) {
			local |= localOnly
		}
		res, err := tx.Exec(`INSERT INTO items (folder, parent, name, kind, mod_ns, node, local, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.folder, parent, name, kind, time.Now().UnixNano(), node, local,
			nowMillis())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e, _, err := s.Item(id)
	if err == nil {
		s.settleLater(e.Folder)
	}
	return e, err
}

// Remove removes a file, or an empty directory, and returns its entry as it was
func (s *Service) Remove(parent int64, name string, dir bool) (Entry, error) {
	name = normalizedName(name)
	var removed Entry
	var folder int64
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := directory(tx, parent); err != nil {
			return err
		}
		r, err := liveChild(tx, parent, name)
		if err != nil {
			return err
		}
		if r == nil {
			return fs.ErrNotExist
		}
		isDir := r.kind == kindDir
		switch {
		case dir && !isDir:
			return ErrNotDir
		case !dir && isDir:
			return ErrIsDir
		}
		if isDir {
			var children bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM items WHERE parent = ? AND deleted = 0)`, r.id).
				Scan(&children); err != nil {
				return err
			}
			if children {
				return ErrNotEmpty
			}
		}
		removed = Entry{Item: r.id, Node: uint64(r.node), Folder: r.folder, Name: r.name, Kind: r.kind}
		folder = r.folder
		return s.removeLocked(tx, r)
	})
	if err != nil {
		return Entry{}, err
	}
	s.detachNode(removed.Node)
	s.settleLater(folder)
	return removed, nil
}

// removeLocked removes an item here: a gateway item (or one whose creation was sent) becomes a tombstone to settle into
// a delete, and one the gateway never heard of is simply gone
func (s *Service) removeLocked(tx *sql.Tx, r *localRow) error {
	if _, err := tx.Exec(`UPDATE work SET detached = 1 WHERE node = ?`, r.node); err != nil {
		return err
	}
	sent, err := s.creationSent(tx, r.id)
	if err != nil {
		return err
	}
	if !r.gateway.Valid && !sent {
		// Nothing of it reached the gateway: drop its unsent creation and edits
		if err := s.cancelQueued(tx, r.id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM items WHERE id = ?`, r.id)
		return err
	}
	if err := captureBase(tx, r.id); err != nil {
		return err
	}
	if r.kind == kindDir {
		// What the gateway had below it when it was removed here, which its delete must still find
		if _, err := tx.Exec(`UPDATE items SET base_subtree = (SELECT subtree_generation FROM remote_items
			WHERE item = items.id) WHERE id = ? AND local & ? = 0`, r.id, localDeleted); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE items SET deleted = 1, deleted_at = ?, local = (local | ?) & ~?, changed_at = ?,
		local_content = NULL WHERE id = ?`, nowUnix(), localDeleted, localContent, nowMillis(), r.id)
	return err
}

// creationSent reports whether the item's creation was sent to the gateway, whose result maps it to a gateway item
func (s *Service) creationSent(q queryer, item int64) (bool, error) {
	var sent bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM outbox WHERE item = ? AND kind = 'create'
		AND state IN ('submitted', 'pending', 'unknown'))`, item).Scan(&sent)
	return sent, err
}

// Rename renames or moves an item within its folder, replacing what has the new name unless noReplace
func (s *Service) Rename(parent int64, name string, newParent int64, newName string, noReplace bool) (*Entry, error) {
	name, newName = normalizedName(name), normalizedName(newName)
	if !validName(newName) || reservedName(newName) {
		return nil, ErrInvalidName
	}
	var replaced *Entry
	var folder int64
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := directory(tx, parent); err != nil {
			return err
		}
		to, err := directory(tx, newParent)
		if err != nil {
			return err
		}
		src, err := liveChild(tx, parent, name)
		if err != nil {
			return err
		}
		if src == nil {
			return fs.ErrNotExist
		}
		if to.folder != src.folder {
			return ErrCrossFolder
		}
		folder = src.folder
		if parent == newParent && name == newName {
			return nil
		}
		if src.kind == kindDir {
			// Not into itself
			var inside bool
			if err := tx.QueryRow(`WITH RECURSIVE up(id, parent) AS (SELECT id, parent FROM items WHERE id = ?
				UNION ALL SELECT i.id, i.parent FROM items AS i JOIN up ON i.id = up.parent)
				SELECT EXISTS (SELECT 1 FROM up WHERE id = ?)`, newParent, src.id).Scan(&inside); err != nil {
				return err
			}
			if inside {
				return fs.ErrInvalid
			}
		}
		target, err := liveChild(tx, newParent, newName)
		if err != nil {
			return err
		}
		if target != nil {
			switch {
			case noReplace:
				return fs.ErrExist
			case src.kind == kindDir && target.kind != kindDir:
				return ErrNotDir
			case src.kind != kindDir && target.kind == kindDir:
				return ErrIsDir
			}
			if target.kind == kindDir {
				var children bool
				if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM items WHERE parent = ? AND deleted = 0)`,
					target.id).Scan(&children); err != nil {
					return err
				}
				if children {
					return ErrNotEmpty
				}
			}
			replaced = &Entry{Item: target.id, Node: uint64(target.node), Folder: target.folder, Name: target.name,
				Kind: target.kind}
			if err := s.removeLocked(tx, target); err != nil {
				return err
			}
		}
		if err := captureBase(tx, src.id); err != nil {
			return err
		}
		local := src.local
		switch {
		case src.local&localCreated != 0 && src.kind == kindFile:
			// A file only here: whether it is sent depends on its name
			local &^= localOnly
			if localOnlyName(newName) {
				local |= localOnly
			}
		case src.local&localCreated == 0:
			local |= localMoved
		}
		_, err = tx.Exec(`UPDATE items SET parent = ?, name = ?, local = ?, changed_at = ? WHERE id = ?`, newParent,
			newName, local, nowMillis(), src.id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replaced != nil {
		s.detachNode(replaced.Node)
	}
	s.settleLater(folder)
	return replaced, nil
}

// SetModTime sets the modification time of a file whose new content isn't settled yet (it travels with the content,
// as cp -p and rsync -t set it right after writing). Of other items it is ignored: the gateway keeps its own.
func (s *Service) SetModTime(node uint64, t time.Time) error {
	if wc := s.workingCopy(node); wc != nil {
		wc.mutex.Lock()
		wc.modNs = t.UnixNano()
		wc.mutex.Unlock()
	}
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE items SET mod_ns = ? WHERE node = ? AND deleted = 0 AND local & ? != 0`,
			t.UnixNano(), node, localCreated|localContent)
		return err
	})
}

// workingCopy is a file being written: the item's content as it started, changed by writes. Every writable handle
// of a node writes to the same one.
type workingCopy struct {
	node uint64
	item int64

	mutex    sync.Mutex
	file     *os.File
	name     string
	writers  int
	size     int64
	modNs    int64
	version  uint64
	detached bool
}

func (s *Service) workingCopy(node uint64) *workingCopy {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.work[node]
}

// detachNode marks a node's working copy as no longer showing an item: what is still written to it is dropped when
// its last handle is closed
func (s *Service) detachNode(node uint64) {
	if wc := s.workingCopy(node); wc != nil {
		wc.mutex.Lock()
		wc.detached = true
		wc.mutex.Unlock()
	}
}

func (s *Service) workDir() string {
	return filepath.Join(s.cfg.StateDir, "Work")
}

func (s *Service) stagedDir() string {
	return filepath.Join(s.cfg.StateDir, "Staged")
}

// Writer writes a file through one handle
type Writer struct {
	s      *Service
	wc     *workingCopy
	entry  Entry
	closed bool
}

// OpenWriter opens a file for writing. truncate starts it empty; otherwise its working copy is made from its content
// at the first write, which needs that content here.
func (s *Service) OpenWriter(e Entry, truncate bool) (*Writer, error) {
	if !e.Writable {
		return nil, ErrReadOnly
	}
	if e.Kind != kindFile {
		return nil, ErrIsDir
	}
	s.mutex.Lock()
	wc := s.work[e.Node]
	if wc == nil {
		wc = &workingCopy{node: e.Node, item: e.Item, size: e.Size, modNs: e.ModTime.UnixNano()}
		s.work[e.Node] = wc
	}
	s.mutex.Unlock()
	wc.mutex.Lock()
	wc.writers++
	wc.mutex.Unlock()
	w := &Writer{s: s, wc: wc, entry: e}
	if truncate {
		if err := w.Truncate(context.Background(), 0); err != nil {
			w.Close()
			return nil, err
		}
	}
	return w, nil
}

// ensure makes the node's working copy, from the item's content unless empty. Callers hold the working copy's mutex.
func (w *Writer) ensure(ctx context.Context, empty bool) error {
	wc := w.wc
	if wc.file != nil {
		return nil
	}
	if err := os.MkdirAll(w.s.workDir(), 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s", wc.node, randomName())
	path := filepath.Join(w.s.workDir(), name)
	if wc.detached {
		// Removed or replaced while open: written to a copy that is dropped when it is closed, as a file removed on a
		// local filesystem is
		f, size, err := w.s.copyContent(ctx, w.entry, path)
		if err != nil {
			if f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
				return err
			}
			size = 0
		}
		if empty {
			size = 0
			if err := f.Truncate(0); err != nil {
				f.Close()
				return err
			}
		}
		wc.file, wc.size, wc.name = f, size, name
		return nil
	}
	if empty {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		wc.file, wc.size = f, 0
	} else {
		f, size, err := w.s.copyContent(ctx, w.entry, path)
		if err != nil {
			os.Remove(path)
			return err
		}
		wc.file, wc.size = f, size
	}
	wc.name = name
	// The working copy is recorded before the write that needed it is answered
	err := w.s.store.tx(context.Background(), func(tx *sql.Tx) error {
		r, err := liveItem(tx, wc.item)
		if err != nil {
			return err
		}
		if r == nil || r.node != int64(wc.node) {
			return fs.ErrNotExist
		}
		if err := captureBase(tx, wc.item); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO work (node, item, file, writing) VALUES (?, ?, ?, 1)
			ON CONFLICT (node) DO UPDATE SET file = excluded.file, writing = 1, detached = 0`, wc.node, wc.item,
			name); err != nil {
			return err
		}
		local := localContent
		if r.local&localCreated != 0 {
			local = 0
		}
		_, err = tx.Exec(`UPDATE items SET local = local | ?, local_content = ?, changed_at = ? WHERE id = ?`, local,
			filepath.Join("Work", name), nowMillis(), wc.item)
		return err
	})
	if err != nil {
		wc.file.Close()
		wc.file = nil
		os.Remove(path)
		return err
	}
	return nil
}

// copyContent copies an item's current content into a new file at path (a reflink where the filesystem has them),
// fetching it from the gateway if it isn't here
func (s *Service) copyContent(ctx context.Context, e Entry, path string) (*os.File, int64, error) {
	if src, err := s.localContentPath(e.Item); err != nil {
		return nil, 0, err
	} else if src != "" {
		return cloneFile(src, path)
	}
	if e.Size == 0 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		return f, 0, err
	}
	cached, err := s.cache.cached(e.ContentID)
	if err != nil {
		return nil, 0, err
	}
	if cached {
		return cloneFile(s.cache.contentPath(e.ContentID), path)
	}
	// Fetched first: a write changes the file it fetched, and the gateway must be reachable for it
	r, err := s.OpenFile(e.Item)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 1<<20)
	var off int64
	for off < r.Size() {
		n, err := r.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				f.Close()
				return nil, 0, err
			}
			off += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			f.Close()
			return nil, 0, err
		}
		if n == 0 {
			break
		}
	}
	return f, off, nil
}

// localContentPath is the file holding an item's content while the gateway hasn't committed it ("" when it has)
func (s *Service) localContentPath(item int64) (string, error) {
	var rel sql.NullString
	err := s.store.db.QueryRow(`SELECT local_content FROM items WHERE id = ?`, item).Scan(&rel)
	if errors.Is(err, sql.ErrNoRows) || !rel.Valid {
		return "", nil
	}
	return filepath.Join(s.cfg.StateDir, rel.String), err
}

// WriteAt writes at off
func (w *Writer) WriteAt(ctx context.Context, p []byte, off int64) (int, error) {
	wc := w.wc
	wc.mutex.Lock()
	defer wc.mutex.Unlock()
	if err := w.ensure(ctx, false); err != nil {
		return 0, err
	}
	n, err := wc.file.WriteAt(p, off)
	if end := off + int64(n); end > wc.size {
		wc.size = end
	}
	wc.modNs = time.Now().UnixNano()
	wc.version++
	return n, err
}

// Truncate sets the file's length; to zero, it needs nothing from the gateway
func (w *Writer) Truncate(ctx context.Context, size int64) error {
	wc := w.wc
	wc.mutex.Lock()
	defer wc.mutex.Unlock()
	if err := w.ensure(ctx, size == 0); err != nil {
		return err
	}
	if err := wc.file.Truncate(size); err != nil {
		return err
	}
	wc.size, wc.modNs = size, time.Now().UnixNano()
	wc.version++
	return nil
}

// Sync makes what was written durable
func (w *Writer) Sync() error {
	wc := w.wc
	wc.mutex.Lock()
	defer wc.mutex.Unlock()
	if wc.file == nil {
		return nil
	}
	if err := wc.file.Sync(); err != nil {
		return err
	}
	return w.s.recordWorkingCopy(wc, true)
}

// recordWorkingCopy stores the working copy's length and time with its item. Callers hold its mutex.
func (s *Service) recordWorkingCopy(wc *workingCopy, writing bool) error {
	return s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE items SET size = ?, mod_ns = ?, changed_at = ? WHERE node = ? AND deleted = 0`,
			wc.size, wc.modNs, nowMillis(), wc.node); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE work SET writing = ? WHERE node = ?`, writing, wc.node)
		return err
	})
}

// Close releases the handle. When the last writable handle of the node is closed, the change waits for the folder's
// changes to settle; what was written to a node that no longer shows an item is dropped.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	wc := w.wc
	wc.mutex.Lock()
	wc.writers--
	last := wc.writers == 0
	var err error
	if last && wc.file != nil {
		if wc.detached {
			wc.file.Close()
			wc.file = nil
			os.Remove(filepath.Join(w.s.workDir(), wc.name))
			err = w.s.store.tx(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`DELETE FROM work WHERE node = ? AND detached = 1`, wc.node)
				return err
			})
		} else {
			err = w.s.recordWorkingCopy(wc, false)
		}
	}
	wc.mutex.Unlock()
	if last {
		w.s.mutex.Lock()
		if wc.file == nil && w.s.work[wc.node] == wc {
			delete(w.s.work, wc.node)
		}
		w.s.mutex.Unlock()
		w.s.settleLater(w.entry.Folder)
	}
	return err
}

// readWorkingCopy reads from a node's working copy, if it has one; ok is false otherwise
func (s *Service) readWorkingCopy(node uint64, p []byte, off int64) (n int, ok bool, err error) {
	wc := s.workingCopy(node)
	if wc == nil {
		return 0, false, nil
	}
	wc.mutex.Lock()
	defer wc.mutex.Unlock()
	if wc.file == nil {
		return 0, false, nil
	}
	if off >= wc.size {
		return 0, true, io.EOF
	}
	n, err = wc.file.ReadAt(p[:min(int64(len(p)), wc.size-off)], off)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	return n, true, err
}

// withWorkingCopy shows what a node's working copy has of an entry's length and time
func (s *Service) withWorkingCopy(e Entry) Entry {
	if wc := s.workingCopy(e.Node); wc != nil {
		wc.mutex.Lock()
		if wc.file != nil {
			e.Size, e.ModTime = wc.size, time.Unix(0, wc.modNs)
		}
		wc.mutex.Unlock()
	}
	return e
}

// cloneFile makes dst a copy of src, a reflink where the filesystem has them, and opens it for writing
func cloneFile(src, dst string) (*os.File, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := reflink(out, in); err != nil {
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return nil, 0, err
		}
	}
	info, err := out.Stat()
	if err != nil {
		out.Close()
		return nil, 0, err
	}
	return out, info.Size(), nil
}
