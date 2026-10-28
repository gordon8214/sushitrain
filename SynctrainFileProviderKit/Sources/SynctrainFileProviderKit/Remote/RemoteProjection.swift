// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Projects confirmed remote metadata (`remote_items`) into the rows of `items` of folders accessed through a gateway,
/// inside one transaction, so that enumeration, sync anchors, attributes and item identifiers work as for other folders.
///
/// - A gateway item keeps its row: a rename or move updates the row, so the system's identifier stays the same.
/// - A new gateway item gets a new row. A removed row is revived only by its gateway ID (when the item shows up again
///   after it was hidden or displaced), never by its path. Rows from before the folder was bound are adopted by path
///   while they have no gateway item, to keep identifiers across the switch.
/// - The content revision changes only when the content identity does; the metadata revision with anything shown.
/// - A row with an overlay (a local change not confirmed by the gateway) shows that change; refreshes update the
///   remote metadata only, and the row follows it once the overlay is resolved.
/// - An item whose parent is not known yet, or whose name collides with another item that cannot be displaced, waits
///   (`placed = 0`) and is placed when that changes.
/// - An item that moved to another folder of the same gateway keeps its row: the destination's feed moves it there,
///   and the source's tombstone for the move (`movedToFolderID`) leaves it alone while this client reads that folder.
final class RemoteProjector {
	private let connection: SQLiteConnection
	private var nextSequence: Int64
	private let startSequence: Int64
	private let now = Date().timeIntervalSince1970
	private var touchedFolders: Set<String> = []
	private var rootIDs: [String: Int64] = [:]
	private var sessionMarks: [String: Int64?] = [:]
	/// Whether this client reads a folder of the gateway, by local folder and the gateway's folder ID
	private var readableFolders: [String: Bool] = [:]

	/// Number of changes the system sees: items added, changed or removed
	private(set) var visibleChanges = 0

	init(connection: SQLiteConnection) throws(IndexError) {
		self.connection = connection
		nextSequence = Int64(try IndexStore.meta(connection, "next_seq") ?? "1") ?? 1
		startSequence = nextSequence
	}

	/// Places waiting items of the folders touched, and stores the sequence. Can be called more than once.
	func finish() throws(IndexError) {
		for folderID in touchedFolders.sorted() {
			try placeWaiting(folderID: folderID)
		}
		if nextSequence != startSequence {
			try IndexStore.setMeta(connection, "next_seq", String(nextSequence))
		}
	}

	private func allocateSequence() -> Int64 {
		defer { nextSequence += 1 }
		return nextSequence
	}

	/// Allocates a sequence for every row at once: start + ID is unique
	private func allocateSequencesByID() throws(IndexError) -> Int64 {
		let highestID = try connection.queryOne("SELECT MAX(id) FROM items", [], { $0.optionalInt64(0) }) ?? nil
		let start = nextSequence
		nextSequence = start + (highestID ?? 0) + 1
		return start
	}

	// MARK: - Rows

	private struct RemoteRow {
		var gatewayID: String
		var itemID: Int64?
		var folderID: String
		var parentGatewayID: String?
		var name: String
		var kind: RemoteItemKind
		var contentID: String?
		var size: Int64
		var modifiedNs: Int64
		var writable: Bool
		var deleted: Bool
		var revision: RemoteRevision
	}

	private struct LocalRow {
		var id: Int64
		var folderID: String
		var path: String
		var parentID: Int64?
		var name: String
		var kind: ItemKind
		var size: Int64
		var modNs: Int64
		var blocksHash: String?
		var deleted: Bool
		var pending: Int64
	}

