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
		let (lower, upper) = descendantRange(of: item.path)
		return try connection.query(
			"""
			SELECT \(IndexStore.itemColumns) FROM items
			WHERE folder_id = ? AND deleted = 0 AND path >= ? AND path < ? ORDER BY path
			""", [item.folderID, lower, upper], IndexStore.item)
	}

	/// Paths below `path` sort between `path + "/"` and `path + "0"` ("0" follows "/" in byte order)
	static func descendantRange(of path: String) -> (String, String) {
		(path + "/", path + "0")
	}

	// MARK: - Recording

	/// Records an item created on disk. The item stays pending until Syncthing indexes a matching entry.
	public func recordCreation(
		folderID: String, parentID: Int64, path: String, kind: ItemKind, size: Int64, modifiedNanoseconds: Int64,
		isSelected: Bool
	) throws(IndexError) -> IndexItem {
		let name = FileNames.lastComponent(path)
		let size = kind == .file ? size : 0
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, changed_seq, selected,
				pending, pending_size, pending_mod_ns, pending_after)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT cursor FROM folders WHERE folder_id = ?))
			""",
			[
				folderID, path, parentID, name, FileNames.collisionKey(name), kind.rawValue, size, modifiedNanoseconds,
				allocateSequence(), isSelected, PendingChange.create.rawValue, size, modifiedNanoseconds, folderID,
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
		try recordFileState(item, size: item.size, modifiedNanoseconds: modifiedNanoseconds, contentChanged: false)
	}

	private func recordFileState(_ item: IndexItem, size: Int64, modifiedNanoseconds: Int64, contentChanged: Bool)
		throws(IndexError) -> IndexItem
	{
		// A creation or rename that Syncthing has not seen yet stays one; it now expects the new state
		let pending: PendingChange = item.pending == .none ? .modify : item.pending
		try connection.run(
			"""
			UPDATE items SET size = ?, mod_ns = ?, pending = ?, pending_size = ?, pending_mod_ns = ?,
				pending_after = (SELECT cursor FROM folders WHERE folder_id = items.folder_id), content_rev = content_rev + ?, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				size, modifiedNanoseconds, pending.rawValue, size, modifiedNanoseconds, contentChanged ? 1 : 0,
				allocateSequence(), item.id,
			])
		return try reload(item.id)
	}

	/// Records that an item (and everything below it) moved to a new path in the same folder. The item keeps its
	/// identifier. Until Syncthing has indexed the new path, records for the old path are ignored.
	public func recordMove(_ item: IndexItem, to parentID: Int64, path: String) throws(IndexError) -> IndexItem {
		let name = FileNames.lastComponent(path)
		let oldPath = item.path

		// Remember the path Syncthing knows the item by. A new item is not known to Syncthing at all, and an item that
		// was already moved keeps its original path (unless it moved back there).
		let originalPath = try connection.queryOne("SELECT pending_path FROM items WHERE id = ?", [item.id]) {
			$0.optionalText(0)
		}
		let isBackAtOriginalPath = item.pending == .rename && originalPath == path
		let pending: PendingChange =
			item.pending == .create ? .create : (isBackAtOriginalPath ? .modify : .rename)
		try connection.run(
			"""
			UPDATE items SET path = ?, parent_id = ?, name = ?, name_key = ?, pending = ?,
				pending_path = CASE WHEN ? = \(PendingChange.rename.rawValue) THEN COALESCE(pending_path, ?) END,
				pending_size = COALESCE(pending_size, size), pending_mod_ns = COALESCE(pending_mod_ns, mod_ns),
				pending_after = (SELECT cursor FROM folders WHERE folder_id = items.folder_id), meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""",
			[
				path, parentID, name, FileNames.collisionKey(name), pending.rawValue, pending.rawValue,
				item.pending == .rename ? nil : oldPath, allocateSequence(), item.id,
			])

		if item.kind.isContainer {
			// Everything below moves along; entries Syncthing knows become pending renames themselves, so that a full
			// pass does not remove them before Syncthing has scanned the new location
			let (lower, upper) = Self.descendantRange(of: oldPath)
			try connection.run(
				"""
				UPDATE items SET path = ? || substr(path, ?),
					pending = CASE WHEN pending = \(PendingChange.create.rawValue) THEN pending
						ELSE \(PendingChange.rename.rawValue) END,
					pending_size = COALESCE(pending_size, size), pending_mod_ns = COALESCE(pending_mod_ns, mod_ns),
					pending_after = (SELECT cursor FROM folders WHERE folder_id = items.folder_id)
				WHERE folder_id = ? AND deleted = 0 AND path >= ? AND path < ?
				""", [path, oldPath.unicodeScalars.count + 1, item.folderID, lower, upper])  // substr counts code points
		}
		return try reload(item.id)
	}

	/// Records that an item (and everything below it) moved to another folder. To Syncthing that is a removal from one
	/// folder and an addition to the other; the item keeps its identifier. The old folder gets tombstones for what
	/// Syncthing knows there, so that its records from before the move do not bring the items back. Items with a
	/// pending rename cannot be moved this way (their old paths are not all recorded).
	public func recordMove(_ item: IndexItem, toFolder folderID: String, parentID: Int64, path: String)
		throws(IndexError) -> IndexItem
	{
		let oldPath = item.path
		let moved = item.kind.isContainer ? [item] + (try descendants(of: item)) : [item]
		if moved.contains(where: { $0.pending == .rename }) {
			throw .notAllowed("items with a pending rename cannot be moved to another folder")
		}

		for entry in moved where entry.pending != .create {
			try connection.run(
				"""
				INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, size, mod_ns, changed_seq,
					deleted, deleted_at, pending, pending_size, pending_mod_ns, pending_after)
				SELECT folder_id, path, parent_id, name, name_key, kind, size, mod_ns, ?, 1, ?, ?, size, mod_ns,
					(SELECT cursor FROM folders WHERE folder_id = items.folder_id)
				FROM items WHERE id = ?
				""", [allocateSequence(), now, PendingChange.delete.rawValue, entry.id])
		}

		// In the new folder, everything is new to Syncthing
		let (lower, upper) = Self.descendantRange(of: oldPath)
		try connection.run(
			"""
			UPDATE items SET folder_id = ?, path = ? || substr(path, ?), pending = \(PendingChange.create.rawValue),
				pending_path = NULL, pending_size = COALESCE(pending_size, size),
				pending_mod_ns = COALESCE(pending_mod_ns, mod_ns),
				pending_after = (SELECT cursor FROM folders WHERE folder_id = ?)
			WHERE folder_id = ? AND deleted = 0 AND (path = ? OR (path >= ? AND path < ?))
			""",
			[
				folderID, path, oldPath.unicodeScalars.count + 1, folderID, item.folderID, oldPath, lower, upper,
			])  // substr counts code points
		let name = FileNames.lastComponent(path)
		try connection.run(
			"""
			UPDATE items SET parent_id = ?, name = ?, name_key = ?, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""", [parentID, name, FileNames.collisionKey(name), allocateSequence(), item.id])
		return try reload(item.id)
	}

	/// Records that an item (and everything below it) was deleted from disk. Until Syncthing has indexed the deletion,
	/// records of the deleted versions are ignored.
	public func recordDeletion(_ item: IndexItem) throws(IndexError) {
		var removed = [item]
		if item.kind.isContainer {
			removed += try descendants(of: item)
		}
		for entry in removed {
			// Something Syncthing never saw needs no confirmation
			let pending: PendingChange = entry.pending == .create ? .none : .delete
			try connection.run(
				"""
				UPDATE items SET deleted = 1, deleted_at = ?, pending = ?, pending_path = NULL,
					pending_size = size, pending_mod_ns = mod_ns, pending_after = (SELECT cursor FROM folders WHERE folder_id = items.folder_id),
					meta_rev = meta_rev + 1, changed_seq = ?
				WHERE id = ?
				""", [now, pending.rawValue, allocateSequence(), entry.id])
		}
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
		try connection.run(
			"DELETE FROM work WHERE kind = ? AND folder_id = ? AND path = ?", [kind.rawValue, folderID, path])
		if includingDescendants {
			let (lower, upper) = Self.descendantRange(of: path)
			try connection.run(
				"DELETE FROM work WHERE kind = ? AND folder_id = ? AND path >= ? AND path < ?",
				[kind.rawValue, folderID, lower, upper])
		}
	}

	/// Points queued work at a new destination (e.g. a rename that is renamed again before the app completed it)
	public func retargetQueuedWork(_ kind: WorkKind, folderID: String, from oldExtra: String, to newExtra: String)
		throws(IndexError)
	{
		try connection.run(
			"UPDATE work SET extra = ? WHERE kind = ? AND folder_id = ? AND extra = ?",
			[newExtra, kind.rawValue, folderID, oldExtra])
	}
}
