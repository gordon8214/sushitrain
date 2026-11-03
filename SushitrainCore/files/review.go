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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// Edit is a change made here that the gateway hasn't committed: on its way, or one that can't be sent and waits to
// be exported or discarded
type Edit struct {
	ID      int64     `json:"id"`
	Folder  string    `json:"folder"`
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	Size    int64     `json:"size"`
	Content bool      `json:"content"`
	Updated time.Time `json:"updated"`
}

// Review reports whether the edit waits for someone to export or discard it
func (e Edit) Review() bool {
	return e.State == stateFailed || e.State == stateUnknown || e.State == stateBlocked
}

// Edits lists the changes that aren't committed, with those waiting for review
func (s *Service) Edits() ([]Edit, error) {
	rows, err := s.store.db.Query(`SELECT o.id, f.dir_name, o.item, o.kind, o.state, o.error, o.size,
		o.staged IS NOT NULL, o.updated_at FROM outbox AS o JOIN folders AS f ON f.id = o.folder
		WHERE o.state IN ` + unresolvedStates + ` ORDER BY o.id`)
	if err != nil {
		return nil, err
	}
	type row struct {
		e    Edit
		item int64
	}
	var list []row
	for rows.Next() {
		var r row
		var updated int64
		if err := rows.Scan(&r.e.ID, &r.e.Folder, &r.item, &r.e.Kind, &r.e.State, &r.e.Error, &r.e.Size,
			&r.e.Content, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		r.e.Updated = time.Unix(updated, 0)
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]Edit, 0, len(list))
	for _, r := range list {
		r.e.Path, _ = s.itemPath(r.item)
		result = append(result, r.e)
	}
	return result, nil
}

// itemPath is an item's path below the mount's root, as it is (or was) here
func (s *Service) itemPath(item int64) (string, error) {
	var names []string
	for id := (sql.NullInt64{Int64: item, Valid: true}); id.Valid; {
		var name string
		var parent sql.NullInt64
		if err := s.store.db.QueryRow(`SELECT name, parent FROM items WHERE id = ?`, id.Int64).Scan(&name,
			&parent); err != nil {
			return strings.Join(names, "/"), err
		}
		names = append([]string{name}, names...)
		id = parent
	}
	return strings.Join(names, "/"), nil
}

// ExportEdit copies the content an edit sends to dest, which must not exist
func (s *Service) ExportEdit(id int64, dest string) error {
	var staged sql.NullString
	err := s.store.db.QueryRow(`SELECT staged FROM outbox WHERE id = ? AND state IN `+unresolvedStates, id).
		Scan(&staged)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no edit %d", id)
	}
	if err != nil {
		return err
	}
	if !staged.Valid {
		return fmt.Errorf("edit %d has no content (it moves or removes an item)", id)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := exportContent(filepath.Join(s.cfg.StateDir, staged.String), out); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// The states of an operation that discarding it gives up: not those on their way to the gateway
const discardableStates = `('queued', 'uploaded', 'failed', 'unknown', 'blocked')`

// DiscardEdit gives up an edit that can't be sent (or one not sent yet) and those of its item after it: the item
// shows what it showed before them, which is what the gateway has unless an earlier edit of it is still to be sent.
// An item whose creation is discarded goes, with what was made below it.
func (s *Service) DiscardEdit(id int64) error {
	var changes Changes
	var detach []uint64
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		var item int64
		var state, kind string
		err := tx.QueryRow(`SELECT item, state, kind FROM outbox WHERE id = ? AND state IN `+unresolvedStates, id).
			Scan(&item, &state, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no edit %d", id)
		}
		if err != nil {
			return err
		}
		if state == stateSubmitted || state == statePending {
			return fmt.Errorf("edit %d is being sent", id)
		}
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM items WHERE id = ?)`, item).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			// Its item was removed here before it reached the gateway: the edit is all that is left
			_, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = 'discarded', updated_at = ?
				WHERE item = ? AND id >= ? AND state IN `+discardableStates, nowUnix(), item, id)
			return err
		}
		p := newProjector(tx)
		defer func() { changes = p.changes }()
		if detach, err = s.discardItem(tx, p, item, id, kind == remoteapi.OpCreate); err != nil {
			return err
		}
		return p.finish()
	})
	if err != nil {
		return err
	}
	s.changed(changes)
	for _, node := range detach {
		s.detachNode(node)
	}
	s.collectStagedSoon()
	s.wakeOutbox()
	return nil
}