	private func remoteRow(_ gatewayID: String) throws(IndexError) -> RemoteRow? {
		try connection.queryOne(
			"""
			SELECT gateway_id, item_id, folder_id, parent_gateway_id, name, kind, content_id, size, modified_ns,
				writable, deleted, revision
			FROM remote_items WHERE gateway_id = ?
			""", [gatewayID]
		) { row in
			RemoteRow(
				gatewayID: row.text(0), itemID: row.optionalInt64(1), folderID: row.text(2),
				parentGatewayID: row.optionalText(3), name: row.text(4), kind: RemoteItemKind(rawValue: row.text(5)),
				contentID: row.optionalText(6), size: row.int64(7), modifiedNs: row.int64(8), writable: row.bool(9),
				deleted: row.bool(10), revision: RemoteRevision(row.text(11)))
		}
	}

	private static let localColumns =
		"id, folder_id, path, parent_id, name, kind, size, mod_ns, blocks_hash, deleted, pending"

	private static func localRow(from row: SQLiteStatement) -> LocalRow {
		LocalRow(
			id: row.int64(0), folderID: row.text(1), path: row.text(2), parentID: row.optionalInt64(3),
			name: row.text(4), kind: ItemKind(rawValue: Int(row.int64(5))) ?? .file, size: row.int64(6),
			modNs: row.int64(7), blocksHash: row.optionalText(8), deleted: row.bool(9), pending: row.int64(10))
	}

	private func localRow(_ id: Int64) throws(IndexError) -> LocalRow? {
		try connection.queryOne("SELECT \(Self.localColumns) FROM items WHERE id = ?", [id], Self.localRow)
	}

	private func hasOverlay(_ itemID: Int64) throws(IndexError) -> Bool {
		try connection.queryOne("SELECT 1 FROM remote_overlays WHERE item_id = ?", [itemID]) { _ in true } ?? false
	}

	private func gatewayIDOfItem(_ itemID: Int64) throws(IndexError) -> String? {
		try connection.queryOne("SELECT gateway_id FROM remote_items WHERE item_id = ?", [itemID]) { $0.text(0) }
	}

	private func rootID(folderID: String) throws(IndexError) -> Int64 {
		if let cached = rootIDs[folderID] {
			return cached
		}
		guard
			let id = try connection.queryOne(
				"SELECT root_id FROM folders WHERE folder_id = ?", [folderID], { $0.int64(0) })
		else {
			throw .notFound
		}
		rootIDs[folderID] = id
		return id
	}

	/// The mark of the folder's snapshot in progress: items stored outside the snapshot while it runs count as seen
	private func sessionMark(folderID: String) throws(IndexError) -> Int64? {
		if let cached = sessionMarks[folderID] {
			return cached
		}
		let mark = try connection.queryOne(
			"SELECT mark FROM remote_snapshots WHERE folder_id = ?", [folderID], { $0.int64(0) })
		sessionMarks[folderID] = .some(mark)
		return mark
	}

	// MARK: - Storing remote metadata

