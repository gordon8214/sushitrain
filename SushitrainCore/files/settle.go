// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// DefaultSettleDelay is how long a folder must be quiet before its changes are settled into operations: editors save
// through temporary files and renames, which are one change to send
const DefaultSettleDelay = 2 * time.Second

// How long a busy folder (a file written for a long time) may hold back the changes that don't depend on it
const settleMaxWait = 30 * time.Second

// errUnsettled means a change was made while settling: the folder is settled again later
var errUnsettled = errors.New("the folder changed while its changes were settled")

// settleLater settles a folder's changes once it has been quiet for a while
func (s *Service) settleLater(folder int64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if t, ok := s.settleTimers[folder]; ok {
		t.Reset(s.cfg.SettleDelay)
		return
	}
	s.settleTimers[folder] = time.AfterFunc(s.cfg.SettleDelay, func() {
		s.mutex.Lock()
		delete(s.settleTimers, folder)
		s.mutex.Unlock()
		if err := s.settleFolder(folder); err != nil && s.ctx.Err() == nil {
			if !errors.Is(err, errUnsettled) {
				s.log.Warn("Could not settle changes", "folder", folder, "error", err)
			}
			s.settleLater(folder)
		}
	})
}

// candidate is an item with changes to settle
type candidate struct {
	id          int64
	parent      sql.NullInt64
	name        string
	kind        int
	node        int64
	local       int
	deleted     bool
	changedAt   int64
	gateway     sql.NullString
	baseRev     sql.NullString
	baseContent sql.NullString
	baseSubtree sql.NullInt64
	modNs       int64
	work        sql.NullString
	// Where the gateway has it (its parent's item and its name)
	baseParent sql.NullInt64
	baseName   sql.NullString
	depth      int

	staged  *staged
	version uint64
	busy    bool
	gone    bool
}

type staged struct {
	rel    string
	size   int64
	digest [sha256.Size]byte
}

func (c *candidate) live() bool {
	return !c.deleted
}

// settleFolder turns a folder's settled changes into operations, in one transaction, after staging the content they
// send. A change made meanwhile leaves everything as it was, to be settled later.
func (s *Service) settleFolder(folder int64) error {
	s.settleMutex.Lock()
	defer s.settleMutex.Unlock()
	cands, err := s.candidates(folder)
	if err != nil || len(cands) == 0 {
		return err
	}
	now := nowMillis()
	var newest int64
	busy := false
	for _, c := range cands {
		newest = max(newest, c.changedAt)
		if wc := s.workingCopy(uint64(c.node)); wc != nil {
			wc.mutex.Lock()
			c.busy = wc.writers > 0
			c.version = wc.version
			wc.mutex.Unlock()
		}
		busy = busy || c.busy
	}
	if now-newest < s.cfg.SettleDelay.Milliseconds() {
		s.settleLater(folder)
		return nil
	}
	if busy {
		s.mutex.Lock()
		since, ok := s.busySince[folder]
		if !ok {
			s.busySince[folder] = time.Now()
			since = time.Now()
		}
		s.mutex.Unlock()
		if time.Since(since) < settleMaxWait {
			s.settleLater(folder)
			return nil
		}
	}
	s.mutex.Lock()
	delete(s.busySince, folder)
	s.mutex.Unlock()
	cands = settleable(cands)
	if len(cands) == 0 {
		s.settleLater(folder)
		return nil
	}

	// The content to send, staged outside the transaction: immutable copies, on disk before anything refers to them
	var stagedFiles []string
	defer func() {
		for _, p := range stagedFiles {
			os.Remove(p)
		}
	}()
	for _, c := range cands {
		if !c.live() || c.kind != kindFile || c.local&(localCreated|localContent) == 0 {
			continue
		}
		st, err := s.stage(c.work)
		if err != nil {
			return err
		}
		stagedFiles = append(stagedFiles, filepath.Join(s.cfg.StateDir, st.rel))
		c.staged = st
	}
	if err := s.hook("settle.staged"); err != nil {
		return err
	}

	var consumed []*candidate
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		for _, c := range cands {
			var changedAt int64
			err := tx.QueryRow(`SELECT changed_at FROM items WHERE id = ?`, c.id).Scan(&changedAt)
			if err != nil || changedAt != c.changedAt {
				return errUnsettled
			}
			if wc := s.workingCopy(uint64(c.node)); wc != nil {
				wc.mutex.Lock()
				changed := wc.version != c.version || wc.writers > 0
				wc.mutex.Unlock()
				if changed {
					return errUnsettled
				}
			}
		}
		var err error
		consumed, err = s.plan(tx, folder, cands)
		return err
	})
	if err != nil {
		return err
	}
	// The staged files are referred to now; the working copies they were made from are not
	stagedFiles = nil
	for _, c := range consumed {
		if c.work.Valid {
			os.Remove(filepath.Join(s.cfg.StateDir, c.work.String))
		}
		s.mutex.Lock()
		if wc := s.work[uint64(c.node)]; wc != nil {
			wc.mutex.Lock()
			if wc.writers == 0 {
				if wc.file != nil {
					wc.file.Close()
				}
				delete(s.work, uint64(c.node))
			}
			wc.mutex.Unlock()
		}
		s.mutex.Unlock()
	}
	s.wakeOutbox()
	return nil
}

