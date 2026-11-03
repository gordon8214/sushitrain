// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// Item kinds in items
const (
	kindFile   = 0
	kindDir    = 1
	kindRoot   = 3
	kindHidden = -1
)

// Changes is what a transaction changed in the mount, for invalidating the kernel's caches once it has committed
type Changes struct {
	// Names in directories that resolve differently now: added, removed, renamed or given a new node
	Entries []EntryChange
	// Nodes whose attributes (size, modification time) changed without a new node
	Attrs []uint64
}

// EntryChange is a name in a directory (by the directory's node) that changed. Node is the node it resolved to
// before, if any. Gone says that node is gone, rather than moved or showing other content under another node: the
// kernel then treats it as removed.
type EntryChange struct {
	Parent uint64
	Name   string
	Node   uint64
	Gone   bool
}

func (c *Changes) entry(parent uint64, name string, node uint64) {
	c.Entries = append(c.Entries, EntryChange{Parent: parent, Name: name, Node: node})
}

// removed records a name whose item is gone
func (c *Changes) removed(parent uint64, name string, node uint64) {
	c.Entries = append(c.Entries, EntryChange{Parent: parent, Name: name, Node: node, Gone: true})
}

func (c *Changes) merge(other Changes) {
	c.Entries = append(c.Entries, other.Entries...)
	c.Attrs = append(c.Attrs, other.Attrs...)
}

// Empty reports whether nothing changed
func (c Changes) Empty() bool {
	return len(c.Entries) == 0 && len(c.Attrs) == 0
}

// projector turns gateway metadata (remote_items) into the items the mount shows, within one transaction, as the
// Swift RemoteProjector does:
//
//   - A gateway item keeps its row through renames and moves; a new gateway item gets a new row. A removed row is
//     revived only by its gateway ID.
//   - An item whose parent isn't shown yet, or whose name another item has that can't be displaced, waits
//     (placed = 0) and is placed when that changes.
//   - A row whose content changes gets a new node.
//   - An item that moved to another folder of the same gateway keeps its row: the destination's feed moves it there,
//     and the source's tombstone for the move (movedToFolderID) leaves it alone while this client reads that folder.
type projector struct {
	tx           *sql.Tx
	changes      Changes
	touched      map[int64]bool
	sessionMarks map[int64]int64
	readsFolder  map[string]bool
	// Folders whose local changes need settling again (a directory they need was removed on the gateway)
	resettle map[int64]bool
}

func newProjector(tx *sql.Tx) *projector {
	return &projector{tx: tx, touched: map[int64]bool{}, sessionMarks: map[int64]int64{},
		readsFolder: map[string]bool{}, resettle: map[int64]bool{}}
}

type remoteRow struct {
	gatewayID string
	item      sql.NullInt64
	folder    int64
	parent    sql.NullString
	name      string
	kind      string
	revision  string
	contentID sql.NullString
	size      int64
	modNs     int64
	deleted   bool
}