	/// Stores the item's confirmed metadata and projects it. `mark` is the snapshot mark of the session the item was
	/// seen in (nil outside a snapshot).
	///
	/// An item from outside the change feed (an operation's result, the current item of a rejection, `GET` of an item)
	/// passes `onlyOver`: the revisions the client knew of it when it sent the request. Such an item is stored only
	/// when no metadata of it is stored, or the stored revision is one of those: otherwise the change feed stored a
	/// state that may be newer (the cursor is past it already), and it is kept. Returns whether the item was stored.
	@discardableResult
	func apply(_ item: RemoteItem, folderID: String, mark: Int64?, onlyOver known: Set<RemoteRevision>? = nil)
		throws(IndexError) -> Bool
	{
		let previous = try remoteRow(item.id)
		if let known, let previous, !known.contains(previous.revision) {
			return false
		}
		if item.isDeleted, let destination = item.movedToFolderID, destination != item.folderID,
			try setsAsideMove(of: item, to: destination, folderID: folderID, previous: previous, mark: mark)
		{
			return false
		}
		var seenMark = mark
		if seenMark == nil {
			seenMark = try sessionMark(folderID: folderID)
		}
		let values: [any SQLBindable] = [
			folderID, item.parentID, item.path, item.name, item.kind.rawValue, item.revision.rawValue,
			item.contentID?.rawValue, item.size ?? 0, item.modifiedNs, item.mode ?? 0, item.writable ?? false,
			item.subtreeGeneration, item.availability?.rawValue, item.isDeleted, now,
		]
		if previous != nil {
			try connection.run(
				"""
				UPDATE remote_items SET folder_id = ?, parent_gateway_id = ?, path = ?, name = ?, kind = ?, revision = ?,
					content_id = ?, size = ?, modified_ns = ?, mode = ?, writable = ?, subtree_generation = ?,
					availability = ?, deleted = ?, updated_at = ?, seen_mark = COALESCE(?, seen_mark)
				WHERE gateway_id = ?
				""", values + [seenMark, item.id])
		}
		else {
			try connection.run(
				"""
				INSERT INTO remote_items (folder_id, parent_gateway_id, path, name, kind, revision, content_id, size,
					modified_ns, mode, writable, subtree_generation, availability, deleted, updated_at, seen_mark,
					gateway_id, placed)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
				""", values + [seenMark ?? 0, item.id])
		}
		if item.isRoot {
			try connection.run(
				"UPDATE remote_bindings SET root_gateway_id = ? WHERE folder_id = ?", [item.id, folderID])
		}
		let writableChanged = previous.map { $0.writable != (item.writable ?? false) } ?? false
		try place(item.id, mayDisplace: true, forceChange: writableChanged)
		if let previous, previous.folderID != folderID, !item.isDeleted, item.kind == .directory {
			try moveMetadata(below: item.id)
		}
		return true
	}

	/// For a tombstone of an item that moved to another folder of the same gateway (`movedToFolderID`): whether it is
	/// set aside rather than stored. It is when this client reads the destination folder, whose change feed moves the
	/// item (keeping its row and identifier), and when the item is in another folder already (that feed came first).
	/// Otherwise it is an ordinary deletion.
	private func setsAsideMove(
		of item: RemoteItem, to destination: String, folderID: String, previous: RemoteRow?, mark: Int64?
	) throws(IndexError) -> Bool {
		if let previous, previous.folderID != folderID {
			return true
		}
		guard try readsFolder(destination, onGatewayOf: folderID) else {
			return false
		}
		var seen = mark
		if seen == nil {
			seen = try sessionMark(folderID: folderID)
		}
		if previous != nil, let seen {
			// Seen by this folder's snapshot, which must not remove it before the destination's feed moves it
			try connection.run("UPDATE remote_items SET seen_mark = ? WHERE gateway_id = ?", [seen, item.id])
		}
		return true
	}

	/// Whether this client reads the folder of the gateway that serves the local folder (it is bound, and readable)
	private func readsFolder(_ remoteFolderID: String, onGatewayOf folderID: String) throws(IndexError) -> Bool {
		let key = folderID + "\u{0}" + remoteFolderID
		if let cached = readableFolders[key] {
			return cached
		}
		var reads = false
		if let serviceID = try IndexStore.remoteBinding(connection, folderID: folderID)?.serviceID,
			let destination = try IndexStore.localFolderID(
				connection, serviceID: serviceID, remoteFolderID: remoteFolderID),
			let binding = try IndexStore.remoteBinding(connection, folderID: destination)
		{
			reads = binding.isRemote && binding.permissions.read
		}
		readableFolders[key] = reads
		return reads
	}

