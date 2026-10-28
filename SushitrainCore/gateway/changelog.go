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
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

// The change log is the items table: every change to an item gives it a new change sequence, so the changes after a
// cursor are the items whose change sequence is greater. Deletions stay as tombstones for tombstoneRetention.
//
// It is fed by reconciling the engine's global index with the items table after index events, at startup and
// periodically. A pass walks both in name order (a merge join), so it finds every difference, including entries that
// left the index and global versions that fell back to an older device's entry, while reading in full only the
// entries that changed.

const (
	tombstoneRetention = 30 * 24 * time.Hour
	// Passes run after index events; this one catches anything an event did not announce
	periodicInterval  = 6 * time.Hour
	reconcileBatch    = 256
	reconcileDebounce = time.Second
	reconcileMaxDelay = 5 * time.Second
)

// Wakes long-polling requests when a folder's change log advances
type notifier struct {
	mutex   sync.Mutex
	waiters map[string]chan struct{}
}

func (n *notifier) wait(folder string) <-chan struct{} {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if n.waiters == nil {
		n.waiters = make(map[string]chan struct{})
	}
	ch, ok := n.waiters[folder]
	if !ok {
		ch = make(chan struct{})
		n.waiters[folder] = ch
	}
	return ch
}

func (n *notifier) notify(folder string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if ch, ok := n.waiters[folder]; ok {
		close(ch)
		delete(n.waiters, folder)
	}
}

// folderLock serializes reconciling a folder with mutations of it
func (s *Service) folderLock(folder string) *sync.Mutex {
	lock, _ := s.folderLocks.LoadOrStore(folder, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

type pendingEntry struct {
	entry GlobalEntry
	live  bool
}

// reconcileLocked brings the folder's items in line with the engine's global index. The folder lock must be held.
func (s *Service) reconcileLocked(ctx context.Context, folderID string) error {
	info, ok := s.backend.Folder(folderID)
	if !ok {
		return fmt.Errorf("folder %q does not exist", folderID)
	}
	root, err := s.ensureFolderRow(ctx, folderID)
	if err != nil {
		return err
	}
	if err := s.checkIndexIdentity(ctx, folderID); err != nil {
		return err
	}

	changed := false
	// Batches that were committed are announced even when a later one fails
	defer func() {
		if changed {
			s.notifier.notify(folderID)
		}
	}()
	// Directories that left the index, decided once their contents have been reconciled
	var directories []itemRow
	batch := make([]pendingEntry, 0, reconcileBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		c, dirs, err := s.applyEntries(ctx, info, root, batch)
		changed = changed || c
		directories = append(directories, dirs...)
		batch = batch[:0]
		return err
	}
	// Files whose entries left the index
	var gone []itemRow
	flushGone := func() error {
		if len(gone) == 0 {
			return nil
		}
		err := s.store.tx(ctx, func(tx *sql.Tx) error {
			ancestors := make(map[string]struct{})
			for _, row := range gone {
				if err := s.tombstone(tx, row); err != nil {
					return err
				}
				addAncestors(ancestors, row)
			}
			return s.touchAncestors(tx, ancestors)
		})
		changed = true
		gone = gone[:0]
		return err
	}
	leftIndex := func(row itemRow) error {
		if row.kind == protocol.FileInfoTypeDirectory {
			directories = append(directories, row)
			return nil
		}
		gone = append(gone, row)
		if len(gone) >= reconcileBatch {
			return flushGone()
		}
		return nil
	}

	// The live items in path order, as the pass starts: this read sees the table as it was then, which is what the
	// pass compares with; the pass's own writes go through other connections
	items, err := s.store.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE folder_id = ? AND deleted = 0
		AND path != '' ORDER BY path`, folderID)
	if err != nil {
		return err
	}
	defer items.Close()
	var current *itemRow
	next := func() error {
		current = nil
		if items.Next() {
			row, err := scanItem(items.Scan)
			if err != nil {
				return err
			}
			current = &row
		}
		return items.Err()
	}
	if err := next(); err != nil {
		return err
	}

	// Entries come in the order of their names, which is the byte order of the item paths
	err = s.backend.GlobalEntries(folderID, func(entry GlobalEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name == "" || strings.ContainsRune(entry.Name, 0) || IsSyncthingPrivatePath(entry.Name, info.MarkerName) {
			return nil
		}
		for current != nil && current.path < entry.Name {
			if err := leftIndex(*current); err != nil {
				return err
			}
			if err := next(); err != nil {
				return err
			}
		}
		live := !entry.Deleted && !entry.Invalid && entry.Type != protocol.FileInfoTypeSymlink
		exists := current != nil && current.path == entry.Name
		if exists {
			// The index sequence identifies one version of one device's entry: the same means unchanged
			unchanged := live && current.stSeq == entry.Sequence && current.kind == entry.Type
			if err := next(); err != nil {
				return err
			}
			if unchanged {
				return nil
			}
		} else if !live {
			// Deleted, and never an item
			return nil
		}
		batch = append(batch, pendingEntry{entry: entry, live: live})
		if len(batch) >= reconcileBatch {
			return flush()
		}
		return nil
	})
	for err == nil && current != nil {
		if err = leftIndex(*current); err == nil {
			err = next()
		}
	}
	if err == nil {
		err = flush()
	}
	if err == nil {
		err = flushGone()
	}
	if err != nil {
		return err
	}
	items.Close()
	c, err := s.tombstoneDirectories(ctx, directories)
	changed = changed || c
	if err != nil {
		return err
	}
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE folders SET swept_at = ? WHERE folder_id = ?`, s.store.now().Unix(), folderID)
		return err
	})
}