// settleable leaves out what depends on files still being written: a removed or new file where one is being
// written (an editor's save in progress), and those files themselves
func settleable(cands []*candidate) []*candidate {
	type location struct {
		parent int64
		name   string
	}
	busyAt := map[location]bool{}
	for _, c := range cands {
		if c.busy && c.parent.Valid {
			busyAt[location{c.parent.Int64, c.name}] = true
		}
	}
	var result []*candidate
	for _, c := range cands {
		if c.busy {
			continue
		}
		if c.baseParent.Valid && busyAt[location{c.baseParent.Int64, c.baseName.String}] {
			continue
		}
		if c.parent.Valid && busyAt[location{c.parent.Int64, c.name}] {
			continue
		}
		result = append(result, c)
	}
	return result
}

func (s *Service) candidates(folder int64) ([]*candidate, error) {
	rows, err := s.store.db.Query(`SELECT i.id, i.parent, i.name, i.kind, i.node, i.local, i.deleted, i.changed_at,
		r.gateway_id, i.base_revision, i.base_content_id, i.base_subtree, i.mod_ns,
		(SELECT 'Work/' || w.file FROM work AS w WHERE w.node = i.node AND w.detached = 0),
		(SELECT p.item FROM remote_items AS p WHERE p.gateway_id = r.parent_gateway_id), r.name
		FROM items AS i LEFT JOIN remote_items AS r ON r.item = i.id
		WHERE i.folder = ? AND i.local & ? != 0 AND i.local & ? = 0`, folder,
		localCreated|localContent|localMoved|localDeleted, localOnly|localFolded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []*candidate
	for rows.Next() {
		c := &candidate{}
		if err := rows.Scan(&c.id, &c.parent, &c.name, &c.kind, &c.node, &c.local, &c.deleted, &c.changedAt, &c.gateway,
			&c.baseRev, &c.baseContent, &c.baseSubtree, &c.modNs, &c.work, &c.baseParent, &c.baseName); err != nil {
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A folder's root has no gateway parent item of its own in remote_items: its children's base parent is the root
	var root int64
	var rootGateway sql.NullString
	if err := s.store.db.QueryRow(`SELECT root_item, root_gateway_id FROM folders WHERE id = ?`, folder).
		Scan(&root, &rootGateway); err != nil {
		return nil, err
	}
	for _, c := range cands {
		if !c.baseParent.Valid && c.gateway.Valid && rootGateway.Valid {
			var parent sql.NullString
			_ = s.store.db.QueryRow(`SELECT parent_gateway_id FROM remote_items WHERE gateway_id = ?`, c.gateway.String).
				Scan(&parent)
			if parent.Valid && parent.String == rootGateway.String {
				c.baseParent = sql.NullInt64{Int64: root, Valid: true}
			}
		}
		if err := s.store.db.QueryRow(`WITH RECURSIVE up(id, parent, depth) AS (SELECT id, parent, 0 FROM items
			WHERE id = ? UNION ALL SELECT i.id, i.parent, up.depth + 1 FROM items AS i JOIN up ON i.id = up.parent)
			SELECT MAX(depth) FROM up`, c.id).Scan(&c.depth); err != nil {
			return nil, err
		}
	}
	return cands, nil
}

// stage makes an immutable copy of a working copy (or an empty file's content) to upload, and hashes it
func (s *Service) stage(work sql.NullString) (*staged, error) {
	if err := os.MkdirAll(s.stagedDir(), 0o700); err != nil {
		return nil, err
	}
	rel := filepath.Join("Staged", randomName())
	path := filepath.Join(s.cfg.StateDir, rel)
	var f *os.File
	var err error
	if work.Valid {
		f, _, err = cloneFile(filepath.Join(s.cfg.StateDir, work.String), path)
	} else {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		os.Remove(path)
		return nil, err
	}
	if err := f.Chmod(0o444); err != nil {
		os.Remove(path)
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		os.Remove(path)
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	if err := syncDir(s.stagedDir()); err != nil {
		os.Remove(path)
		return nil, err
	}
	st := &staged{rel: rel, size: size}
	copy(st.digest[:], h.Sum(nil))
	return st, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Categories of operations, in the order they go when nothing else decides
const (
	orderDirCreate = iota
	orderMove
	orderFileCreate
	orderReplace
	orderFileDelete
	orderDirDelete
)

type plannedOp struct {
	c          *candidate
	kind       string
	itemKind   string
	order      int
	targetPar  sql.NullInt64
	targetName string
	after      []*plannedOp
	emitted    bool
	// An operation not yet sent that this one merges into
	merge sql.NullInt64
}

// plan writes the operations for the candidates: adopting replaced files, folding removed directories, merging into
// operations not sent yet, and ordering them so that each finds what it needs on the gateway. Returns the candidates
// whose working copies it used.
func (s *Service) plan(tx *sql.Tx, folder int64, cands []*candidate) ([]*candidate, error) {
	type location struct {
		parent int64
		name   string
	}
	byID := map[int64]*candidate{}
	for _, c := range cands {
		byID[c.id] = c
	}

	// A new file where a removed gateway file was (an editor's save: a temporary file renamed over it, or the file
	// renamed away to a backup and a new one written) replaces that file: it keeps its identity and history
	newAt := map[location]*candidate{}
	for _, c := range cands {
		if c.live() && c.kind == kindFile && c.local&localCreated != 0 && c.parent.Valid {
			sent, err := s.creationSent(tx, c.id)
			if err != nil {
				return nil, err
			}
			if !sent {
				newAt[location{c.parent.Int64, c.name}] = c
			}
		}
	}
	for _, t := range cands {
		if t.live() || t.kind != kindFile || !t.gateway.Valid || !t.baseParent.Valid {
			continue
		}
		n := newAt[location{t.baseParent.Int64, t.baseName.String}]
		if n == nil || n.gone {
			continue
		}
		if err := s.adopt(tx, t, n); err != nil {
			return nil, err
		}
		delete(newAt, location{t.baseParent.Int64, t.baseName.String})
		t.gone = true
	}

	// rm -r removes everything below a directory before the directory: its removal carries theirs
	for _, c := range cands {
		if c.gone || c.live() || c.kind != kindDir {
			continue
		}
		if c.parent.Valid {
			if p := byID[c.parent.Int64]; p != nil && !p.live() && p.kind == kindDir && p.gateway.Valid {
				continue
			}
		}
		res, err := tx.Exec(`WITH RECURSIVE sub(id) AS (SELECT id FROM items WHERE parent = ?1
				UNION ALL SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id)
			UPDATE items SET local = ?2 WHERE id IN (SELECT id FROM sub) AND deleted = 1`, c.id, localFolded)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			for _, d := range cands {
				if !d.live() && d.id != c.id && isBelow(byID, d, c.id) {
					d.gone = true
				}
			}
		}
	}

	var ops []*plannedOp
	byItem := map[int64][]*plannedOp{}
	add := func(op *plannedOp) {
		ops = append(ops, op)
		byItem[op.c.id] = append(byItem[op.c.id], op)
	}
	for _, c := range cands {
		if c.gone {
			continue
		}
		hasGateway := c.gateway.Valid
		if !hasGateway {
			sent, err := s.creationSent(tx, c.id)
			if err != nil {
				return nil, err
			}
			hasGateway = sent
		}
		switch {
		case !c.live():
			if !hasGateway {
				continue
			}
			order := orderFileDelete
			if c.kind == kindDir {
				order = orderDirDelete
			}
			add(&plannedOp{c: c, kind: remoteapi.OpDelete, order: order})
		case c.local&localCreated != 0:
			queued, err := s.queuedOp(tx, c.id, remoteapi.OpCreate)
			if err != nil {
				return nil, err
			}
			op := &plannedOp{c: c, kind: remoteapi.OpCreate, targetPar: c.parent, targetName: c.name, merge: queued}
			if c.kind == kindDir {
				op.itemKind, op.order = remoteapi.KindDirectory, orderDirCreate
			} else {
				op.itemKind, op.order = remoteapi.KindFile, orderFileCreate
			}
			add(op)
		default:
			if c.local&localMoved != 0 {
				moved := !c.baseParent.Valid || c.parent.Int64 != c.baseParent.Int64 || c.name != c.baseName.String
				pending, err := hasUnresolved(tx, c.id)
				if err != nil {
					return nil, err
				}
				if moved || pending {
					queued, err := s.queuedOp(tx, c.id, remoteapi.OpMove)
					if err != nil {
						return nil, err
					}
					add(&plannedOp{c: c, kind: remoteapi.OpMove, order: orderMove, targetPar: c.parent,
						targetName: c.name, merge: queued})
				}
			}
			if c.local&localContent != 0 && c.kind == kindFile {
				queued, err := s.queuedContentOp(tx, c.id)
				if err != nil {
					return nil, err
				}
				add(&plannedOp{c: c, kind: remoteapi.OpReplace, order: orderReplace, merge: queued})
			}
		}
	}

	// What each operation needs first
	deleteAt := map[location]*plannedOp{}
	createOf := map[int64]*plannedOp{}
	for _, op := range ops {
		if op.kind == remoteapi.OpDelete && op.c.baseParent.Valid {
			deleteAt[location{op.c.baseParent.Int64, op.c.baseName.String}] = op
		}
		if op.kind == remoteapi.OpCreate && op.itemKind == remoteapi.KindDirectory {
			createOf[op.c.id] = op
		}
	}
	for _, op := range ops {
		if op.targetPar.Valid {
			if p := createOf[op.targetPar.Int64]; p != nil && p != op {
				op.after = append(op.after, p)
			}
			if d := deleteAt[location{op.targetPar.Int64, op.targetName}]; d != nil && d.c != op.c {
				// The name is freed first
				op.after = append(op.after, d)
			}
		}
		if op.kind == remoteapi.OpDelete && op.c.kind == kindDir {
			// What moves out of the directory goes before the directory does
			for _, other := range ops {
				if other.kind == remoteapi.OpMove && other.c.baseParent.Valid &&
					wasBelow(byID, other.c, op.c.id) {
					op.after = append(op.after, other)
				}
			}
		}
		// Operations on one item stay in the order they are listed
		if list := byItem[op.c.id]; len(list) > 1 {
			for _, prior := range list {
				if prior == op {
					break
				}
				op.after = append(op.after, prior)
			}
		}
	}
	ordered := orderOps(ops)

	for _, op := range ordered {
		if err := s.writeOp(tx, folder, op); err != nil {
			return nil, err
		}
	}
	var consumed []*candidate
	for _, c := range cands {
		if c.gone {
			continue
		}
		var content sql.NullString
		if c.staged != nil && c.live() {
			content = sql.NullString{String: c.staged.rel, Valid: true}
		}
		// Settled: what it shows now comes from its operations until the gateway has committed them
		if _, err := tx.Exec(`UPDATE items SET local = local & ?, local_content = COALESCE(?, local_content),
			base_revision = NULL, base_content_id = NULL, base_subtree = NULL WHERE id = ?`, localOnly, content,
			c.id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM work WHERE node = ?`, c.node); err != nil {
			return nil, err
		}
		consumed = append(consumed, c)
	}
	return consumed, nil
}

// isBelow reports whether a candidate is below the item dir, by its current parents
func isBelow(byID map[int64]*candidate, c *candidate, dir int64) bool {
	for p := c.parent; p.Valid; {
		if p.Int64 == dir {
			return true
		}
		parent := byID[p.Int64]
		if parent == nil {
			return false
		}
		p = parent.parent
	}
	return false
}

// wasBelow reports whether a candidate was below the item dir on the gateway, by its base parents
func wasBelow(byID map[int64]*candidate, c *candidate, dir int64) bool {
	for p := c.baseParent; p.Valid; {
		if p.Int64 == dir {
			return true
		}
		parent := byID[p.Int64]
		if parent == nil {
			return false
		}
		p = parent.baseParent
	}
	return false
}

// orderOps orders operations so that each comes after what it needs, otherwise by category and depth
func orderOps(ops []*plannedOp) []*plannedOp {
	less := func(a, b *plannedOp) bool {
		if a.order != b.order {
			return a.order < b.order
		}
		if a.order == orderDirDelete || a.order == orderFileDelete {
			return a.c.depth > b.c.depth
		}
		return a.c.depth < b.c.depth
	}
	var result []*plannedOp
	for len(result) < len(ops) {
		var next *plannedOp
		for _, op := range ops {
			if op.emitted {
				continue
			}
			ready := !slices.ContainsFunc(op.after, func(a *plannedOp) bool { return !a.emitted })
			if ready && (next == nil || less(op, next)) {
				next = op
			}
		}
		if next == nil {
			// A cycle: what is left goes in category order, and the gateway sorts out what it can
			for _, op := range ops {
				if !op.emitted && (next == nil || less(op, next)) {
					next = op
				}
			}
		}
		next.emitted = true
		result = append(result, next)
	}
	return result
}

// adopt makes the new file n take over the gateway item of the removed file t, whose place it took
func (s *Service) adopt(tx *sql.Tx, t, n *candidate) error {
	if _, err := tx.Exec(`UPDATE remote_items SET item = ? WHERE gateway_id = ?`, n.id, t.gateway.String); err != nil {
		return err
	}
	// The edits of the removed file that are still unresolved continue as the new file's
	if _, err := tx.Exec(`UPDATE outbox SET item = ? WHERE item = ? AND state IN `+unresolvedStates, n.id,
		t.id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET local = (local & ~?) | ?, base_revision = ?, base_content_id = ?
		WHERE id = ?`, localCreated, localContent, t.baseRev, t.baseContent, n.id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, t.id); err != nil {
		return err
	}
	n.local = (n.local &^ localCreated) | localContent
	n.gateway, n.baseRev, n.baseContent = t.gateway, t.baseRev, t.baseContent
	n.baseParent, n.baseName = t.baseParent, t.baseName
	return nil
}

// queuedOp is an operation of the item of this kind that wasn't sent yet, which a new change merges into
func (s *Service) queuedOp(tx *sql.Tx, item int64, kind string) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := tx.QueryRow(`SELECT id FROM outbox WHERE item = ? AND kind = ? AND state IN ('queued', 'uploaded')
		AND id = (SELECT MAX(id) FROM outbox WHERE item = ? AND state IN `+unresolvedStates+`)`, item, kind, item).
		Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, nil
	}
	return id, err
}

// queuedContentOp is the item's last unsent operation that sends content (a create or a replace)
func (s *Service) queuedContentOp(tx *sql.Tx, item int64) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := tx.QueryRow(`SELECT id FROM outbox WHERE item = ? AND kind IN ('create', 'replace')
		AND state IN ('queued', 'uploaded')
		AND id = (SELECT MAX(id) FROM outbox WHERE item = ? AND state IN `+unresolvedStates+`)`, item, item).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, nil
	}
	return id, err
}

// cancelQueued cancels an item's operations that weren't sent (their staged content goes with the next collection)
func (s *Service) cancelQueued(tx *sql.Tx, item int64) error {
	_, err := tx.Exec(`UPDATE outbox SET state = 'canceled', updated_at = ? WHERE item = ?
		AND state IN ('queued', 'uploaded')`, nowUnix(), item)
	return err
}

// writeOp records a planned operation, or merges it into the unsent one it continues
func (s *Service) writeOp(tx *sql.Tx, folder int64, op *plannedOp) error {
	c := op.c
	var staged sql.NullString
	var digest string
	var size int64
	var modNs sql.NullInt64
	if c.staged != nil && (op.kind == remoteapi.OpCreate || op.kind == remoteapi.OpReplace) {
		staged = sql.NullString{String: c.staged.rel, Valid: true}
		digest, size = remoteapi.B64.EncodeToString(c.staged.digest[:]), c.staged.size
		modNs = sql.NullInt64{Int64: c.modNs, Valid: true}
	}
	if op.merge.Valid {
		if staged.Valid {
			// New content: the upload starts over; the operation keeps its place and ID (it wasn't sent)
			if _, err := tx.Exec(`UPDATE outbox SET staged = ?, digest = ?, size = ?, mod_ns = ?, upload_id = '',
				state = 'queued', updated_at = ? WHERE id = ?`, staged, digest, size, modNs, nowUnix(),
				op.merge.Int64); err != nil {
				return err
			}
		}
		if op.targetPar.Valid {
			_, err := tx.Exec(`UPDATE outbox SET target_parent = ?, target_name = ?, updated_at = ? WHERE id = ?`,
				op.targetPar, op.targetName, nowUnix(), op.merge.Int64)
			return err
		}
		return nil
	}
	// The first change of an item is based on what was captured of it; a later one on the result of the operation
	// before it
	pending, err := hasUnresolved(tx, c.id)
	if err != nil {
		return err
	}
	baseRev, baseContent := c.baseRev.String, c.baseContent.String
	if pending {
		baseRev, baseContent = "", ""
	}
	var subtree sql.NullInt64
	if op.kind == remoteapi.OpDelete && c.kind == kindDir {
		subtree = c.baseSubtree
	}
	_, err = tx.Exec(`INSERT INTO outbox (op_id, folder, item, kind, item_kind, state, base_revision, base_content_id,
		subtree_generation, target_parent, target_name, staged, digest, size, mod_ns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), folder, c.id, op.kind,
		op.itemKind, baseRev, baseContent, subtree, op.targetPar, op.targetName, staged, digest, size, modNs,
		nowUnix(), nowUnix())
	return err
}