	/// After a directory moved to another folder: the gateway metadata of the rows below it moves along (the rows did,
	/// see `place`), so that the old folder's snapshots and tombstones no longer concern them
	private func moveMetadata(below gatewayID: String) throws(IndexError) {
		guard let itemID = try remoteRow(gatewayID)?.itemID, let row = try localRow(itemID), !row.deleted,
			row.kind.isContainer
		else {
			return
		}
		let (lower, upper) = LocalChangeRecorder.descendantRange(of: row.path)
		let below = "SELECT id FROM items WHERE folder_id = ?1 AND deleted = 0 AND path >= ?2 AND path < ?3"
		try connection.run(
			"UPDATE remote_items SET folder_id = ?1 WHERE folder_id != ?1 AND item_id IN (\(below))",
			[row.folderID, lower, upper])
		try connection.run(
			"UPDATE remote_overlays SET folder_id = ?1 WHERE folder_id != ?1 AND item_id IN (\(below))",
			[row.folderID, lower, upper])
	}

	/// Maps a gateway item to a local row (for the result of a create: the row the user created)
	func map(_ gatewayID: String, to itemID: Int64) throws(IndexError) {
		try connection.run(
			"UPDATE remote_items SET item_id = NULL, placed = 0 WHERE item_id = ? AND gateway_id != ?",
			[itemID, gatewayID])
		if let current = try remoteRow(gatewayID)?.itemID, current != itemID, let row = try localRow(current),
			!row.deleted
		{
			try removeRow(row, unplace: false)
		}
		try connection.run("UPDATE remote_items SET item_id = ? WHERE gateway_id = ?", [itemID, gatewayID])
	}

	/// Detaches a gateway item from its row (the row now shows another item); it gets a new row when placed again
	func unmap(_ gatewayID: String) throws(IndexError) {
		try connection.run("UPDATE remote_items SET item_id = NULL, placed = 0 WHERE gateway_id = ?", [gatewayID])
	}

	/// Projects the stored metadata of the gateway item into its row again (after its overlay was resolved)
	func reproject(_ gatewayID: String) throws(IndexError) {
		try place(gatewayID, mayDisplace: true, forceChange: true)
	}

	/// Keeps a gateway item without a row (the source of a cross-host move, until it is deleted)
	func hide(_ gatewayID: String) throws(IndexError) {
		try connection.run("UPDATE remote_items SET item_id = NULL, placed = 1 WHERE gateway_id = ?", [gatewayID])
	}

	// MARK: - For recording local changes in the same transaction

	func nextChangeSequence() -> Int64 {
		allocateSequence()
	}

	func sequencesByID() throws(IndexError) -> Int64 {
		try allocateSequencesByID()
	}

	func countVisibleChange() {
		visibleChanges += 1
	}

	// MARK: - Placing