// applyEntries applies a batch of changed global entries in one transaction. It reports whether any item changed,
// and returns the directories that left the index, which tombstoneDirectories handles once their contents are
// reconciled.
func (s *Service) applyEntries(ctx context.Context, info FolderInfo, root string, batch []pendingEntry) (bool, []itemRow,
	error,
) {
	// Read the full index entries before taking the write lock
	files := make([]protocol.FileInfo, len(batch))
	for i, pending := range batch {
		if !pending.live {
			continue
		}
		fi, ok, err := s.backend.GlobalFile(info.ID, pending.entry.Name)
		if err != nil {
			return false, nil, err
		}
		if !ok || fi.Deleted || fi.IsInvalid() {
			batch[i].live = false
			continue
		}
		files[i] = fi
	}

	changed := false
	var directories []itemRow
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		directories = directories[:0]
		ancestors := make(map[string]struct{})
		for i, pending := range batch {
			name := pending.entry.Name
			row, exists, err := liveItemByPath(tx, info.ID, name)
			if err != nil {
				return err
			}
			switch {
			case !pending.live && exists && row.kind == protocol.FileInfoTypeDirectory:
				directories = append(directories, row)

			case !pending.live && exists:
				if err := s.tombstone(tx, row); err != nil {
					return err
				}
				addAncestors(ancestors, row)
				changed = true

			case pending.live && !exists:
				parent, err := s.ensureParent(tx, info, root, parentPath(name), ancestors)
				if err != nil {
					return err
				}
				row = itemRow{id: newID(prefixItem), folder: info.ID, path: name,
					parent: sql.NullString{String: parent, Valid: true}}
				row.setFromFileInfo(files[i], info.IgnorePerms)
				if err := s.insertItem(tx, row); err != nil {
					return err
				}
				ancestors[parent] = struct{}{}
				changed = true

			case pending.live && exists:
				fi := files[i]
				if fi.Type != row.kind {
					// A different kind of item at the same path is a different item
					if err := s.tombstone(tx, row); err != nil {
						return err
					}
					fresh := itemRow{id: newID(prefixItem), folder: info.ID, path: name, parent: row.parent}
					fresh.setFromFileInfo(fi, info.IgnorePerms)
					if err := s.insertItem(tx, fresh); err != nil {
						return err
					}
					addAncestors(ancestors, row)
					changed = true
					continue
				}
				updated := row
				updated.setFromFileInfo(fi, info.IgnorePerms)
				if updated.revision != row.revision {
					if err := s.updateItem(tx, updated); err != nil {
						return err
					}
					addAncestors(ancestors, row)
					changed = true
				} else if updated.stSeq != row.stSeq {
					// Another device's entry with the same version: remember it, so the next pass sees no change
					if _, err := tx.Exec(`UPDATE items SET st_sequence = ? WHERE item_id = ?`, updated.stSeq,
						row.id); err != nil {
						return err
					}
				}
			}
		}
		return s.touchAncestors(tx, ancestors)
	})
	return changed, directories, err
}