// discardItem drops the edits of an item from operation from on, and, when that is its creation, the item and what
// was made below it. It returns the nodes whose working copies no longer show anything, to detach after the commit.
func (s *Service) discardItem(tx *sql.Tx, p *projector, item, from int64, creation bool) ([]uint64, error) {
	// An earlier edit still to be sent keeps the item as it made it
	var earlier sql.NullString
	var hasEarlier bool
	err := tx.QueryRow(`SELECT staged FROM outbox WHERE item = ? AND id < ? AND state IN `+unresolvedStates+`
		ORDER BY id DESC LIMIT 1`, item, from).Scan(&earlier)
	switch {
	case err == nil:
		hasEarlier = true
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if hasEarlier {
		var parent sql.NullInt64
		var name string
		var node int64
		if err := tx.QueryRow(`SELECT parent, name, node FROM items WHERE id = ?`, item).Scan(&parent, &name,
			&node); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = 'discarded', updated_at = ?
			WHERE item = ? AND id >= ? AND state IN `+discardableStates, nowUnix(), item, from); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE work SET detached = 1 WHERE item = ?`, item); err != nil {
			return nil, err
		}
		next, err := allocate(tx, metaNextNode)
		if err != nil {
			return nil, err
		}
		// Its content goes back to what the earlier edit sends, under a new node for handles open on the other
		if _, err := tx.Exec(`UPDATE items SET node = ?, local_content = ?, local = local & ~? WHERE id = ?`, next,
			earlier, localContent, item); err != nil {
			return nil, err
		}
		parentNode, err := p.nodeOf(parent)
		if err != nil {
			return nil, err
		}
		p.changes.entry(parentNode, name, uint64(node))
		return []uint64{uint64(node)}, nil
	}

	query := `SELECT i.id, i.parent, i.name, i.node, i.deleted, i.local, r.gateway_id FROM items AS i
		LEFT JOIN remote_items AS r ON r.item = i.id WHERE i.id = ?1`
	if creation {
		query = `WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
				SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id)
			SELECT i.id, i.parent, i.name, i.node, i.deleted, i.local, r.gateway_id FROM sub
			JOIN items AS i ON i.id = sub.id LEFT JOIN remote_items AS r ON r.item = i.id`
	}
	rows, err := tx.Query(query, item)
	if err != nil {
		return nil, err
	}
	type entry struct {
		id      int64
		parent  sql.NullInt64
		name    string
		node    int64
		deleted bool
		local   int
		gateway sql.NullString
	}
	var list []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.parent, &e.name, &e.node, &e.deleted, &e.local, &e.gateway); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, e := range list[1:] {
		if e.local&localOnly != 0 && !e.deleted {
			return nil, fmt.Errorf("%s holds files that are only on this machine, such as %q: move them out first",
				list[0].name, e.name)
		}
	}
	var detach []uint64
	for i, e := range list {
		// The item's own edits from the discarded one on, and every edit of what was made below it
		first := int64(0)
		if i == 0 {
			first = from
		}
		if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = 'discarded', updated_at = ?
			WHERE item = ? AND id >= ? AND state IN `+discardableStates, nowUnix(), e.id, first); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE work SET detached = 1 WHERE item = ?`, e.id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE items SET local = 0, local_content = NULL, base_revision = NULL,
			base_content_id = NULL, base_subtree = NULL WHERE id = ?`, e.id); err != nil {
			return nil, err
		}
		detach = append(detach, uint64(e.node))
	}
	// Bottom up: an item the gateway never had goes; another shows the gateway's state, with a new node for content
	// that changes back
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		if e.gateway.Valid {
			next, err := allocate(tx, metaNextNode)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE items SET node = ? WHERE id = ?`, next, e.id); err != nil {
				return nil, err
			}
			parentNode, err := p.nodeOf(e.parent)
			if err != nil {
				return nil, err
			}
			p.changes.entry(parentNode, e.name, uint64(e.node))
			if _, err := p.place(e.gateway.String, true); err != nil {
				return nil, err
			}
			continue
		}
		if !e.deleted {
			parentNode, err := p.nodeOf(e.parent)
			if err != nil {
				return nil, err
			}
			p.changes.removed(parentNode, e.name, uint64(e.node))
		}
		if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, e.id); err != nil {
			return nil, err
		}
	}
	return detach, nil
}