func (p *projector) remoteRow(gatewayID string) (*remoteRow, error) {
	var r remoteRow
	err := p.tx.QueryRow(`SELECT gateway_id, item, folder, parent_gateway_id, name, kind, revision, content_id, size,
		modified_ns, deleted FROM remote_items WHERE gateway_id = ?`, gatewayID).Scan(&r.gatewayID, &r.item, &r.folder,
		&r.parent, &r.name, &r.kind, &r.revision, &r.contentID, &r.size, &r.modNs, &r.deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

type itemRow struct {
	id        int64
	folder    int64
	parent    sql.NullInt64
	name      string
	kind      int
	size      int64
	modNs     int64
	contentID sql.NullString
	node      int64
	deleted   bool
}

const itemColumns = `id, folder, parent, name, kind, size, mod_ns, content_id, node, deleted`

func scanItem(row interface{ Scan(...any) error }) (*itemRow, error) {
	var r itemRow
	if err := row.Scan(&r.id, &r.folder, &r.parent, &r.name, &r.kind, &r.size, &r.modNs, &r.contentID, &r.node,
		&r.deleted); err != nil {
		return nil, err
	}
	return &r, nil
}

func (p *projector) itemRow(id int64) (*itemRow, error) {
	r, err := scanItem(p.tx.QueryRow(`SELECT `+itemColumns+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// nodeOf is the node of an item, or the root's for no item
func (p *projector) nodeOf(item sql.NullInt64) (uint64, error) {
	if !item.Valid {
		return rootNode, nil
	}
	var node int64
	err := p.tx.QueryRow(`SELECT node FROM items WHERE id = ?`, item.Int64).Scan(&node)
	return uint64(node), err
}

// overlaid is the condition on an item (as i) that it has changes here the gateway hasn't committed, which its row
// shows instead of the gateway's metadata: changes not settled yet, operations not resolved, or a working copy
const overlaid = `(i.local != 0 OR EXISTS (SELECT 1 FROM outbox AS o WHERE o.item = i.id AND o.state IN ` +
	unresolvedStates + `) OR EXISTS (SELECT 1 FROM work AS w WHERE w.item = i.id AND w.detached = 0))`

func (p *projector) hasOverlay(item int64) (bool, error) {
	var result bool
	err := p.tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM items AS i WHERE i.id = ? AND `+overlaid+`)`, item).Scan(&result)
	return result, err
}

// subtreeHasOverlay reports whether something below an item has changes here
func (p *projector) subtreeHasOverlay(item int64) (bool, error) {
	var result bool
	err := p.tx.QueryRow(`WITH RECURSIVE sub(id) AS (SELECT id FROM items WHERE parent = ?1
			UNION ALL SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id)
		SELECT EXISTS (SELECT 1 FROM items AS i JOIN sub ON sub.id = i.id WHERE `+overlaid+`)`, item).Scan(&result)
	return result, err
}

// mapItem records that a gateway item is what an item created here became (the result of its creation). It shows
// the gateway's metadata once nothing of it is pending.
func (p *projector) mapItem(item remoteapi.Item, folder, local int64) error {
	var existing sql.NullInt64
	err := p.tx.QueryRow(`SELECT item FROM remote_items WHERE gateway_id = ?`, item.ID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing.Valid && existing.Int64 != local {
		// The change feed came first and showed it as a new item: that row goes
		row, err := p.itemRow(existing.Int64)
		if err != nil {
			return err
		}
		if row != nil && !row.deleted {
			if err := p.removeRow(row, false); err != nil {
				return err
			}
		}
	}
	if _, err := p.tx.Exec(`UPDATE remote_items SET item = NULL WHERE item = ? AND gateway_id != ?`, local,
		item.ID); err != nil {
		return err
	}
	var parent, contentID sql.NullString
	if item.ParentID != nil {
		parent = sql.NullString{String: *item.ParentID, Valid: true}
	}
	if item.ContentID != "" {
		contentID = sql.NullString{String: item.ContentID, Valid: true}
	}
	mark, err := p.sessionMark(folder)
	if err != nil {
		return err
	}
	_, err = p.tx.Exec(`INSERT INTO remote_items (gateway_id, folder, item, parent_gateway_id, name, kind, revision,
		content_id, size, modified_ns, subtree_generation, deleted, seen_mark, placed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 1)
		ON CONFLICT (gateway_id) DO UPDATE SET item = excluded.item, placed = 1`, item.ID, folder, local, parent,
		item.Name, item.Kind, item.Revision, contentID, item.Size, item.ModifiedNs, item.SubtreeGeneration, mark)
	return err
}

// sessionMark is the mark of the folder's snapshot in progress (0 without one): items stored outside the snapshot
// while it runs count as seen
func (p *projector) sessionMark(folder int64) (int64, error) {
	if mark, ok := p.sessionMarks[folder]; ok {
		return mark, nil
	}
	var mark int64
	err := p.tx.QueryRow(`SELECT mark FROM snapshots WHERE folder = ?`, folder).Scan(&mark)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	p.sessionMarks[folder] = mark
	return mark, nil
}

// apply stores an item's metadata from the gateway and projects it. mark is the snapshot mark of the session it was
// seen in (0 outside a snapshot). An item from outside the change feed (an operation's result, the current item of a
// rejection) passes onlyOver, the revisions the client knew of it: it is stored only when nothing is stored of it, or
// one of those is, since the feed may have stored a newer state already. Returns whether the item was stored.
func (p *projector) apply(item remoteapi.Item, folder int64, mark int64, onlyOver []string) (bool, error) {
	previous, err := p.remoteRow(item.ID)
	if err != nil {
		return false, err
	}
	if onlyOver != nil && previous != nil && !slices.Contains(onlyOver, previous.revision) {
		return false, nil
	}
	if item.Deleted && item.MovedToFolderID != "" {
		setAside, err := p.setsAsideMove(item, folder, previous, mark)
		if err != nil || setAside {
			return false, err
		}
	}
	if mark == 0 {
		if mark, err = p.sessionMark(folder); err != nil {
			return false, err
		}
	}
	var parent, contentID sql.NullString
	if item.ParentID != nil {
		parent = sql.NullString{String: *item.ParentID, Valid: true}
	}
	if item.ContentID != "" {
		contentID = sql.NullString{String: item.ContentID, Valid: true}
	}
	var subtree sql.NullInt64
	if item.Kind == remoteapi.KindDirectory {
		subtree = sql.NullInt64{Int64: item.SubtreeGeneration, Valid: true}
	}
	if previous != nil {
		_, err = p.tx.Exec(`UPDATE remote_items SET folder = ?, parent_gateway_id = ?, name = ?, kind = ?, revision = ?,
			content_id = ?, size = ?, modified_ns = ?, subtree_generation = ?, deleted = ?,
			seen_mark = CASE WHEN ? > 0 THEN ? ELSE seen_mark END
			WHERE gateway_id = ?`, folder, parent, item.Name, item.Kind, item.Revision, contentID, item.Size,
			item.ModifiedNs, subtree, item.Deleted, mark, mark, item.ID)
	} else {
		_, err = p.tx.Exec(`INSERT INTO remote_items (gateway_id, folder, parent_gateway_id, name, kind, revision,
			content_id, size, modified_ns, subtree_generation, deleted, seen_mark, placed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`, item.ID, folder, parent, item.Name, item.Kind,
			item.Revision, contentID, item.Size, item.ModifiedNs, subtree, item.Deleted, mark)
	}
	if err != nil {
		return false, err
	}
	if item.ParentID == nil && !item.Deleted {
		if _, err := p.tx.Exec(`UPDATE folders SET root_gateway_id = ? WHERE id = ?`, item.ID, folder); err != nil {
			return false, err
		}
	}
	if _, err := p.place(item.ID, true); err != nil {
		return false, err
	}
	if previous != nil && previous.folder != folder && !item.Deleted && item.Kind == remoteapi.KindDirectory {
		if err := p.moveMetadata(item.ID, folder); err != nil {
			return false, err
		}
	}
	return true, nil
}

// setsAsideMove reports whether a tombstone of an item that moved to another folder of the same gateway is set aside
// rather than stored: when this client reads the destination folder, whose feed moves the item (keeping its row),
// and when the item is in another folder already (that feed came first)
func (p *projector) setsAsideMove(item remoteapi.Item, folder int64, previous *remoteRow, mark int64) (bool, error) {
	if previous != nil && previous.folder != folder {
		return true, nil
	}
	reads, err := p.readsDestination(folder, item.MovedToFolderID)
	if err != nil || !reads {
		return false, err
	}
	if mark == 0 {
		if mark, err = p.sessionMark(folder); err != nil {
			return false, err
		}
	}
	if previous != nil && mark > 0 {
		// Seen by this folder's snapshot, which must not remove it before the destination's feed moves it
		if _, err := p.tx.Exec(`UPDATE remote_items SET seen_mark = ? WHERE gateway_id = ?`, mark, item.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// readsDestination reports whether this client has the folder remoteID of the gateway that serves folder, readable
func (p *projector) readsDestination(folder int64, remoteID string) (bool, error) {
	key := strconv.FormatInt(folder, 10) + "\x00" + remoteID
	if reads, ok := p.readsFolder[key]; ok {
		return reads, nil
	}
	var reads bool
	err := p.tx.QueryRow(`SELECT d.can_read FROM folders AS f JOIN folders AS d ON d.service_id = f.service_id
		WHERE f.id = ? AND d.remote_id = ?`, folder, remoteID).Scan(&reads)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	p.readsFolder[key] = reads
	return reads, nil
}

// moveMetadata moves the gateway metadata and rows below a directory that moved to another folder along with it
func (p *projector) moveMetadata(gatewayID string, folder int64) error {
	r, err := p.remoteRow(gatewayID)
	if err != nil || r == nil || !r.item.Valid {
		return err
	}
	below := `WITH RECURSIVE sub(id) AS (
		SELECT id FROM items WHERE parent = ?1 AND deleted = 0
		UNION ALL SELECT i.id FROM items AS i JOIN sub ON i.parent = sub.id WHERE i.deleted = 0)`
	if _, err := p.tx.Exec(below+` UPDATE items SET folder = ?2 WHERE id IN (SELECT id FROM sub)`, r.item.Int64,
		folder); err != nil {
		return err
	}
	_, err = p.tx.Exec(below+` UPDATE remote_items SET folder = ?2 WHERE item IN (SELECT id FROM sub)`, r.item.Int64,
		folder)
	return err
}

func itemKind(kind string) int {
	switch kind {
	case remoteapi.KindFile:
		return kindFile
	case remoteapi.KindDirectory:
		return kindDir
	}
	// Symbolic links and kinds this client doesn't know are not shown
	return kindHidden
}

func (p *projector) setPlaced(gatewayID string, item sql.NullInt64) error {
	_, err := p.tx.Exec(`UPDATE remote_items SET item = ?, placed = 1 WHERE gateway_id = ?`, item, gatewayID)
	return err
}

func (p *projector) setWaiting(gatewayID string) error {
	_, err := p.tx.Exec(`UPDATE remote_items SET placed = 0 WHERE gateway_id = ?`, gatewayID)
	return err
}

// rootItem is the row of a folder's root, which keeps its row (named after the folder in the mount)
func (p *projector) rootItem(folder int64) (int64, error) {
	var id int64
	err := p.tx.QueryRow(`SELECT root_item FROM folders WHERE id = ?`, folder).Scan(&id)
	return id, err
}

// parentRow is the live row of a gateway item's parent, if it is shown
func (p *projector) parentRow(r *remoteRow) (*itemRow, error) {
	if !r.parent.Valid {
		return nil, nil
	}
	var id int64
	err := p.tx.QueryRow(`SELECT item FROM remote_items WHERE gateway_id = ? AND placed = 1 AND deleted = 0
		AND item IS NOT NULL`, r.parent.String).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parent, err := p.itemRow(id)
	if err != nil || parent == nil || parent.deleted || (parent.kind != kindDir && parent.kind != kindRoot) {
		return nil, err
	}
	return parent, nil
}

// place projects a gateway item's stored metadata into its row, and reports false when it has to wait
func (p *projector) place(gatewayID string, mayDisplace bool) (bool, error) {
	r, err := p.remoteRow(gatewayID)
	if err != nil || r == nil {
		return false, err
	}
	p.touched[r.folder] = true

	// The row of an item with a local change shows that change until its operation is resolved
	if r.item.Valid {
		overlaid, err := p.hasOverlay(r.item.Int64)
		if err != nil {
			return false, err
		}
		if overlaid {
			return true, p.setPlaced(gatewayID, r.item)
		}
	}
	if !r.parent.Valid {
		// The folder's root keeps the row it got when the folder was added
		if r.deleted {
			return true, nil
		}
		root, err := p.rootItem(r.folder)
		if err != nil {
			return false, err
		}
		if _, err := p.tx.Exec(`UPDATE remote_items SET item = NULL WHERE item = ? AND gateway_id != ?`, root,
			gatewayID); err != nil {
			return false, err
		}
		if _, err := p.tx.Exec(`UPDATE items SET mod_ns = ? WHERE id = ?`, r.modNs, root); err != nil {
			return false, err
		}
		return true, p.setPlaced(gatewayID, sql.NullInt64{Int64: root, Valid: true})
	}

	name := normalizedName(r.name)
	kind := itemKind(r.kind)
	// Gone, or not shown
	if r.deleted || kind == kindHidden || !shownName(name) {
		if r.item.Valid {
			row, err := p.itemRow(r.item.Int64)
			if err != nil {
				return false, err
			}
			if row != nil && !row.deleted {
				if err := p.removeRow(row, false); err != nil {
					return false, err
				}
			}
		}
		// Linked as removeRow left it: a directory kept for the changes in it is made again, as another item
		_, err := p.tx.Exec(`UPDATE remote_items SET placed = 1 WHERE gateway_id = ?`, gatewayID)
		return true, err
	}
	parent, err := p.parentRow(r)
	if err != nil {
		return false, err
	}
	if parent == nil {
		return false, p.setWaiting(gatewayID)
	}

	var current *itemRow
	if r.item.Valid {
		if current, err = p.itemRow(r.item.Int64); err != nil {
			return false, err
		}
	}
	if current != nil && current.kind != kind {
		// A change of kind: the old row goes, and the item is shown as a new one
		if !current.deleted {
			if err := p.removeRow(current, false); err != nil {
				return false, err
			}
		}
		if _, err := p.tx.Exec(`UPDATE remote_items SET item = NULL WHERE gateway_id = ?`, gatewayID); err != nil {
			return false, err
		}
		current = nil
	}

	// Another row where this one goes
	var excluded int64
	if current != nil {
		excluded = current.id
	}
	other, err := scanItem(p.tx.QueryRow(`SELECT `+itemColumns+` FROM items WHERE parent = ? AND name = ? AND deleted = 0
		AND id != ?`, parent.id, name, excluded))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if other != nil {
		overlaid, err := p.hasOverlay(other.id)
		if err == nil && !overlaid {
			overlaid, err = p.subtreeHasOverlay(other.id)
		}
		if err != nil {
			return false, err
		}
		if overlaid || !mayDisplace {
			// A local change is there, or two items would take each other's place in turn: wait
			return false, p.setWaiting(gatewayID)
		}
		// The other row is stale: its item moved away or is gone, and the feed hasn't said so yet
		if err := p.removeRow(other, true); err != nil {
			return false, err
		}
	}

	var contentID sql.NullString
	var size int64
	if kind == kindFile {
		contentID, size = r.contentID, r.size
	}
	if current != nil {
		moved := current.parent.Int64 != parent.id || current.name != name
		contentChanged := kind == kindFile && current.contentID != contentID
		attrsChanged := current.size != size || current.modNs != r.modNs
		if current.deleted || moved || contentChanged || attrsChanged || current.folder != parent.folder {
			node := current.node
			if contentChanged {
				// A new node, so that handles on the old content keep their own pages
				if node, err = allocate(p.tx, metaNextNode); err != nil {
					return false, err
				}
			}
			if _, err := p.tx.Exec(`UPDATE items SET folder = ?, parent = ?, name = ?, size = ?, mod_ns = ?,
				content_id = ?, node = ?, deleted = 0, deleted_at = NULL WHERE id = ?`, parent.folder, parent.id, name,
				size, r.modNs, contentID, node, current.id); err != nil {
				return false, err
			}
			switch {
			case current.deleted:
				p.changes.entry(uint64(parent.node), name, 0)
			case moved:
				oldParent, err := p.nodeOf(current.parent)
				if err != nil {
					return false, err
				}
				p.changes.entry(oldParent, current.name, uint64(current.node))
				p.changes.entry(uint64(parent.node), name, 0)
			case node != current.node:
				p.changes.entry(uint64(parent.node), name, uint64(current.node))
			default:
				p.changes.Attrs = append(p.changes.Attrs, uint64(node))
			}
		}
		return true, p.setPlaced(gatewayID, sql.NullInt64{Int64: current.id, Valid: true})
	}

	node, err := allocate(p.tx, metaNextNode)
	if err != nil {
		return false, err
	}
	res, err := p.tx.Exec(`INSERT INTO items (folder, parent, name, kind, size, mod_ns, content_id, node)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, parent.folder, parent.id, name, kind, size, r.modNs, contentID, node)
	if err != nil {
		return false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	p.changes.entry(uint64(parent.node), name, 0)
	return true, p.setPlaced(gatewayID, sql.NullInt64{Int64: id, Valid: true})
}

// removeRow removes a row and the rows below it, except those with changes here, which keep the directories above
// them. With unplace, their gateway items wait to be placed again (they were displaced, not deleted).
func (p *projector) removeRow(row *itemRow, unplace bool) error {
	now := nowUnix()
	if kept, err := p.hasOverlay(row.id); err != nil || kept {
		return err
	}
	if row.kind == kindDir || row.kind == kindRoot {
		// What has changes here stays, with the directories it is in
		keep := `WITH RECURSIVE sub(id, parent) AS (
				SELECT id, parent FROM items WHERE parent = ?1
				UNION ALL SELECT i.id, i.parent FROM items AS i JOIN sub ON i.parent = sub.id),
			changed(id) AS (SELECT sub.id FROM sub JOIN items AS i ON i.id = sub.id WHERE ` + overlaid + `),
			kept(id) AS (SELECT id FROM changed
				UNION SELECT sub.parent FROM sub JOIN kept ON kept.id = sub.id WHERE sub.parent != ?1)`
		var keeps bool
		if err := p.tx.QueryRow(keep+` SELECT EXISTS (SELECT 1 FROM kept)`, row.id).Scan(&keeps); err != nil {
			return err
		}
		sub := keep + `, gone(id) AS (SELECT id FROM sub WHERE id NOT IN (SELECT id FROM kept))`
		if keeps {
			// What goes from directories that stay is dropped by the kernel too
			if err := p.removedBelow(sub+` SELECT i.parent, i.name, i.node FROM items AS i WHERE i.id IN
				(SELECT id FROM gone) AND i.deleted = 0 AND (i.parent = ?1 OR i.parent IN (SELECT id FROM kept))`,
				row.id); err != nil {
				return err
			}
		}
		if unplace {
			if _, err := p.tx.Exec(sub+` UPDATE remote_items SET placed = 0 WHERE item IN (SELECT id FROM gone)`,
				row.id); err != nil {
				return err
			}
		}
		if _, err := p.tx.Exec(sub+` UPDATE items SET deleted = 1, deleted_at = ?2 WHERE id IN (SELECT id FROM gone)
			AND deleted = 0`, row.id, now); err != nil {
			return err
		}
		if keeps {
			if unplace {
				// Displaced, with changes below it: it stays where it is, and its gateway item waits
				_, err := p.tx.Exec(`UPDATE remote_items SET placed = 0 WHERE item = ?`, row.id)
				return err
			}
			// Removed on the gateway with changes here below it: the directories they need are made again there
			if _, err := p.tx.Exec(keep+` UPDATE items SET local = local | ?2 WHERE (id = ?1 OR id IN
				(SELECT id FROM kept)) AND kind = ?3 AND id NOT IN (SELECT id FROM changed)`, row.id, localCreated,
				kindDir); err != nil {
				return err
			}
			if _, err := p.tx.Exec(keep+` UPDATE remote_items SET item = NULL WHERE item = ?1 OR (item IN
				(SELECT id FROM kept) AND item NOT IN (SELECT id FROM changed))`, row.id); err != nil {
				return err
			}
			p.resettle[row.folder] = true
			return nil
		}
	}
	if unplace {
		if _, err := p.tx.Exec(`UPDATE remote_items SET placed = 0 WHERE item = ?`, row.id); err != nil {
			return err
		}
	}
	if _, err := p.tx.Exec(`UPDATE items SET deleted = 1, deleted_at = ? WHERE id = ?`, now, row.id); err != nil {
		return err
	}
	parent, err := p.nodeOf(row.parent)
	if err != nil {
		return err
	}
	p.changes.removed(parent, row.name, uint64(row.node))
	return nil
}

// removedBelow records the items a query returns (parent, name, node) as gone
func (p *projector) removedBelow(query string, args ...any) error {
	rows, err := p.tx.Query(query, args...)
	if err != nil {
		return err
	}
	type gone struct {
		parent sql.NullInt64
		name   string
		node   int64
	}
	var list []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.parent, &g.name, &g.node); err != nil {
			rows.Close()
			return err
		}
		list = append(list, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range list {
		parent, err := p.nodeOf(g.parent)
		if err != nil {
			return err
		}
		p.changes.removed(parent, g.name, uint64(g.node))
	}
	return nil
}

// placeWaiting places a folder's waiting items until no more can be placed. Only items whose parent is shown are
// tried, a page at a time, and another pass is made only when one placed something. Nothing is displaced here, so
// two items that collide can't take each other's place in turn.
func (p *projector) placeWaiting(folder int64) error {
	var rootGatewayID sql.NullString
	if err := p.tx.QueryRow(`SELECT root_gateway_id FROM folders WHERE id = ?`, folder).Scan(&rootGatewayID); err != nil {
		return err
	}
	for progress := true; progress; {
		progress = false
		var after int64
		for {
			rows, err := p.tx.Query(`SELECT r.rowid, r.gateway_id FROM remote_items AS r INDEXED BY remote_items_waiting
				WHERE r.folder = ? AND r.placed = 0 AND r.deleted = 0 AND r.rowid > ?
				AND (r.parent_gateway_id = ? OR EXISTS (SELECT 1 FROM remote_items AS q
					WHERE q.gateway_id = r.parent_gateway_id AND q.placed = 1 AND q.deleted = 0 AND q.item IS NOT NULL))
				ORDER BY r.rowid LIMIT 200`, folder, after, rootGatewayID)
			if err != nil {
				return err
			}
			var page []string
			for rows.Next() {
				var gatewayID string
				if err := rows.Scan(&after, &gatewayID); err != nil {
					rows.Close()
					return err
				}
				page = append(page, gatewayID)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			for _, gatewayID := range page {
				placed, err := p.place(gatewayID, false)
				if err != nil {
					return err
				}
				progress = progress || placed
			}
		}
	}
	return nil
}

// removeUnseen ends a complete snapshot (with the changes after its start): items it didn't show are gone
func (p *projector) removeUnseen(folder, mark int64) error {
	// A page at a time, each after the last, so that the rows looked at once aren't looked at again
	var after int64
	for {
		rows, err := p.tx.Query(`SELECT rowid, gateway_id FROM remote_items WHERE folder = ? AND seen_mark != ?
			AND deleted = 0 AND rowid > ? ORDER BY rowid LIMIT 500`, folder, mark, after)
		if err != nil {
			return err
		}
		var unseen []string
		for rows.Next() {
			var gatewayID string
			if err := rows.Scan(&after, &gatewayID); err != nil {
				rows.Close()
				return err
			}
			unseen = append(unseen, gatewayID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(unseen) == 0 {
			return nil
		}
		for _, gatewayID := range unseen {
			if _, err := p.tx.Exec(`UPDATE remote_items SET deleted = 1 WHERE gateway_id = ?`, gatewayID); err != nil {
				return err
			}
			if _, err := p.place(gatewayID, false); err != nil {
				return err
			}
		}
	}
}

// finish places the waiting items of the folders touched. It can be called more than once.
func (p *projector) finish() error {
	folders := make([]int64, 0, len(p.touched))
	for folder := range p.touched {
		folders = append(folders, folder)
	}
	slices.Sort(folders)
	for _, folder := range folders {
		if err := p.placeWaiting(folder); err != nil {
			return err
		}
	}
	return nil
}