func addAncestors(set map[string]struct{}, row itemRow) {
	if row.parent.Valid {
		set[row.parent.String] = struct{}{}
	}
}

// touchAncestors gives every directory in set, and the directories above them, a new subtree generation, recorded
// as a change so that clients see it
func (s *Service) touchAncestors(tx *sql.Tx, set map[string]struct{}) error {
	if len(set) == 0 {
		return nil
	}
	// Collect the whole chains first, so that each directory is touched once
	all := make(map[string]struct{})
	for id := range set {
		for id != "" {
			if _, done := all[id]; done {
				break
			}
			all[id] = struct{}{}
			var parent sql.NullString
			if err := tx.QueryRow(`SELECT parent_item FROM items WHERE item_id = ?`, id).Scan(&parent); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				return err
			}
			id = parent.String
		}
	}
	// Each directory gets its own sequence: a page of changes ends at a sequence, so rows sharing one could be split
	// across pages and the rest skipped
	seq, err := nextChangeSeq(tx, int64(len(all)))
	if err != nil {
		return err
	}
	now := s.store.now().Unix()
	for id := range all {
		if _, err := tx.Exec(`UPDATE items SET subtree_gen = ?, change_seq = ?, changed_at = ? WHERE item_id = ? AND deleted = 0`,
			seq, seq, now, id); err != nil {
			return err
		}
		seq++
	}
	return nil
}

// hasLiveChildren reports whether a directory still contains items. Such a directory stays, as implied by its
// contents, when the index no longer lists it (for instance an ignored directory with included contents).
func hasLiveChildren(tx *sql.Tx, id string) (bool, error) {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM items WHERE parent_item = ? AND deleted = 0 LIMIT 1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// tombstoneDirectories deletes the directories that left the index unless they still have contents, deepest first
func (s *Service) tombstoneDirectories(ctx context.Context, rows []itemRow) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}
	slices.SortFunc(rows, func(a, b itemRow) int { return strings.Compare(b.path, a.path) })
	changed := false
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		changed = false
		ancestors := make(map[string]struct{})
		for _, row := range rows {
			implied, err := hasLiveChildren(tx, row.id)
			if err != nil {
				return err
			}
			if implied {
				continue
			}
			if err := s.tombstone(tx, row); err != nil {
				return err
			}
			addAncestors(ancestors, row)
			changed = true
		}
		return s.touchAncestors(tx, ancestors)
	})
	return changed, err
}

// ensureParent returns the item ID of the directory at p, creating rows for directories the index does not list
// (implied by their contents)
func (s *Service) ensureParent(tx *sql.Tx, info FolderInfo, root string, p string, ancestors map[string]struct{}) (string, error) {
	if p == "" {
		return root, nil
	}
	row, ok, err := liveItemByPath(tx, info.ID, p)
	if err != nil {
		return "", err
	}
	if ok {
		return row.id, nil
	}
	parent, err := s.ensureParent(tx, info, root, parentPath(p), ancestors)
	if err != nil {
		return "", err
	}
	implied := itemRow{id: newID(prefixItem), folder: info.ID, path: p, parent: sql.NullString{String: parent, Valid: true}}
	implied.setFromFileInfo(protocol.FileInfo{Name: p, Type: protocol.FileInfoTypeDirectory, NoPermissions: true}, true)
	if err := s.insertItem(tx, implied); err != nil {
		return "", err
	}
	ancestors[parent] = struct{}{}
	return implied.id, nil
}