// Unresolved counts a folder's changes that the gateway hasn't committed: not settled, on their way, or waiting for
// review
func (s *Service) Unresolved(folder int64) (int, error) {
	return unresolved(s.store.db, folder)
}

func unresolved(q queryer, folder int64) (int, error) {
	var n int
	err := q.QueryRow(`SELECT
		(SELECT COUNT(*) FROM outbox WHERE folder = ?1 AND state IN `+unresolvedStates+`) +
		(SELECT COUNT(*) FROM items WHERE folder = ?1 AND local & ?2 != 0 AND local & ?3 = 0) +
		(SELECT COUNT(*) FROM work AS w JOIN items AS i ON i.id = w.item WHERE i.folder = ?1 AND w.detached = 0
			AND i.local & ?3 = 0)`, folder, localCreated|localContent|localMoved|localDeleted, localOnly).Scan(&n)
	return n, err
}

// Wait waits until the changes of an item (and of everything below it) are committed by the gateway, or can't be
// sent
func (s *Service) Wait(ctx context.Context, item int64) error {
	for {
		var pending, review bool
		err := s.store.db.QueryRow(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
				SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id)
			SELECT EXISTS (SELECT 1 FROM sub JOIN items AS i ON i.id = sub.id WHERE i.local & ?2 != 0
					AND i.local & ?3 = 0)
				OR EXISTS (SELECT 1 FROM outbox WHERE item IN (SELECT id FROM sub) AND state IN `+activeStates+`)
				OR EXISTS (SELECT 1 FROM work AS w JOIN items AS i ON i.id = w.item WHERE w.item IN (SELECT id FROM sub)
					AND w.detached = 0 AND i.local & ?3 = 0),
			EXISTS (SELECT 1 FROM outbox WHERE item IN (SELECT id FROM sub) AND state IN ('failed', 'unknown',
				'blocked'))`, item, localCreated|localContent|localMoved|localDeleted, localOnly).Scan(&pending, &review)
		if err != nil {
			return err
		}
		if review {
			return errors.New("a change can't be sent: export or discard it")
		}
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// recover puts things in order after the daemon stopped: what was written to removed nodes goes; working copies that
// were being written when it stopped (their content may be torn) are kept for review rather than sent; content that
// nothing refers to goes
func (s *Service) recover() error {
	rows, err := s.store.db.Query(`SELECT w.node, w.item, w.file, w.detached, w.writing, i.folder, i.local,
		(SELECT r.gateway_id FROM remote_items AS r WHERE r.item = i.id)
		FROM work AS w LEFT JOIN items AS i ON i.id = w.item`)
	if err != nil {
		return err
	}
	type workRow struct {
		node, item int64
		file       string
		detached   bool
		writing    bool
		folder     sql.NullInt64
		local      sql.NullInt64
		gateway    sql.NullString
	}
	var list []workRow
	for rows.Next() {
		var w workRow
		if err := rows.Scan(&w.node, &w.item, &w.file, &w.detached, &w.writing, &w.folder, &w.local,
			&w.gateway); err != nil {
			rows.Close()
			return err
		}
		list = append(list, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range list {
		path := filepath.Join(s.workDir(), w.file)
		if w.detached || !w.folder.Valid {
			os.Remove(path)
			if err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`DELETE FROM work WHERE node = ?`, w.node)
				return err
			}); err != nil {
				return err
			}
			continue
		}
		if !w.writing {
			continue
		}
		if w.local.Int64&localOnly != 0 {
			// A file that is never sent (an editor's swap file): it keeps what it has
			if err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE work SET writing = 0 WHERE node = ?`, w.node)
				return err
			}); err != nil {
				return err
			}
			continue
		}
		st, err := s.stage(sql.NullString{String: filepath.Join("Work", w.file), Valid: true})
		if err != nil {
			// One file that can't be recovered (its working copy is gone after a power loss, say) mustn't keep the
			// rest from starting; with its file gone, its change is lost
			s.log.Warn("Could not recover a change that was being written", "item", w.item, "error", err)
			if errors.Is(err, fs.ErrNotExist) {
				_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
					if _, err := tx.Exec(`DELETE FROM work WHERE node = ?`, w.node); err != nil {
						return err
					}
					_, err := tx.Exec(`UPDATE items SET local = local & ~?, local_content = NULL WHERE id = ?
						AND local_content = ?`, localContent, w.item, filepath.Join("Work", w.file))
					return err
				})
			}
			continue
		}
		err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
			kind, itemKind := "replace", ""
			if w.local.Int64&localCreated != 0 && !w.gateway.Valid {
				kind, itemKind = "create", "file"
			}
			var parent sql.NullInt64
			var name string
			var modNs int64
			if err := tx.QueryRow(`SELECT parent, name, mod_ns FROM items WHERE id = ?`, w.item).Scan(&parent, &name,
				&modNs); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO outbox (op_id, folder, item, kind, item_kind, state, target_parent,
				target_name, staged, digest, size, mod_ns, error, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 'blocked', ?, ?, ?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), w.folder, w.item, kind,
				itemKind, parent, name, st.rel, remoteapi.B64.EncodeToString(st.digest[:]), st.size, modNs,
				"it was being written when Synctrain stopped", nowUnix(), nowUnix()); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE items SET local = local & ~?, local_content = ?, size = ? WHERE id = ?`,
				localContent|localCreated, st.rel, st.size, w.item); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM work WHERE node = ?`, w.node)
			return err
		})
		if err != nil {
			return err
		}
		os.Remove(path)
		s.log.Warn("A file was being written when Synctrain stopped; its content is kept for review", "item", w.item)
	}
	// Working copies no row refers to (made as the daemon stopped, or replaced) are of no use
	referenced := map[string]bool{}
	rows, err = s.store.db.Query(`SELECT file FROM work`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var file string
		if err := rows.Scan(&file); err != nil {
			rows.Close()
			return err
		}
		referenced[file] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	entries, _ := os.ReadDir(s.workDir())
	for _, entry := range entries {
		if !referenced[entry.Name()] {
			os.Remove(filepath.Join(s.workDir(), entry.Name()))
		}
	}
	return s.collectStaged()
}

