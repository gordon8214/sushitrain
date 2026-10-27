// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

extension IndexStore {
	/// Runs the block with a recorder for changes made through the File Provider, in a single transaction
	@discardableResult
	public func recordLocalChanges<T>(_ block: (LocalChangeRecorder) throws(IndexError) -> T) throws(IndexError) -> T {
		try transaction { connection throws(IndexError) in
			let recorder = try LocalChangeRecorder(connection: connection)
			let result = try block(recorder)
			try recorder.finish()
			return result
		}
	}
}

/// Records changes that the File Provider extension made to the folders on disk, so that they show up right away and
/// are not undone by index records from before Syncthing scanned them. Each change is marked pending until the app has
/// seen Syncthing index it (see `IndexWriter.apply`). Obtain one through `IndexStore.recordLocalChanges`.
///
/// The recorder works with the items as they are in the index when it records a change, not with the `IndexItem`
/// passed in, which was read earlier: the app may have applied Syncthing's records of the item since.
///
/// A deletion leaves a pending marker (a tombstone) for every item it removes, and a rename records where Syncthing
/// knows the item. Records up to the sequence the change was recorded after (the folder's cursor, or the record last
/// applied to the item if that is newer) predate the change; so do newer records of the version that was deleted or
/// moved away (see `IndexWriter`). The items below a renamed directory move along without becoming pending themselves:
/// the directory's rename covers them.
public final class LocalChangeRecorder {
	private let connection: SQLiteConnection
	private var nextSequence: Int64
	private let startSequence: Int64
	private let now = Date().timeIntervalSince1970

	init(connection: SQLiteConnection) throws(IndexError) {
		self.connection = connection
		nextSequence = Int64(try IndexStore.meta(connection, "next_seq") ?? "1") ?? 1
		startSequence = nextSequence
	}

	func finish() throws(IndexError) {
		if nextSequence != startSequence {
			try IndexStore.setMeta(connection, "next_seq", String(nextSequence))
		}
	}

	private func allocateSequence() -> Int64 {
		defer { nextSequence += 1 }
		return nextSequence
	}

	/// Allocates a sequence for the item and every item below it at once: start + ID, for the returned start. The ID is
	/// unique, so start + ID is too. A file needs one sequence only.
	private func allocateSequences(for item: IndexItem) throws(IndexError) -> Int64 {
		if !item.kind.isContainer {
			return allocateSequence() - item.id
		}
		let highestID = try connection.queryOne("SELECT MAX(id) FROM items", [], { $0.optionalInt64(0) }) ?? nil
		let start = nextSequence
		nextSequence = start + (highestID ?? 0) + 1
		return start
	}

	// MARK: - Reading

	public func item(id: Int64) throws(IndexError) -> IndexItem? {
		try connection.queryOne(
			"SELECT \(IndexStore.itemColumns) FROM items WHERE id = ? AND deleted = 0", [id], IndexStore.item)
	}

	public func folder(id: String) throws(IndexError) -> IndexFolder? {
		try connection.queryOne(
			"SELECT folder_id, root_id, label, folder_type, selective FROM folders WHERE folder_id = ?", [id],
			IndexStore.folder)
	}

	/// The live item in the container whose name cannot coexist with the given name
	public func child(of parentID: Int64, named name: String) throws(IndexError) -> IndexItem? {
		try Self.child(connection, of: parentID, named: name)
	}

	/// All live items below a directory
	public func descendants(of item: IndexItem) throws(IndexError) -> [IndexItem] {
		try Self.descendants(connection, of: item)
	}

	static func child(_ connection: SQLiteConnection, of parentID: Int64, named name: String) throws(IndexError)
		-> IndexItem?
	{
		try connection.queryOne(
			"""
			SELECT \(IndexStore.itemColumns) FROM items
			WHERE parent_id = ? AND name_key = ? AND deleted = 0 LIMIT 1
			""", [parentID, FileNames.collisionKey(name)], IndexStore.item)
	}

