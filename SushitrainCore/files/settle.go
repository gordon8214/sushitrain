// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
		if s.ctx.Err() != nil {
			return
		}
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
			// A handle left on a file that was replaced writes to nothing that is sent: it holds nothing back
			c.busy = wc.writers > 0 && !wc.detached
			c.version = wc.version
			wc.mutex.Unlock()
		}
		busy = busy || c.busy
	}
	if now-newest < s.cfg.SettleDelay.Milliseconds() {
		// A file changed again and again (a log, a database) holds back the folder's other changes only for a while;
		// then those that are quiet are settled without it
		s.mutex.Lock()
		since, ok := s.heldSince[folder]
		if !ok {
			s.heldSince[folder] = time.Now()
			since = time.Now()
		}
		s.mutex.Unlock()
		if time.Since(since) < settleMaxWait {
			s.settleLater(folder)
			return nil
		}
		var quiet []*candidate
		for _, c := range cands {
			if now-c.changedAt >= s.cfg.SettleDelay.Milliseconds() {
				quiet = append(quiet, c)
			}
		}
		if len(quiet) == 0 {
			s.settleLater(folder)
			return nil
		}
		cands = quiet
		defer s.settleLater(folder)
	}
	s.mutex.Lock()
	delete(s.heldSince, folder)
	s.mutex.Unlock()
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
	staging := cands[:0:0]
	for _, c := range cands {
		if !c.live() || c.kind != kindFile || c.local&(localCreated|localContent) == 0 {
			staging = append(staging, c)
			continue
		}
		st, err := s.stage(c.work)
		if err != nil {
			// That file's change waits (or, with its working copy gone, is given up); the others go on
			s.unstageable(c, err)
			continue
		}
		stagedFiles = append(stagedFiles, filepath.Join(s.cfg.StateDir, st.rel))
		c.staged = st
		staging = append(staging, c)
	}
	cands = staging
	if err := s.hook("settle.staged"); err != nil {
		return err
	}

	// The candidates' working copies are held from the check that nothing writes them to their retirement, so that
	// a handle opened meanwhile gets a working copy of its own rather than one about to be removed. Working copies
	// are locked before the database, as everywhere.
	var held []*workingCopy
	s.mutex.Lock()
	for _, c := range cands {
		if wc := s.work[uint64(c.node)]; wc != nil {
			held = append(held, wc)
		}
	}
	s.mutex.Unlock()
	sort.Slice(held, func(i, j int) bool { return held[i].node < held[j].node })
	for _, wc := range held {
		wc.mutex.Lock()
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			for _, wc := range held {
				wc.mutex.Unlock()
			}
		}
	}
	defer unlock()
	versions := map[uint64]*workingCopy{}
	for _, wc := range held {
		versions[wc.node] = wc
	}

	var consumed []*candidate
	err = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		for _, c := range cands {
			var changedAt int64
			err := tx.QueryRow(`SELECT changed_at FROM items WHERE id = ?`, c.id).Scan(&changedAt)
			if err != nil || changedAt != c.changedAt {
				return errUnsettled
			}
			if wc := versions[uint64(c.node)]; wc != nil && !wc.detached && (wc.version != c.version || wc.writers > 0) {
				return errUnsettled
			}
			// Nor may the gateway have mapped it meanwhile (its creation committed while it was staged)
			var gateway sql.NullString
			if err := tx.QueryRow(`SELECT gateway_id FROM remote_items WHERE item = ?`, c.id).Scan(&gateway); err != nil &&
				!errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if gateway != c.gateway {
				return errUnsettled
			}
		}
		var err error
		consumed, err = s.plan(tx, folder, cands)
		return err
	})
	if err != nil {
		return err
	}
	// For tests: a handle opened from here on waits for the working copies to be retired, and takes new ones
	_ = s.hook("settle.committed")
	// The staged files are referred to now; the working copies they were made from are not
	stagedFiles = nil
	var retired []*workingCopy
	for _, c := range consumed {
		if c.work.Valid {
			os.Remove(filepath.Join(s.cfg.StateDir, c.work.String))
		}
		if wc := versions[uint64(c.node)]; wc != nil {
			if wc.file != nil {
				wc.file.Close()
				wc.file = nil
			}
			wc.retired = true
			retired = append(retired, wc)
		}
	}
	unlock()
	s.mutex.Lock()
	for _, wc := range retired {
		if s.work[wc.node] == wc {
			delete(s.work, wc.node)
		}
	}
	s.mutex.Unlock()
	s.wakeOutbox()
	return nil
}