// leftovers moves a folder's files that are only here (and so would be lost with it) out of the mount, to a new
// directory named after the folder in ~/Sync, or in the home folder if ~/Sync is itself synchronized. It refuses
// unless all of them moved.
func (s *Service) leftovers(folder int64) (string, int, error) {
	rows, err := s.store.db.Query(`SELECT i.id, w.file, i.local_content FROM items AS i
		LEFT JOIN work AS w ON w.node = i.node AND w.detached = 0
		WHERE i.folder = ? AND i.deleted = 0 AND i.kind = ? AND i.local & ? != 0`, folder, kindFile, localOnly)
	if err != nil {
		return "", 0, err
	}
	type leftover struct {
		item    int64
		work    sql.NullString
		content sql.NullString
	}
	var list []leftover
	for rows.Next() {
		var l leftover
		if err := rows.Scan(&l.item, &l.work, &l.content); err != nil {
			rows.Close()
			return "", 0, err
		}
		list = append(list, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return "", 0, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", 0, err
	}
	base := filepath.Join(home, "Sync")
	if _, err := os.Stat(filepath.Join(base, ".stfolder")); err == nil {
		base = home
	}
	// Named after the folder's directory in the mount, which is one valid name, never after the label as the
	// gateway gave it
	var root string
	if err := s.store.db.QueryRow(`SELECT dir_name FROM folders WHERE id = ?`, folder).Scan(&root); err != nil {
		return "", 0, err
	}
	if !validName(root) {
		root = "Folder"
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", 0, err
	}
	var dest string
	for i := 1; ; i++ {
		dest = filepath.Join(base, root+" (not synchronized)")
		if i > 1 {
			dest = filepath.Join(base, fmt.Sprintf("%s (not synchronized %d)", root, i))
		}
		if filepath.Dir(dest) != base {
			return "", 0, fmt.Errorf("%q can't be a directory name", root)
		}
		// A new directory, this user's only, which nothing else could have put in place beforehand
		err := os.Mkdir(dest, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", 0, err
		}
	}
	into, err := os.OpenRoot(dest)
	if err != nil {
		return "", 0, err
	}
	defer into.Close()
	for _, l := range list {
		rel, err := s.itemPath(l.item)
		if err != nil {
			return "", 0, err
		}
		target := strings.TrimPrefix(rel, root+"/")
		if dir := filepath.Dir(target); dir != "." {
			if err := into.MkdirAll(dir, 0o700); err != nil {
				return "", 0, err
			}
		}
		var src string
		switch {
		case l.work.Valid:
			src = filepath.Join(s.workDir(), l.work.String)
		case l.content.Valid:
			src = filepath.Join(s.cfg.StateDir, l.content.String)
		}
		out, err := into.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", 0, err
		}
		if src != "" {
			if err := exportContent(src, out); err != nil {
				out.Close()
				return "", 0, err
			}
		}
		if err := out.Sync(); err != nil {
			out.Close()
			return "", 0, err
		}
		if err := out.Close(); err != nil {
			return "", 0, err
		}
	}
	return dest, len(list), nil
}