	/// Projects a gateway item's stored metadata into its row. Returns false when it has to wait.
	@discardableResult
	private func place(_ gatewayID: String, mayDisplace: Bool, forceChange: Bool = false) throws(IndexError) -> Bool {
		guard let remote = try remoteRow(gatewayID) else {
			return false
		}
		touchedFolders.insert(remote.folderID)

		// The row of an item with a local change shows that change until its operation is resolved
		if let itemID = remote.itemID, try hasOverlay(itemID) {
			try setPlaced(gatewayID, itemID: itemID)
			return true
		}
		if remote.parentGatewayID == nil {
			// The folder root keeps its row (named after the folder)
			if !remote.deleted {
				let rootID = try rootID(folderID: remote.folderID)
				try connection.run(
					"UPDATE remote_items SET item_id = NULL WHERE item_id = ? AND gateway_id != ?", [rootID, gatewayID])
				try setPlaced(gatewayID, itemID: rootID)
			}
			return true
		}

		let name = FileNames.normalized(remote.name)
		let kind: ItemKind?
		switch remote.kind {
		case .file: kind = .file
		case .directory: kind = .directory
		default: kind = nil
		}
		// Gone, or not shown (symbolic links, unknown kinds, names that cannot be on disk or belong to Syncthing)
		guard !remote.deleted, let kind, FileNames.isValid(name), !FileNames.isReserved(name) else {
			if let itemID = remote.itemID, let row = try localRow(itemID), !row.deleted {
				try removeRow(row, unplace: false)
			}
			try setPlaced(gatewayID, itemID: remote.itemID)
			return true
		}
		guard let parent = try parentRow(of: remote) else {
			try setWaiting(gatewayID)
			return false
		}
		let path = FileNames.join(parent.path, name)

		var current = try remote.itemID.flatMap(localRow)
		if let row = current, row.kind != kind {
			// The system cannot change an item's kind: remove the old row and show the item as a new one
			if !row.deleted {
				try removeRow(row, unplace: false)
			}
			try connection.run("UPDATE remote_items SET item_id = NULL WHERE gateway_id = ?", [gatewayID])
			current = nil
		}

		// Other rows where this one goes: the same path, or a name that cannot coexist with this one. Two lookups, each
		// on its index: for an OR of the two, SQLite scans every row (or every sibling), once per item placed.
		let excluded = current?.id ?? 0
		let obstacles = try connection.query(
			"""
			SELECT \(Self.localColumns) FROM items INDEXED BY items_live_path
			WHERE deleted = 0 AND id != ? AND folder_id = ? AND path = ?
			UNION
			SELECT \(Self.localColumns) FROM items INDEXED BY items_child_name
			WHERE deleted = 0 AND id != ? AND parent_id = ? AND name_key = ?
			""", [excluded, parent.folderID, path, excluded, parent.id, FileNames.collisionKey(name)], Self.localRow)
		for other in obstacles {
			let samePath = other.folderID == parent.folderID && other.path.utf8.elementsEqual(path.utf8)
			let otherHasOverlay = try hasOverlay(other.id)
			if try otherHasOverlay ? true : subtreeHasOverlay(other) {
				// A local change is there: wait for it to be resolved
				try setWaiting(gatewayID)
				return false
			}
			let otherGatewayID = try gatewayIDOfItem(other.id)
			if otherGatewayID == nil, current == nil, samePath, other.kind == kind {
				// A row from before the folder was bound to the gateway: keep its identifier
				try connection.run("UPDATE remote_items SET item_id = ? WHERE gateway_id = ?", [other.id, gatewayID])
				current = other
				continue
			}
			// The other row is stale (its item moved away or is gone) when it is at this very path; a name that only
			// collides could be a genuine second item (a case-sensitive gateway), so that waits
			guard mayDisplace, otherGatewayID == nil || samePath else {
				try setWaiting(gatewayID)
				return false
			}
			try removeRow(other, unplace: true)
		}

		let contentID = kind == .file ? remote.contentID : nil
		let size = kind == .file ? remote.size : 0
		if let row = current {
			let moved = row.folderID != parent.folderID || !row.path.utf8.elementsEqual(path.utf8)
			let contentChanged = kind == .file && row.blocksHash != contentID
			let changed =
				row.deleted || moved || row.parentID != parent.id || !row.name.utf8.elementsEqual(name.utf8)
				|| row.size != size || row.modNs != remote.modifiedNs || contentChanged || row.pending != 0
				|| forceChange
			if changed {
				try connection.run(
					"""
					UPDATE items SET folder_id = ?, path = ?, parent_id = ?, name = ?, name_key = ?, size = ?, mod_ns = ?,
						blocks_hash = ?, modified_by = NULL, deleted = 0, deleted_at = NULL, implied = 0, pending = 0,
						pending_path = NULL, pending_size = NULL, pending_mod_ns = NULL,
						content_rev = content_rev + ?, meta_rev = meta_rev + 1, changed_seq = ?
					WHERE id = ?
					""",
					[
						parent.folderID, path, parent.id, name, FileNames.collisionKey(name), size, remote.modifiedNs,
						contentID, contentChanged ? 1 : 0, allocateSequence(), row.id,
					])
				if moved, kind == .directory, !row.deleted {
					// Everything below moves along; the rows keep their parents, so the system needs no change
					let (lower, upper) = LocalChangeRecorder.descendantRange(of: row.path)
					try connection.run(
						"""
						UPDATE items SET folder_id = ?, path = ? || substr(path, ?)
						WHERE folder_id = ? AND deleted = 0 AND path >= ? AND path < ?
						""", [parent.folderID, path, row.path.unicodeScalars.count + 1, row.folderID, lower, upper])
				}
				visibleChanges += 1
			}
			try setPlaced(gatewayID, itemID: row.id)
			return true
		}

		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, blocks_hash, changed_seq,
				selected)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT selected FROM items WHERE id = ?))
			""",
			[
				parent.folderID, path, parent.id, name, FileNames.collisionKey(name), kind.rawValue, size,
				remote.modifiedNs, contentID, allocateSequence(), parent.id,
			])
		try setPlaced(gatewayID, itemID: connection.lastInsertRowID)
		visibleChanges += 1
		return true
	}

	/// The live row of the item's parent, if it is shown
	private func parentRow(of remote: RemoteRow) throws(IndexError) -> LocalRow? {
		guard let parentGatewayID = remote.parentGatewayID else {
			return nil
		}
		if try IndexStore.rootGatewayID(connection, folderID: remote.folderID) == parentGatewayID {
			return try localRow(try rootID(folderID: remote.folderID))
		}
		guard
			let parentID = try connection.queryOne(
				"""
				SELECT item_id FROM remote_items
				WHERE gateway_id = ? AND placed = 1 AND deleted = 0 AND item_id IS NOT NULL
				""", [parentGatewayID], { $0.int64(0) }),
			let parent = try localRow(parentID), !parent.deleted, parent.kind.isContainer
		else {
			return nil
		}
		return parent
	}

	private func setPlaced(_ gatewayID: String, itemID: Int64?) throws(IndexError) {
		try connection.run(
			"UPDATE remote_items SET item_id = ?, placed = 1 WHERE gateway_id = ?", [itemID, gatewayID])
	}

	private func setWaiting(_ gatewayID: String) throws(IndexError) {
		try connection.run("UPDATE remote_items SET placed = 0 WHERE gateway_id = ?", [gatewayID])
	}

	private func subtreeHasOverlay(_ row: LocalRow) throws(IndexError) -> Bool {
		guard row.kind.isContainer else {
			return false
		}
		let (condition, values) = LocalChangeRecorder.subtree(below: row.path)
		return try connection.queryOne(
			"""
			SELECT EXISTS (
				SELECT 1 FROM items AS i JOIN remote_overlays AS o ON o.item_id = i.id
				WHERE i.folder_id = ? AND i.deleted = 0 AND \(condition.replacingOccurrences(of: "path", with: "i.path")))
			""", [row.folderID] + values, { $0.bool(0) }) ?? false
	}

	/// Removes a row and the rows below it, except those with local changes (which keep a directory above them). With
	/// `unplace`, the gateway items of the removed rows wait to be placed again (they were displaced, not deleted).
	private func removeRow(_ row: LocalRow, unplace: Bool) throws(IndexError) {
		if row.kind.isContainer {
			let (condition, values) = LocalChangeRecorder.subtree(below: row.path)
			let start = try allocateSequencesByID()
			let scope = """
				folder_id = ? AND deleted = 0 AND \(condition)
					AND NOT EXISTS (SELECT 1 FROM remote_overlays AS o WHERE o.item_id = items.id)
				"""
			if unplace {
				try connection.run(
					"UPDATE remote_items SET placed = 0 WHERE item_id IN (SELECT id FROM items WHERE \(scope))",
					[row.folderID] + values)
			}
			try connection.run(
				"""
				UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_path = NULL, pending_size = NULL,
					pending_mod_ns = NULL, meta_rev = meta_rev + 1, changed_seq = ? + id
				WHERE \(scope)
				""", [now, start, row.folderID] + values)
			visibleChanges += connection.changes
			if try subtreeHasOverlay(row) {
				// Local changes below it: the directory stays until they are resolved
				return
			}
		}
		if unplace {
			try connection.run("UPDATE remote_items SET placed = 0 WHERE item_id = ?", [row.id])
		}
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_path = NULL, pending_size = NULL,
				pending_mod_ns = NULL, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ? AND deleted = 0
			""", [now, allocateSequence(), row.id])
		visibleChanges += connection.changes
	}

	/// Waiting items read at once
	private static let waitingPage = 200

	/// Places the folder's waiting items until no more can be placed. Only items whose parent is shown are tried:
	/// the others (whose parent was not received yet, is hidden, or waits itself) cannot be placed, and are tried once
	/// their parent is, in a later pass. The items are read a page at a time, and another pass is made only when this
	/// one placed something. Nothing is displaced here, so two items that collide cannot take each other's place in
	/// turn.
	private func placeWaiting(folderID: String) throws(IndexError) {
		let rootGatewayID = try IndexStore.rootGatewayID(connection, folderID: folderID)
		var progress = true
		while progress {
			progress = false
			var afterRowID: Int64 = 0
			while true {
				let page = try connection.query(
					"""
					SELECT r.rowid, r.gateway_id FROM remote_items AS r INDEXED BY remote_items_waiting
					WHERE r.folder_id = ? AND r.placed = 0 AND r.deleted = 0 AND r.rowid > ?
						AND (r.parent_gateway_id = ? OR EXISTS (
							SELECT 1 FROM remote_items AS p
							WHERE p.gateway_id = r.parent_gateway_id AND p.placed = 1 AND p.deleted = 0
								AND p.item_id IS NOT NULL))
					ORDER BY r.rowid LIMIT ?
					""", [folderID, afterRowID, rootGatewayID, Self.waitingPage]
				) { (rowID: $0.int64(0), gatewayID: $0.text(1)) }
				guard let last = page.last else {
					break
				}
				afterRowID = last.rowID
				for entry in page where try place(entry.gatewayID, mayDisplace: false) {
					progress = true
				}
			}
		}
	}

	// MARK: - Completing a snapshot

	/// After a complete snapshot (with the changes after its start): items it did not show are gone, and rows from
	/// before the folder was bound that no gateway item adopted are removed. Rows with local changes stay.
	func removeUnseen(folderID: String, mark: Int64) throws(IndexError) {
		// In batches, so that a folder whose items all went away does not load them all at once. Every item handled is
		// marked deleted, which ends the loop.
		while true {
			let unseen = try connection.query(
				"""
				SELECT gateway_id FROM remote_items WHERE folder_id = ? AND seen_mark != ? AND deleted = 0 LIMIT 500
				""", [folderID, mark], { $0.text(0) })
			if unseen.isEmpty {
				break
			}
			for gatewayID in unseen {
				try connection.run(
					"UPDATE remote_items SET deleted = 1, updated_at = ? WHERE gateway_id = ?", [now, gatewayID])
				try place(gatewayID, mayDisplace: false)
			}
		}
		var afterID: Int64 = 0
		while true {
			let unmapped = try connection.query(
				"""
				SELECT \(Self.localColumns) FROM items
				WHERE folder_id = ? AND deleted = 0 AND kind != ? AND id > ?
					AND NOT EXISTS (SELECT 1 FROM remote_items AS r WHERE r.item_id = items.id)
					AND NOT EXISTS (SELECT 1 FROM remote_overlays AS o WHERE o.item_id = items.id)
				ORDER BY id LIMIT 500
				""", [folderID, ItemKind.folderRoot.rawValue, afterID], Self.localRow)
			guard let last = unmapped.last else {
				break
			}
			afterID = last.id
			for row in unmapped {
				if let current = try localRow(row.id), !current.deleted {
					try removeRow(current, unplace: false)
				}
			}
		}
	}
}