// unstageable deals with a change whose content can't be staged: it waits, unless its working copy is gone (as
// after a power loss right after it was made), when the change is given up and the item shows its content before it
func (s *Service) unstageable(c *candidate, err error) {
	if !errors.Is(err, fs.ErrNotExist) || !c.work.Valid {
		s.log.Warn("Could not stage a change; it waits", "item", c.id, "error", err)
		return
	}
	s.log.Warn("A change was lost: the file it was written to is gone", "item", c.id, "file", c.work.String)
	_ = s.store.tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM work WHERE node = ? AND file = ?`, c.node,
			strings.TrimPrefix(c.work.String, "Work/")); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE items SET local = local & ~?, local_content = NULL, changed_at = ? WHERE id = ?
			AND local_content = ?`, localContent, nowMillis(), c.id, c.work.String)
		return err
	})
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
		err := s.adopt(tx, t, n)
		if errors.Is(err, errNoAdoption) {
			continue
		}
		if err != nil {
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
		// Only what was below it on the gateway: something moved into it here and removed with it was elsewhere
		// there, and needs a removal of its own
		rows, err := tx.Query(`WITH RECURSIVE below(id) AS (SELECT gateway_id FROM remote_items WHERE item = ?1
				UNION ALL SELECT r.gateway_id FROM remote_items AS r JOIN below ON r.parent_gateway_id = below.id)
			SELECT i.id FROM items AS i JOIN remote_items AS r ON r.item = i.id
			WHERE r.gateway_id IN (SELECT id FROM below) AND i.id != ?1 AND i.deleted = 1`, c.id)
		if err != nil {
			return nil, err
		}
		folded := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			folded[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for id := range folded {
			if _, err := tx.Exec(`UPDATE items SET local = ? WHERE id = ?`, localFolded, id); err != nil {
				return nil, err
			}
			// What was still to be sent of it goes with it, rather than change what the directory's removal finds
			if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', updated_at = ? WHERE item = ?
				AND state IN ('queued', 'uploaded')`, nowUnix(), id); err != nil {
				return nil, err
			}
		}
		for _, d := range cands {
			if !d.live() && folded[d.id] {
				d.gone = true
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
	moveFrom := map[location]*plannedOp{}
	createOf := map[int64]*plannedOp{}
	dirDelete := map[int64]*plannedOp{}
	for _, op := range ops {
		if op.kind == remoteapi.OpDelete && op.c.baseParent.Valid {
			deleteAt[location{op.c.baseParent.Int64, op.c.baseName.String}] = op
		}
		if op.kind == remoteapi.OpMove && op.c.baseParent.Valid {
			moveFrom[location{op.c.baseParent.Int64, op.c.baseName.String}] = op
		}
		if op.kind == remoteapi.OpCreate && op.itemKind == remoteapi.KindDirectory {
			createOf[op.c.id] = op
		}
		if op.kind == remoteapi.OpDelete && op.c.kind == kindDir {
			dirDelete[op.c.id] = op
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
			if m := moveFrom[location{op.targetPar.Int64, op.targetName}]; m != nil && m.c != op.c {
				// So is a name another item moves away from (log rotation: mv log.1 log.2; mv log log.1)
				op.after = append(op.after, m)
			}
		}
		if op.kind == remoteapi.OpMove {
			// What moves out of a directory goes before the directory does
			for _, dir := range basedBelow(byID, op.c) {
				if d := dirDelete[dir]; d != nil {
					d.after = append(d.after, op)
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

// basedBelow returns the directories a candidate was below on the gateway, by its base parents, as far as the
// candidates know them
func basedBelow(byID map[int64]*candidate, c *candidate) []int64 {
	var dirs []int64
	for p := c.baseParent; p.Valid && len(dirs) <= len(byID); {
		dirs = append(dirs, p.Int64)
		parent := byID[p.Int64]
		if parent == nil {
			break
		}
		p = parent.baseParent
	}
	return dirs
}

// orderOps orders operations so that each comes after what it needs, otherwise by category and depth, then as listed:
// of the operations whose needs are met, the first by those goes next. A tar x or git clone settles tens of thousands
// at once, inside the transaction, so it is a topological sort over a heap.
func orderOps(ops []*plannedOp) []*plannedOp {
	less := func(i, j int) bool {
		a, b := ops[i], ops[j]
		if a.order != b.order {
			return a.order < b.order
		}
		if a.c.depth != b.c.depth {
			if a.order == orderDirDelete || a.order == orderFileDelete {
				return a.c.depth > b.c.depth
			}
			return a.c.depth < b.c.depth
		}
		return i < j
	}
	at := make(map[*plannedOp]int, len(ops))
	for i, op := range ops {
		at[op] = i
	}
	waiting := make([]int, len(ops))
	dependents := make([][]int, len(ops))
	for i, op := range ops {
		for _, a := range op.after {
			// One that isn't planned is never met: only a cycle's way out emits what waits for it
			waiting[i]++
			if j, ok := at[a]; ok {
				dependents[j] = append(dependents[j], i)
			}
		}
	}
	ready := &opHeap{less: less}
	for i := range ops {
		if waiting[i] == 0 {
			ready.items = append(ready.items, i)
		}
	}
	heap.Init(ready)
	// For cycles: every operation in that order, and the first not emitted yet
	all := make([]int, len(ops))
	for i := range all {
		all[i] = i
	}
	slices.SortFunc(all, func(i, j int) int {
		if less(i, j) {
			return -1
		}
		return 1
	})
	first := 0

	emitted := make([]bool, len(ops))
	result := make([]*plannedOp, 0, len(ops))
	for len(result) < len(ops) {
		var next int
		if ready.Len() > 0 {
			next = heap.Pop(ready).(int)
			if emitted[next] {
				continue
			}
		} else {
			// A cycle: what is left goes in category order, and the gateway sorts out what it can
			for emitted[all[first]] {
				first++
			}
			next = all[first]
		}
		emitted[next] = true
		result = append(result, ops[next])
		for _, d := range dependents[next] {
			if waiting[d]--; waiting[d] == 0 && !emitted[d] {
				heap.Push(ready, d)
			}
		}
	}
	return result
}

// opHeap holds the indices of operations whose needs are met, the first by less on top
type opHeap struct {
	items []int
	less  func(i, j int) bool
}

func (h *opHeap) Len() int           { return len(h.items) }
func (h *opHeap) Less(i, j int) bool { return h.less(h.items[i], h.items[j]) }
func (h *opHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *opHeap) Push(x any)         { h.items = append(h.items, x.(int)) }

func (h *opHeap) Pop() any {
	n := len(h.items) - 1
	x := h.items[n]
	h.items = h.items[:n]
	return x
}

// errNoAdoption means a new file can't take over the gateway item of the one it replaced
var errNoAdoption = errors.New("the removed file's gateway item is being moved")

// adopt makes the new file n take over the gateway item of the removed file t, whose place it took
func (s *Service) adopt(tx *sql.Tx, t, n *candidate) error {
	// A move of the removed file that is on its way, or waits for review, takes the gateway item elsewhere: the new
	// file can't take its place. One not sent yet is given up, as it would move the new file.
	var moving bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM outbox WHERE item = ? AND kind = 'move'
		AND state IN ('submitted', 'pending', 'failed', 'unknown', 'blocked'))`, t.id).Scan(&moving); err != nil {
		return err
	}
	if moving {
		return errNoAdoption
	}
	if _, err := tx.Exec(`UPDATE outbox SET state = 'canceled', updated_at = ? WHERE item = ? AND kind = 'move'
		AND state IN ('queued', 'uploaded')`, nowUnix(), t.id); err != nil {
		return err
	}
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
