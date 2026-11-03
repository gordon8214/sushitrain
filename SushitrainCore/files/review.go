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

// DiscardEdit gives up an edit that can't be sent (or one not sent yet) and those of its item after it: the item
// shows what the gateway has again, and an item the gateway never had goes
func (s *Service) DiscardEdit(id int64) error {
	var changes Changes
	err := s.store.tx(context.Background(), func(tx *sql.Tx) error {
		var item int64
		var state string
		err := tx.QueryRow(`SELECT item, state FROM outbox WHERE id = ? AND state IN `+unresolvedStates, id).
			Scan(&item, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no edit %d", id)
		}
		if err != nil {
			return err
		}
		if state == stateSubmitted || state == statePending {
			return fmt.Errorf("edit %d is being sent", id)
		}
		p := newProjector(tx)
		defer func() { changes = p.changes }()
		if err := s.discardItem(tx, p, item); err != nil {
			return err
		}
		return p.finish()
	})
	if err != nil {
		return err
	}
	s.changed(changes)
	s.collectStagedSoon()
	s.wakeOutbox()
	return nil
}

// discardItem drops what is pending of an item and of everything below it
func (s *Service) discardItem(tx *sql.Tx, p *projector, item int64) error {
	rows, err := tx.Query(`WITH RECURSIVE sub(id) AS (SELECT ?1 UNION ALL
			SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id)
		SELECT i.id, i.parent, i.name, i.node, i.deleted, r.gateway_id FROM sub JOIN items AS i ON i.id = sub.id
		LEFT JOIN remote_items AS r ON r.item = i.id`, item)
	if err != nil {
		return err
	}
	type entry struct {
		id      int64
		parent  sql.NullInt64
		name    string
		node    int64
		deleted bool
		gateway sql.NullString
	}
	var list []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.parent, &e.name, &e.node, &e.deleted, &e.gateway); err != nil {
			rows.Close()
			return err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range list {
		if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', error = 'discarded', updated_at = ?
			WHERE item = ? AND state IN `+unresolvedStates, nowUnix(), e.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE work SET detached = 1 WHERE item = ?`, e.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET local = 0, local_content = NULL, base_revision = NULL,
			base_content_id = NULL, base_subtree = NULL WHERE id = ?`, e.id); err != nil {
			return err
		}
		s.detachNode(uint64(e.node))
	}
	// Bottom up: an item the gateway never had goes; another shows the gateway's state, with a new node for content
	// that changes back
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		if e.gateway.Valid {
			if _, err := tx.Exec(`UPDATE items SET node = ? WHERE id = ?`, mustAllocate(tx), e.id); err != nil {
				return err
			}
			parentNode, err := p.nodeOf(e.parent)
			if err != nil {
				return err
			}
			p.changes.entry(parentNode, e.name, uint64(e.node))
			if _, err := p.place(e.gateway.String, true); err != nil {
				return err
			}
			continue
		}
		if !e.deleted {
			parentNode, err := p.nodeOf(e.parent)
			if err != nil {
				return err
			}
			p.changes.entry(parentNode, e.name, uint64(e.node))
		}
		if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, e.id); err != nil {
			return err
		}
	}
	return nil
}

func mustAllocate(tx *sql.Tx) int64 {
	node, err := allocate(tx, metaNextNode)
	if err != nil {
		panic(err)
	}
	return node
}

// Unresolved counts a folder's changes that the gateway hasn't committed: not settled, on their way, or waiting for
// review
func (s *Service) Unresolved(folder int64) (int, error) {
	var n int
	err := s.store.db.QueryRow(`SELECT
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
		st, err := s.stage(sql.NullString{String: filepath.Join("Work", w.file), Valid: true})
		if err != nil {
			return err
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
	return s.collectStaged()
}

// leftovers moves a folder's files that are only here (and so would be lost with it) out of the mount, to a new
// directory named after the folder in ~/Sync, or in the home folder if ~/Sync is itself synchronized. It refuses
// unless all of them moved.
func (s *Service) leftovers(folder int64, label string) (string, int, error) {
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
	dest := filepath.Join(base, label+" (not synchronized)")
	for i := 2; ; i++ {
		if _, err := os.Lstat(dest); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dest = filepath.Join(base, fmt.Sprintf("%s (not synchronized %d)", label, i))
	}
	var root string
	if err := s.store.db.QueryRow(`SELECT dir_name FROM folders WHERE id = ?`, folder).Scan(&root); err != nil {
		return "", 0, err
	}
	for _, l := range list {
		rel, err := s.itemPath(l.item)
		if err != nil {
			return "", 0, err
		}
		target := filepath.Join(dest, strings.TrimPrefix(rel, root+"/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", 0, err
		}
		var src string
		switch {
		case l.work.Valid:
			src = filepath.Join(s.workDir(), l.work.String)
		case l.content.Valid:
			src = filepath.Join(s.cfg.StateDir, l.content.String)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