// GatewayID is the gateway's ID of an item ("" while the gateway doesn't have it)
func (s *Service) GatewayID(item int64) (string, error) {
	var id string
	err := s.store.db.QueryRow(`SELECT gateway_id FROM remote_items WHERE item = ?`, item).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SentCount is how many operations the gateway has taken, for tests that check nothing was sent
func (s *Service) SentCount() int {
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE state IN ('committed', 'conflict')`).Scan(&n)
	return n
}

// ItemPath is an item's slash-separated path below the mount's root
func (s *Service) ItemPath(item int64) (string, error) {
	return s.itemPath(item)
}

// Leave takes every folder out of the mount, for a switch to Synchronize mode. Changes still being sent are waited for
// until ctx is done; any that isn't on its gateway then stops the switch with every folder in place. Files that are only
// here are moved out of each folder, and the cache is emptied. The pairings with gateways stay, for a switch back.
func (s *Service) Leave(ctx context.Context) ([]Removal, error) {
	folders, err := s.folderRefs()
	if err != nil {
		return nil, err
	}
	for _, f := range folders {
		if err := s.Wait(ctx, f.rootItem); err != nil {
			return nil, fmt.Errorf("%w in %s: %w", ErrUnresolved, f.dirName, err)
		}
	}
	for _, f := range folders {
		if n, err := s.Unresolved(f.id); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, fmt.Errorf("%w: %d in %s", ErrUnresolved, n, f.dirName)
		}
	}
	var removals []Removal
	for _, f := range folders {
		dest, moved, err := s.RemoveFolder(f.id)
		if err != nil {
			return removals, fmt.Errorf("removing %s: %w", f.dirName, err)
		}
		removals = append(removals, Removal{Leftovers: dest, Moved: moved})
	}
	return removals, s.cache.evict()
}