	static func descendants(_ connection: SQLiteConnection, of item: IndexItem) throws(IndexError) -> [IndexItem] {
		let (condition, values) = subtree(below: item.path)
		return try connection.query(
			"""
			SELECT \(IndexStore.itemColumns) FROM items
			WHERE folder_id = ? AND deleted = 0 AND \(condition) ORDER BY path
			""", [item.folderID] + values, IndexStore.item)
	}

	/// Paths below `path` sort between `path + "/"` and `path + "0"` ("0" follows "/" in byte order)
	static func descendantRange(of path: String) -> (String, String) {
		(path + "/", path + "0")
	}

	/// A condition on `path` (with its values) that holds for the items below the path in its folder
	static func subtree(below path: String) -> (String, [any SQLBindable]) {
		if path.isEmpty {
			return ("path != ''", [])
		}
		let (lower, upper) = descendantRange(of: path)
		return ("path >= ? AND path < ?", [lower, upper])
	}

	/// The current state of a live item, with what the recorder needs beyond `IndexItem`
	private struct Row {
		var item: IndexItem
		/// Where Syncthing knows a renamed item
		var originalPath: String?
		/// The size the last change through the File Provider wrote (nil when it changed nothing but the location)
		var pendingSize: Int64?
	}

	private func row(_ id: Int64) throws(IndexError) -> Row {
		guard
			let row = try connection.queryOne(
				"""
				SELECT \(IndexStore.itemColumns), pending_path, pending_size FROM items WHERE id = ? AND deleted = 0
				""", [id],
				{ Row(item: IndexStore.item(from: $0), originalPath: $0.optionalText(17), pendingSize: $0.optionalInt64(18)) }
			)
		else {
			throw .notFound
		}
		return row
	}

	/// The Syncthing sequence up to which the folder's records have been applied
	private func cursor(_ folderID: String) throws(IndexError) -> Int64 {
		try connection.queryOne("SELECT cursor FROM folders WHERE folder_id = ?", [folderID]) { $0.int64(0) } ?? 0
	}

	/// The live container that an item at the path goes into, checked against the index as it is now
	private func container(_ parentID: Int64, folderID: String, for path: String) throws(IndexError) -> IndexItem {
		guard let parent = try item(id: parentID) else {
			throw .notFound
		}
		guard parent.kind.isContainer, parent.folderID == folderID else {
			throw .notAllowed("the parent is not a directory in folder \(folderID)")
		}
		guard !path.isEmpty, FileNames.isValidPath(path),
			FileNames.parentPath(path).utf8.elementsEqual(parent.path.utf8)
		else {
			throw .notAllowed("\(path) is not a valid path in \(parent.path.isEmpty ? "the folder" : parent.path)")
		}
		return parent
	}

	/// Throws when a live item in the container has a name that cannot coexist with the given one
	private func checkNameIsFree(_ name: String, in parentID: Int64, except itemID: Int64? = nil) throws(IndexError) {
		if let existing = try Self.child(connection, of: parentID, named: name), existing.id != itemID {
			throw .nameCollision(existing: existing.id)
		}
	}

	// MARK: - Recording