func (s *Service) insertItem(tx *sql.Tx, row itemRow) error {
	seq, err := nextChangeSeq(tx, 1)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO items (item_id, folder_id, path, parent_item, kind, deleted, st_sequence, revision,
		content_id, size, mod_ns, mode, subtree_gen, change_seq, changed_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.folder, row.path, row.parent, row.kind, row.stSeq, row.revision, row.contentID, row.size, row.modNs,
		row.mode, seq, seq, s.store.now().Unix())
	return err
}

func (s *Service) updateItem(tx *sql.Tx, row itemRow) error {
	seq, err := nextChangeSeq(tx, 1)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE items SET path = ?, parent_item = ?, kind = ?, st_sequence = ?, revision = ?, content_id = ?,
		size = ?, mod_ns = ?, mode = ?, change_seq = ?, changed_at = ? WHERE item_id = ?`,
		row.path, row.parent, row.kind, row.stSeq, row.revision, row.contentID, row.size, row.modNs, row.mode, seq,
		s.store.now().Unix(), row.id)
	return err
}

// tombstone marks an item deleted, as a change
func (s *Service) tombstone(tx *sql.Tx, row itemRow) error {
	seq, err := nextChangeSeq(tx, 1)
	if err != nil {
		return err
	}
	revision := revisionOf(protocol.FileInfo{Name: row.path, Type: row.kind, Deleted: true,
		Version: protocol.Vector{Counters: []protocol.Counter{{ID: protocol.ShortID(seq), Value: uint64(seq)}}}})
	_, err = tx.Exec(`UPDATE items SET deleted = 1, revision = ?, change_seq = ?, changed_at = ? WHERE item_id = ?`,
		revision, seq, s.store.now().Unix(), row.id)
	return err
}

// ensureFolderRow records an exported folder and its root item, and returns the root's ID
func (s *Service) ensureFolderRow(ctx context.Context, folderID string) (string, error) {
	var root string
	err := s.store.db.QueryRow(`SELECT root_item FROM folders WHERE folder_id = ?`, folderID).Scan(&root)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT root_item FROM folders WHERE folder_id = ?`, folderID).Scan(&root); err == nil {
			return nil
		}
		root = newID(prefixItem)
		row := itemRow{id: root, folder: folderID, path: ""}
		row.setFromFileInfo(protocol.FileInfo{Type: protocol.FileInfoTypeDirectory, NoPermissions: true}, true)
		if err := s.insertItem(tx, row); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO folders (folder_id, root_item) VALUES (?, ?)`, folderID, root)
		return err
	})
	return root, err
}

// checkIndexIdentity compares the engine's index of the folder with what the service last saw. A different index ID,
// or a local sequence that went backwards, means Syncthing's database was reset or restored without the service's:
// the service may have recorded states the engine no longer knows, so it starts a new authority generation (the pass
// that follows compares every entry anyway).
func (s *Service) checkIndexIdentity(ctx context.Context, folderID string) error {
	indexID, err := s.backend.IndexID(folderID)
	if err != nil {
		return err
	}
	localSeq, err := s.backend.LocalSequence(folderID)
	if err != nil {
		return err
	}
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		var knownID, knownSeq int64
		err := tx.QueryRow(`SELECT index_id, local_sequence FROM folders WHERE folder_id = ?`, folderID).Scan(&knownID,
			&knownSeq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if knownID != 0 && (knownID != int64(indexID) || localSeq < knownSeq) {
			gen, err := bumpGeneration(tx)
			if err != nil {
				return err
			}
			s.log.Warn("Syncthing's index of the folder was reset or restored; starting a new authority generation",
				"folder", folderID, "generation", gen)
		}
		_, err = tx.Exec(`UPDATE folders SET index_id = ?, local_sequence = ? WHERE folder_id = ?`, int64(indexID),
			localSeq, folderID)
		return err
	})
}

// pruneTombstones removes tombstones older than the retention period. Cursors from before the newest pruned
// tombstone then expire.
func (s *Service) pruneTombstones(ctx context.Context) error {
	cutoff := s.store.now().Add(-tombstoneRetention).Unix()
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		// Tombstones of deleted items, and of items that moved to another folder
		rows, err := tx.Query(`SELECT folder_id, MAX(change_seq) FROM (
			SELECT folder_id, change_seq FROM items WHERE deleted = 1 AND changed_at < ?
			UNION ALL SELECT folder_id, change_seq FROM moved_out WHERE changed_at < ?) GROUP BY folder_id`, cutoff, cutoff)
		if err != nil {
			return err
		}
		pruned := map[string]int64{}
		for rows.Next() {
			var folder string
			var seq int64
			if err := rows.Scan(&folder, &seq); err != nil {
				rows.Close()
				return err
			}
			pruned[folder] = seq
		}
		rows.Close()
		for folder, seq := range pruned {
			if _, err := tx.Exec(`DELETE FROM items WHERE folder_id = ? AND deleted = 1 AND change_seq <= ?`, folder,
				seq); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM moved_out WHERE folder_id = ? AND change_seq <= ?`, folder, seq); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE folders SET pruned_below = MAX(pruned_below, ?) WHERE folder_id = ?`, seq,
				folder); err != nil {
				return err
			}
		}
		return nil
	})
}

// runReconciler reconciles folders after index events, debounced, and does full passes periodically
func (s *Service) runReconciler(ctx context.Context) {
	// Events only mark folders dirty, so none is lost while a pass runs
	var dirtyMutex sync.Mutex
	dirty := make(map[string]struct{})
	wake := make(chan struct{}, 1)
	stop := s.backend.Watch(func(folder string) {
		dirtyMutex.Lock()
		dirty[folder] = struct{}{}
		dirtyMutex.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	defer stop()

	// Folders waiting for a pass, since when
	first := make(map[string]time.Time)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	periodic := time.NewTicker(periodicInterval)
	defer periodic.Stop()
	reconcile := func(folder string) {
		if s.reconcileFolder(ctx, folder) {
			delete(first, folder)
			return
		}
		// Held by an operation: try again shortly
		if _, ok := first[folder]; !ok {
			first[folder] = time.Now()
		}
		timer.Reset(reconcileDebounce)
	}

	for _, folder := range s.exportedFolderIDs() {
		reconcile(folder)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			dirtyMutex.Lock()
			folders := dirty
			dirty = make(map[string]struct{})
			dirtyMutex.Unlock()
			for folder := range folders {
				if !s.isExported(folder) {
					continue
				}
				if _, ok := first[folder]; !ok {
					first[folder] = time.Now()
				}
				timer.Reset(reconcileDebounce)
			}
		case <-timer.C:
			for folder := range first {
				reconcile(folder)
			}
		case <-s.exportsChanged:
			for _, folder := range s.exportedFolderIDs() {
				reconcile(folder)
			}
		case <-periodic.C:
			if err := s.pruneTombstones(ctx); err != nil {
				s.log.Warn("Could not prune tombstones", "error", err)
			}
			for _, folder := range s.exportedFolderIDs() {
				reconcile(folder)
			}
		}
		// Don't let a steady stream of events delay a pass indefinitely
		for folder, since := range first {
			if time.Since(since) > reconcileMaxDelay {
				reconcile(folder)
			}
		}
	}
}

// reconcileFolder reconciles a folder unless an operation holds it, and reports whether it did. An operation may hold
// the folder while it waits for Syncthing's folder loop (behind a long pull), which must not hold up other folders.
func (s *Service) reconcileFolder(ctx context.Context, folder string) bool {
	lock := s.folderLock(folder)
	if !lock.TryLock() {
		return false
	}
	defer lock.Unlock()
	if err := s.reconcileLocked(ctx, folder); err != nil && ctx.Err() == nil {
		s.log.Warn("Could not reconcile folder", "folder", folder, "error", err)
	}
	s.retryPendingOperations(folder)
	return true
}