	/// Records an item created on disk. The item stays pending until Syncthing indexes a matching entry. Throws
	/// `.nameCollision` when the container has an item whose name cannot coexist with it.
	public func recordCreation(
		folderID: String, parentID: Int64, path: String, kind: ItemKind, size: Int64, modifiedNanoseconds: Int64,
		isSelected: Bool
	) throws(IndexError) -> IndexItem {
		guard kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be created")
		}
		let path = FileNames.normalized(path)
		_ = try container(parentID, folderID: folderID, for: path)
		let name = FileNames.lastComponent(path)
		try checkNameIsFree(name, in: parentID)
		let size = kind == .file ? size : 0
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, changed_seq, selected,
				pending, pending_size, pending_mod_ns, pending_after)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			""",
			[
				folderID, path, parentID, name, FileNames.collisionKey(name), kind.rawValue, size, modifiedNanoseconds,
				allocateSequence(), isSelected, PendingChange.create.rawValue, size, modifiedNanoseconds,
				try cursor(folderID),
			])
		return try reload(connection.lastInsertRowID)
	}

	/// Records new contents written to a file
	public func recordContentChange(_ item: IndexItem, size: Int64, modifiedNanoseconds: Int64) throws(IndexError)
		-> IndexItem
	{
		try recordFileState(item, size: size, modifiedNanoseconds: modifiedNanoseconds, contentChanged: true)
	}

	/// Records a new modification time set on a file or directory
	public func recordModificationDate(_ item: IndexItem, modifiedNanoseconds: Int64) throws(IndexError) -> IndexItem {
		try recordFileState(item, size: nil, modifiedNanoseconds: modifiedNanoseconds, contentChanged: false)
	}

	private func recordFileState(_ item: IndexItem, size: Int64?, modifiedNanoseconds: Int64, contentChanged: Bool)
		throws(IndexError) -> IndexItem
	{
		let current = try row(item.id).item
		guard current.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be changed")
		}
		let size = current.kind == .file ? (size ?? current.size) : 0
		// A creation or rename that Syncthing has not seen yet stays one; it now expects the new state
		let pending: PendingChange = current.pending == .none ? .modify : current.pending
		try connection.run(
			"""
			UPDATE items SET size = ?, mod_ns = ?, pending = ?, pending_size = ?, pending_mod_ns = ?,
				pending_after = MAX(?, st_seq), content_rev = content_rev + ?, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				size, modifiedNanoseconds, pending.rawValue, size, modifiedNanoseconds, try cursor(current.folderID),
				contentChanged ? 1 : 0, allocateSequence(), current.id,
			])
		return try reload(current.id)
	}

	/// Records that an item (and everything below it) moved to a new path in the same folder. The item keeps its
	/// identifier. Until Syncthing has indexed the new path, records for the old path are ignored. Throws
	/// `.nameCollision` when the container has another item whose name cannot coexist with the new name.
	public func recordMove(_ item: IndexItem, to parentID: Int64, path: String) throws(IndexError) -> IndexItem {
		let path = FileNames.normalized(path)
		let current = try row(item.id)
		let moving = current.item
		guard moving.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be moved")
		}
		let oldPath = moving.path
		if path.utf8.elementsEqual(oldPath.utf8) {
			return moving
		}
		_ = try container(parentID, folderID: moving.folderID, for: path)
		if moving.kind.isContainer, path.utf8.starts(with: (oldPath + "/").utf8) {
			throw .notAllowed("a directory cannot be moved into itself")
		}
		let name = FileNames.lastComponent(path)
		try checkNameIsFree(name, in: parentID, except: moving.id)

		// Remember the path Syncthing knows the item by. An item that was already moved keeps its original path
		// (unless it moved back there).
		let pending: PendingChange
		let originalPath: String?
		switch moving.pending {
		case .create:
			// Syncthing does not know the item, unless it scanned it before the app applied the record: leave a
			// deletion behind for what moves, so that such a record does not bring it back at the old path
			pending = .create
			originalPath = nil
			try leaveDeletionMarkers(for: moving, onlyNewItems: true)
		case .rename where current.originalPath?.utf8.elementsEqual(path.utf8) == true:
			// Back where Syncthing knows it: what is left to confirm is a change of the contents, if there was one
			pending = current.pendingSize == nil ? .none : .modify
			originalPath = nil
		case .rename:
			pending = .rename
			originalPath = current.originalPath
		default:
			pending = .rename
			originalPath = oldPath
		}
		let isUnchanged = pending == .none
		try connection.run(
			"""
			UPDATE items SET path = ?, parent_id = ?, name = ?, name_key = ?, pending = ?, pending_path = ?,
				pending_size = CASE WHEN ? THEN NULL ELSE pending_size END,
				pending_mod_ns = CASE WHEN ? THEN NULL ELSE pending_mod_ns END,
				pending_after = MAX(?, st_seq), meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				path, parentID, name, FileNames.collisionKey(name), pending.rawValue, originalPath, isUnchanged,
				isUnchanged, try cursor(moving.folderID), allocateSequence(), moving.id,
			])

		if moving.kind.isContainer {
			// Everything below moves along. Entries Syncthing knows keep their state: the rename of the directory
			// covers them (a full pass does not remove them, and records of their old paths are ignored).
			let (lower, upper) = Self.descendantRange(of: oldPath)
			try connection.run(
				"""
				UPDATE items SET path = ? || substr(path, ?)
				WHERE folder_id = ? AND deleted = 0 AND path >= ? AND path < ?
				""", [path, oldPath.unicodeScalars.count + 1, moving.folderID, lower, upper])  // substr counts code points
		}
		return try reload(moving.id)
	}

	/// The hash of an item's contents, as a pending deletion keeps it: only when Syncthing hashed what is deleted (the
	/// contents did not change through the File Provider since). A record with another hash is a newer version.
	private static let knownBlocksHash = """
		CASE WHEN pending IN (\(PendingChange.create.rawValue), \(PendingChange.modify.rawValue)) \
		OR pending_size IS NOT NULL THEN NULL ELSE blocks_hash END
		"""

	/// Leaves pending deletions at the current paths of the item and the items below it (only of the ones Syncthing has
	/// not seen yet, if requested)
	private func leaveDeletionMarkers(for item: IndexItem, onlyNewItems: Bool) throws(IndexError) {
		let (subtree, values) = Self.subtree(below: item.path)
		let condition = onlyNewItems ? "pending = \(PendingChange.create.rawValue)" : "1"
		let start = try allocateSequences(for: item)
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, blocks_hash, changed_seq,
				deleted, deleted_at, pending, pending_size, pending_mod_ns, pending_after)
			SELECT folder_id, path, parent_id, name, name_key, kind, size, mod_ns, \(Self.knownBlocksHash), ? + id, 1,
				?, \(PendingChange.delete.rawValue), size, mod_ns, MAX(?, st_seq)
			FROM items WHERE folder_id = ? AND deleted = 0 AND (id = ? OR (\(subtree))) AND \(condition)
			""", [start, now, try cursor(item.folderID), item.folderID, item.id] + values)
	}

	/// Records that an item (and everything below it) moved to another folder. To Syncthing that is a removal from one
	/// folder and an addition to the other; the item keeps its identifier. The old folder gets pending deletions for
	/// what moved, so that its records from before the move do not bring the items back. Items with a pending rename
	/// cannot be moved this way (their old paths would have to be kept in the old folder).
	public func recordMove(_ item: IndexItem, toFolder folderID: String, parentID: Int64, path: String)
		throws(IndexError) -> IndexItem
	{
		let path = FileNames.normalized(path)
		let moving = try row(item.id).item
		guard moving.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be moved")
		}
		if moving.folderID == folderID {
			return try recordMove(moving, to: parentID, path: path)
		}
		_ = try container(parentID, folderID: folderID, for: path)
		let name = FileNames.lastComponent(path)
		try checkNameIsFree(name, in: parentID)

		let oldPath = moving.path
		let (subtree, subtreeValues) = Self.subtree(below: oldPath)
		let scope = "folder_id = ? AND deleted = 0 AND (id = ? OR (\(subtree)))"
		let scopeValues: [any SQLBindable] = [moving.folderID, moving.id] + subtreeValues
		if try connection.queryOne(
			"SELECT EXISTS (SELECT 1 FROM items WHERE \(scope) AND pending = \(PendingChange.rename.rawValue))",
			scopeValues, { $0.bool(0) }) == true
		{
			throw .notAllowed("items with a pending rename cannot be moved to another folder")
		}

		try leaveDeletionMarkers(for: moving, onlyNewItems: false)

		// In the new folder, everything is new to Syncthing (and the sequences of the old folder's records mean nothing)
		let start = try allocateSequences(for: moving)
		let oldPathLength = oldPath.unicodeScalars.count  // substr counts code points
		try connection.run(
			"""
			UPDATE items SET folder_id = ?, path = ? || substr(path, ?), pending = \(PendingChange.create.rawValue),
				pending_path = NULL, pending_size = size, pending_mod_ns = mod_ns, pending_after = ?, st_seq = 0,
				meta_rev = meta_rev + 1, changed_seq = ? + id
			WHERE \(scope)
			""",
			[folderID, path, oldPathLength + 1, try cursor(folderID), start] + scopeValues)
		try connection.run(
			"UPDATE items SET parent_id = ?, name = ?, name_key = ? WHERE id = ?",
			[parentID, name, FileNames.collisionKey(name), moving.id])
		return try reload(moving.id)
	}

	/// Records that an item (and everything below it) was deleted from disk. Every item removed stays as a pending
	/// deletion until Syncthing has indexed it (or a full pass shows that Syncthing never knew the path); until then,
	/// records of the deleted versions are ignored. An item that is no longer in the index is left alone.
	public func recordDeletion(_ item: IndexItem) throws(IndexError) {
		guard let current = try self.item(id: item.id) else {
			return
		}
		guard current.kind != .folderRoot else {
			throw .notAllowed("folder roots cannot be deleted")
		}
		let (subtree, values) = Self.subtree(below: current.path)
		let start = try allocateSequences(for: current)
		// A renamed item keeps the path Syncthing knows it by, so that records of that path do not bring it back
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = \(PendingChange.delete.rawValue),
				pending_path = CASE WHEN pending = \(PendingChange.rename.rawValue) THEN pending_path END,
				blocks_hash = \(Self.knownBlocksHash), pending_size = size, pending_mod_ns = mod_ns,
				pending_after = MAX(?, st_seq), meta_rev = meta_rev + 1, changed_seq = ? + id
			WHERE folder_id = ? AND deleted = 0 AND (id = ? OR (\(subtree)))
			""", [now, try cursor(current.folderID), start, current.folderID, current.id] + values)
	}

	private func reload(_ id: Int64) throws(IndexError) -> IndexItem {
		guard let item = try item(id: id) else {
			throw .notFound
		}
		return item
	}

	// MARK: - Work for the app

	public func enqueue(_ kind: WorkKind, folderID: String, path: String, extra: String? = nil) throws(IndexError) {
		try IndexStore.enqueue(connection, kind, folderID: folderID, path: path, extra: extra)
	}

	/// Removes queued work that is no longer needed (optionally also for the paths below the path)
	public func dequeue(_ kind: WorkKind, folderID: String, path: String, includingDescendants: Bool = false)
		throws(IndexError)
	{
		let path = FileNames.normalized(path)
		try connection.run(
			"DELETE FROM work WHERE kind = ? AND folder_id = ? AND path = ?", [kind.rawValue, folderID, path])
		if includingDescendants {
			let (condition, values) = Self.subtree(below: path)
			try connection.run(
				"DELETE FROM work WHERE kind = ? AND folder_id = ? AND \(condition)",
				[kind.rawValue, folderID] + values)
		}
	}

	/// Points queued work at a new destination (e.g. a rename that is renamed again before the app completed it)
	public func retargetQueuedWork(_ kind: WorkKind, folderID: String, from oldExtra: String, to newExtra: String)
		throws(IndexError)
	{
		try connection.run(
			"UPDATE work SET extra = ? WHERE kind = ? AND folder_id = ? AND extra = ?",
			[FileNames.normalized(newExtra), kind.rawValue, folderID, FileNames.normalized(oldExtra)])
	}
}
