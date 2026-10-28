// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The shared mirror of the Syncthing global index that the File Provider extension serves from. The app writes it,
/// the extension reads it (and records its own changes in it). Every call opens the database and closes it again.
public struct IndexStore: Sendable {
	public let url: URL
	/// Where the folders are, to compare Syncthing's records with the files on disk (nil when only the index is used)
	let container: SharedContainer?

	static let schemaVersion = 7

	static let itemColumns = """
		id, folder_id, path, parent_id, name, kind, size, mod_ns, blocks_hash, modified_by, content_rev, meta_rev, \
		changed_seq, selected, implied, pending, deleted
		"""

	public init(url: URL) {
		self.url = url
		container = nil
	}

	public init(container: SharedContainer) {
		url = container.indexURL
		self.container = container
	}

	func withConnection<T>(_ block: (SQLiteConnection) throws(IndexError) -> T) throws(IndexError) -> T {
		let connection = try SQLiteConnection(url: url)
		try Self.migrate(connection)
		return try block(connection)
	}

	func transaction<T>(_ block: (SQLiteConnection) throws(IndexError) -> T) throws(IndexError) -> T {
		try withConnection { connection throws(IndexError) in
			try connection.transaction { () throws(IndexError) in try block(connection) }
		}
	}

	/// Brings the database to the schema version (the current one, except in tests of older versions)
	static func migrate(_ connection: SQLiteConnection, to target: Int = schemaVersion) throws(IndexError) {
		let version = try connection.userVersion
		if version == target {
			return
		}
		if version > schemaVersion {
			throw .unsupportedSchema(version)
		}

		try connection.transaction { () throws(IndexError) in
			// Another process may have migrated in the meantime
			let version = try connection.userVersion
			if version >= target {
				return
			}
			if version < 1 {
				try createInitialSchema(connection)
			}
			if version < 2, target >= 2 {
				// For changes made through the File Provider: where a renamed item was before, and the Syncthing
				// sequence the folder's index was at (records up to it predate the change)
				try connection.execute(
					"""
					ALTER TABLE items ADD COLUMN pending_path TEXT;
					ALTER TABLE items ADD COLUMN pending_after INTEGER NOT NULL DEFAULT 0;
					""")
			}
			if version < 3, target >= 3 {
				try connection.execute(
					"""
					CREATE INDEX IF NOT EXISTS items_implied ON items(folder_id) WHERE implied = 1 AND deleted = 0;
					CREATE INDEX IF NOT EXISTS items_pending ON items(folder_id, pending) WHERE deleted = 0;
					CREATE INDEX IF NOT EXISTS items_pending_tombstones ON items(folder_id)
						WHERE deleted = 1 AND pending != 0;
					CREATE INDEX IF NOT EXISTS items_child_name ON items(parent_id, name_key) WHERE deleted = 0;
					""")
				// Items below a renamed directory no longer become pending renames themselves (without an old path,
				// nothing could confirm them); the rename of the directory covers them
				try connection.run(
					"""
					UPDATE items SET pending = 0, pending_size = NULL, pending_mod_ns = NULL
					WHERE deleted = 0 AND pending = \(PendingChange.rename.rawValue) AND pending_path IS NULL
					""")
				try canonicalizePaths(connection)
			}
			if version < 4, target >= 4 {
				// What the system keeps about items but Syncthing does not synchronize (tags, favorites, recents)
				try connection.execute(
					"""
					CREATE TABLE IF NOT EXISTS item_attributes (
						item_id INTEGER PRIMARY KEY,
						tag_data TEXT,
						favorite_rank INTEGER,
						last_used REAL
					);
					""")
			}
			if version < 5, target >= 5 {
				// Folders accessed through a gateway's remote file API (see RemoteStore.swift)
				try createRemoteSchema(connection)
			}
			if version < 6, target >= 6 {
				try addOutboxConflictPolicy(connection)
			}
			if version < 7, target >= 7 {
				try addOriginsAndDiscards(connection)
			}
			try connection.setUserVersion(target)
		}
	}

	private static func createInitialSchema(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute(
			"""
			CREATE TABLE IF NOT EXISTS items (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				folder_id TEXT NOT NULL,
				path TEXT NOT NULL,
				parent_id INTEGER,
				name TEXT NOT NULL,
				name_key TEXT NOT NULL,
				kind INTEGER NOT NULL,
				size INTEGER NOT NULL DEFAULT 0,
				mod_ns INTEGER NOT NULL DEFAULT 0,
				blocks_hash TEXT,
				modified_by TEXT,
				st_seq INTEGER NOT NULL DEFAULT 0,
				content_rev INTEGER NOT NULL DEFAULT 1,
				meta_rev INTEGER NOT NULL DEFAULT 1,
				changed_seq INTEGER NOT NULL,
				selected INTEGER NOT NULL DEFAULT 0,
				implied INTEGER NOT NULL DEFAULT 0,
				pending INTEGER NOT NULL DEFAULT 0,
				pending_size INTEGER,
				pending_mod_ns INTEGER,
				deleted INTEGER NOT NULL DEFAULT 0,
				deleted_at REAL,
				sweep_mark INTEGER NOT NULL DEFAULT 0
			);
			CREATE UNIQUE INDEX IF NOT EXISTS items_live_path ON items(folder_id, path) WHERE deleted = 0;
			CREATE INDEX IF NOT EXISTS items_tombstone_path ON items(folder_id, path) WHERE deleted = 1;
			CREATE INDEX IF NOT EXISTS items_children ON items(parent_id, deleted, id);
			CREATE INDEX IF NOT EXISTS items_changed ON items(changed_seq);
			CREATE INDEX IF NOT EXISTS items_name_key ON items(folder_id, parent_id, name_key) WHERE deleted = 0;
			CREATE TABLE IF NOT EXISTS folders (
				folder_id TEXT PRIMARY KEY,
				root_id INTEGER NOT NULL,
				label TEXT NOT NULL,
				folder_type TEXT NOT NULL,
				selective INTEGER NOT NULL,
				cursor INTEGER NOT NULL DEFAULT 0
			);
			CREATE TABLE IF NOT EXISTS work (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				kind TEXT NOT NULL,
				folder_id TEXT NOT NULL,
				path TEXT NOT NULL,
				extra TEXT,
				attempts INTEGER NOT NULL DEFAULT 0,
				last_error TEXT,
				created_at REAL NOT NULL
			);
			CREATE TABLE IF NOT EXISTS meta (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL
			);
			""")
		try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('generation', ?)", [UUID().uuidString])
		try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('next_seq', '1')")
		try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('pruned_below', '0')")
	}

	/// Paths are stored canonically decomposed since version 3 (see `FileNames.normalized`). Before, the extension
	/// recorded what it created in composed form, which Syncthing's entries (decomposed) never matched. Such rows are
	/// converted; a live row whose converted path is taken (by Syncthing's entry for the same file) is removed, and what
	/// was below it moves to the entry that took its place.
	private static func canonicalizePaths(_ connection: SQLiteConnection) throws(IndexError) {
		var nextSequence = Int64(try meta(connection, "next_seq") ?? "1") ?? 1
		let now = Date().timeIntervalSince1970
		var afterID: Int64 = 0
		while true {
			// Only paths with characters outside printable ASCII can change
			let rows = try connection.query(
				"""
				SELECT id, folder_id, path, pending_path, deleted FROM items
				WHERE id > ? AND kind != \(ItemKind.folderRoot.rawValue)
					AND (path GLOB '*[^ -~]*' OR pending_path GLOB '*[^ -~]*')
				ORDER BY id LIMIT 500
				""", [afterID]
			) { row in
				(
					id: row.int64(0), folderID: row.text(1), path: row.text(2), pendingPath: row.optionalText(3),
					deleted: row.bool(4)
				)
			}
			guard let last = rows.last else {
				break
			}
			afterID = last.id

			for row in rows {
				let path = FileNames.normalized(row.path)
				let pendingPath = row.pendingPath.map(FileNames.normalized)
				// Swift compares strings by canonical equivalence; the index compares bytes
				let pathChanged = !path.utf8.elementsEqual(row.path.utf8)
				let pendingPathChanged = !(pendingPath ?? "").utf8.elementsEqual((row.pendingPath ?? "").utf8)
				guard pathChanged || pendingPathChanged else {
					continue
				}
				defer { nextSequence += 1 }
				if pathChanged, !row.deleted,
					try connection.queryOne(
						"SELECT id FROM items WHERE folder_id = ? AND path = ? AND deleted = 0", [row.folderID, path],
						{ $0.int64(0) }) != nil
				{
					try connection.run(
						"""
						UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_path = NULL,
							pending_size = NULL, pending_mod_ns = NULL, meta_rev = meta_rev + 1, changed_seq = ?
						WHERE id = ?
						""", [now, nextSequence, row.id])
					continue
				}
				try connection.run(
					"""
					UPDATE items SET path = ?, name = ?, pending_path = ?, meta_rev = meta_rev + 1, changed_seq = ?
					WHERE id = ?
					""", [path, FileNames.lastComponent(path), pendingPath, nextSequence, row.id])
			}
		}

		let orphans = try connection.query(
			"""
			SELECT c.id, c.folder_id, c.path FROM items AS c JOIN items AS p ON p.id = c.parent_id
			WHERE c.deleted = 0 AND p.deleted = 1
			"""
		) { (id: $0.int64(0), folderID: $0.text(1), path: $0.text(2)) }
		for orphan in orphans {
			let parentPath = FileNames.parentPath(orphan.path)
			let parentID =
				parentPath.isEmpty
				? try connection.queryOne(
					"SELECT root_id FROM folders WHERE folder_id = ?", [orphan.folderID], { $0.int64(0) })
				: try connection.queryOne(
					"SELECT id FROM items WHERE folder_id = ? AND path = ? AND deleted = 0",
					[orphan.folderID, parentPath], { $0.int64(0) })
			if let parentID {
				try connection.run("UPDATE items SET parent_id = ? WHERE id = ?", [parentID, orphan.id])
			}
		}
		try setMeta(connection, "next_seq", String(nextSequence))
	}

	static func item(from row: SQLiteStatement) -> IndexItem {
		IndexItem(
			id: row.int64(0),
			folderID: row.text(1),
			path: row.text(2),
			parentID: row.optionalInt64(3),
			name: row.text(4),
			kind: ItemKind(rawValue: Int(row.int64(5))) ?? .file,
			size: row.int64(6),
			modifiedNanoseconds: row.int64(7),
			blocksHash: row.optionalText(8),
			modifiedBy: row.optionalText(9),
			contentRevision: row.int64(10),
			metadataRevision: row.int64(11),
			changedSequence: row.int64(12),
			isSelected: row.bool(13),
			isImplied: row.bool(14),
			pending: PendingChange(rawValue: Int(row.int64(15))) ?? .none,
			isDeleted: row.bool(16)
		)
	}

	static func folder(from row: SQLiteStatement) -> IndexFolder {
		IndexFolder(
			folderID: row.text(0), rootItemID: row.int64(1), label: row.text(2), folderType: row.text(3),
			isSelective: row.bool(4))
	}

	static func meta(_ connection: SQLiteConnection, _ key: String) throws(IndexError) -> String? {
		try connection.queryOne("SELECT value FROM meta WHERE key = ?", [key]) { $0.text(0) }
	}

	static func setMeta(_ connection: SQLiteConnection, _ key: String, _ value: String) throws(IndexError) {
		try connection.run("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)", [key, value])
	}

	static func generation(_ connection: SQLiteConnection) throws(IndexError) -> UUID {
		guard let string = try meta(connection, "generation"), let uuid = UUID(uuidString: string) else {
			throw .sqlite(code: 0, message: "index has no generation")
		}
		return uuid
	}

	/// The highest sequence handed out so far. This is not MAX(changed_seq), which goes down when tombstones are pruned.
	static func currentSequence(_ connection: SQLiteConnection) throws(IndexError) -> Int64 {
		(Int64(try meta(connection, "next_seq") ?? "1") ?? 1) - 1
	}

	// MARK: - Reading

	/// The live item with the given ID
	public func item(id: Int64) throws(IndexError) -> IndexItem? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.itemColumns) FROM items WHERE id = ? AND deleted = 0", [id], Self.item)
		}
	}

	/// The live item at the given path in the folder ("" for the folder root)
	public func item(folderID: String, path: String) throws(IndexError) -> IndexItem? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.itemColumns) FROM items WHERE folder_id = ? AND path = ? AND deleted = 0",
				[folderID, FileNames.normalized(path)], Self.item)
		}
	}

	public func folders() throws(IndexError) -> [IndexFolder] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT folder_id, root_id, label, folder_type, selective FROM folders ORDER BY label, folder_id", [],
				Self.folder)
		}
	}

	public func folder(id: String) throws(IndexError) -> IndexFolder? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT folder_id, root_id, label, folder_type, selective FROM folders WHERE folder_id = ?", [id],
				Self.folder)
		}
	}

	/// Live children of a container in ID order, starting after the given ID. A nil parent lists the folder roots.
	public func children(of parentID: Int64?, after afterID: Int64 = 0, limit: Int) throws(IndexError) -> [IndexItem] {
		try withConnection { connection throws(IndexError) in
			if let parentID {
				return try connection.query(
					"""
					SELECT \(Self.itemColumns) FROM items
					WHERE parent_id = ? AND deleted = 0 AND id > ? ORDER BY id LIMIT ?
					""", [parentID, afterID, limit], Self.item)
			}
			// Only folder roots have no parent, which lets this use the index of children
			return try connection.query(
				"""
				SELECT \(Self.itemColumns) FROM items
				WHERE parent_id IS NULL AND deleted = 0 AND id > ? AND kind = \(ItemKind.folderRoot.rawValue)
				ORDER BY id LIMIT ?
				""", [afterID, limit], Self.item)
		}
	}

	/// All live items in ID order, starting after the given ID
	public func liveItems(after afterID: Int64 = 0, limit: Int) throws(IndexError) -> [IndexItem] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.itemColumns) FROM items WHERE deleted = 0 AND id > ? ORDER BY id LIMIT ?",
				[afterID, limit], Self.item)
		}
	}

	/// The anchor that represents the current state of the index
	public func currentAnchor() throws(IndexError) -> SyncAnchor {
		try withConnection { connection throws(IndexError) in
			SyncAnchor(generation: try Self.generation(connection), sequence: try Self.currentSequence(connection))
		}
	}

	/// Changes since the anchor, oldest first. Throws `.anchorExpired` when the anchor can no longer be served.
	public func changes(since anchor: SyncAnchor, limit: Int) throws(IndexError) -> ChangeSet {
		try withConnection { connection throws(IndexError) in
			// One read transaction, so that the anchor matches the rows
			try connection.readTransaction { () throws(IndexError) in
				try Self.changes(connection, since: anchor, limit: limit)
			}
		}
	}

	private static func changes(_ connection: SQLiteConnection, since anchor: SyncAnchor, limit: Int)
		throws(IndexError) -> ChangeSet
	{
		let generation = try generation(connection)
		let prunedBelow = Int64(try meta(connection, "pruned_below") ?? "0") ?? 0
		let current = try currentSequence(connection)
		if anchor.generation != generation || anchor.sequence < prunedBelow || anchor.sequence > current {
			throw .anchorExpired
		}

		let rows = try connection.query(
			"SELECT \(itemColumns) FROM items WHERE changed_seq > ? ORDER BY changed_seq LIMIT ?",
			[anchor.sequence, limit], item)
		let hasMore = rows.count == limit
		let lastSequence = hasMore ? (rows.last?.changedSequence ?? anchor.sequence) : current
		return ChangeSet(
			updated: rows.filter { !$0.isDeleted },
			deleted: rows.filter(\.isDeleted),
			anchor: SyncAnchor(generation: generation, sequence: max(lastSequence, anchor.sequence)),
			hasMore: hasMore
		)
	}

	/// The live item in the container whose name cannot coexist with the given name
	public func child(of parentID: Int64, named name: String) throws(IndexError) -> IndexItem? {
		try withConnection { connection throws(IndexError) in
			try LocalChangeRecorder.child(connection, of: parentID, named: name)
		}
	}

	/// All live items below a directory. This loads the whole subtree; to check for some, use `hasDescendants(of:)`.
	public func descendants(of item: IndexItem) throws(IndexError) -> [IndexItem] {
		try withConnection { connection throws(IndexError) in try LocalChangeRecorder.descendants(connection, of: item) }
	}

	/// Whether there are live items below a directory (only ones that are not kept on this device, if requested)
	public func hasDescendants(of item: IndexItem, unselectedOnly: Bool = false) throws(IndexError) -> Bool {
		let (condition, values) = LocalChangeRecorder.subtree(below: item.path)
		return try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT EXISTS (
					SELECT 1 FROM items WHERE folder_id = ? AND deleted = 0 AND \(condition)
						\(unselectedOnly ? "AND selected = 0" : "") LIMIT 1)
				""", [item.folderID] + values, { $0.bool(0) }) ?? false
		}
	}

	/// The versions of the live files below a directory, by path relative to the folder root (like `IndexItem.path`)
	public func descendantFiles(of item: IndexItem) throws(IndexError) -> [String: FileVersion] {
		let (condition, values) = LocalChangeRecorder.subtree(below: item.path)
		return try withConnection { connection throws(IndexError) in
			let stmt = try connection.statement(
				"""
				SELECT path, size, mod_ns FROM items
				WHERE folder_id = ? AND deleted = 0 AND kind = \(ItemKind.file.rawValue) AND \(condition)
				""", [item.folderID] + values)
			var files: [String: FileVersion] = [:]
			while try stmt.step() {
				files[stmt.text(0)] = FileVersion(size: stmt.int64(1), modifiedNanoseconds: stmt.int64(2))
			}
			return files
		}
	}

	/// The short device ID of this device, as recorded by the app
	public func localShortDeviceID() throws(IndexError) -> String? {
		try withConnection { connection throws(IndexError) in try Self.meta(connection, "local_short_id") }
	}

	// MARK: - Writing (app side)

	/// Runs the block with a writer in a single transaction
	@discardableResult
	public func write<T>(_ block: (IndexWriter) throws(IndexError) -> T) throws(IndexError) -> T {
		try transaction { connection throws(IndexError) in
			let writer = try IndexWriter(connection: connection, container: container)
			let result = try block(writer)
			try writer.finish()
			return result
		}
	}

	/// Removes all items and folders and starts a new generation, which invalidates all anchors handed out before.
	/// Folders accessed through a gateway keep their items (and so their gateway item mappings), overlays, outbox and
	/// staged files; the reset is refused while any of their operations is unresolved.
	public func reset() throws(IndexError) {
		try transaction { connection throws(IndexError) in
			if try Self.hasUnresolvedRemoteMutations(connection) {
				throw .notAllowed("changes to folders on a gateway are still being sent")
			}
			// A switch in progress resumes from its journal and the rows it fenced, which a reset would drop
			if try connection.queryOne("SELECT EXISTS (SELECT 1 FROM remote_migrations)", [], { $0.bool(0) }) == true {
				throw .notAllowed("a folder is being switched between Syncthing and a gateway")
			}
			let kept = "SELECT folder_id FROM remote_bindings WHERE access_mode = '\(RemoteAccessMode.remoteAPI.rawValue)'"
			try connection.run(
				"DELETE FROM item_attributes WHERE item_id IN (SELECT id FROM items WHERE folder_id NOT IN (\(kept)))")
			try connection.run("DELETE FROM items WHERE folder_id NOT IN (\(kept))")
			try connection.run("DELETE FROM folders WHERE folder_id NOT IN (\(kept))")
			try Self.setMeta(connection, "generation", UUID().uuidString)
			try Self.setMeta(connection, "pruned_below", "0")
		}
	}

	// MARK: - Attributes

	/// The attributes the system keeps for the items (only those that have any)
	public func attributes(of itemIDs: [Int64]) throws(IndexError) -> [Int64: ItemAttributes] {
		guard !itemIDs.isEmpty else {
			return [:]
		}
		return try withConnection { connection throws(IndexError) in
			var result: [Int64: ItemAttributes] = [:]
			// Within SQLite's limit on the number of parameters
			for start in stride(from: 0, to: itemIDs.count, by: 500) {
				let ids = Array(itemIDs[start..<min(start + 500, itemIDs.count)])
				let placeholders = Array(repeating: "?", count: ids.count).joined(separator: ", ")
				let rows = try connection.query(
					"""
					SELECT item_id, tag_data, favorite_rank, last_used FROM item_attributes
					WHERE item_id IN (\(placeholders))
					""", ids
				) { row in
					(
						row.int64(0),
						ItemAttributes(
							tagData: row.optionalText(1).flatMap { Data(base64Encoded: $0) },
							favoriteRank: row.optionalInt64(2),
							lastUsedDate: row.isNull(3) ? nil : Date(timeIntervalSince1970: row.double(3)))
					)
				}
				for (id, attributes) in rows {
					result[id] = attributes
				}
			}
			return result
		}
	}

	public func setAttributes(_ attributes: ItemAttributes, of itemID: Int64) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			if attributes.isEmpty {
				try connection.run("DELETE FROM item_attributes WHERE item_id = ?", [itemID])
			}
			else {
				try connection.run(
					"""
					INSERT OR REPLACE INTO item_attributes (item_id, tag_data, favorite_rank, last_used)
					VALUES (?, ?, ?, ?)
					""",
					[
						itemID, attributes.tagData?.base64EncodedString(), attributes.favoriteRank,
						attributes.lastUsedDate?.timeIntervalSince1970,
					])
			}
		}
	}

	// MARK: - Work queue

	static let workColumns = "id, kind, folder_id, path, extra, attempts, last_error, created_at"

	/// Queues work for the app. Identical work that is already queued is not queued again.
	public func enqueue(_ kind: WorkKind, folderID: String, path: String, extra: String? = nil) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try Self.enqueue(connection, kind, folderID: folderID, path: path, extra: extra)
		}
	}

	static func enqueue(_ connection: SQLiteConnection, _ kind: WorkKind, folderID: String, path: String, extra: String?)
		throws(IndexError)
	{
		// Paths (and the new path of a rename, in extra) are stored like the paths of items
		let path = FileNames.normalized(path)
		let extra = extra.map(FileNames.normalized)
		let existing = try connection.queryOne(
			"SELECT id FROM work WHERE kind = ? AND folder_id = ? AND path = ? AND extra IS ?",
			[kind.rawValue, folderID, path, extra]
		) { $0.int64(0) }
		if existing == nil {
			try connection.run(
				"INSERT INTO work (kind, folder_id, path, extra, created_at) VALUES (?, ?, ?, ?, ?)",
				[kind.rawValue, folderID, path, extra, Date().timeIntervalSince1970])
		}
	}

	/// Queued work, oldest first (only of the given kinds, when they are given)
	public func pendingWork(limit: Int = 100, kinds: [WorkKind]? = nil) throws(IndexError) -> [WorkItem] {
		if let kinds, kinds.isEmpty {
			return []
		}
		return try withConnection { connection throws(IndexError) in
			guard let kinds else {
				return try connection.query(
					"SELECT \(Self.workColumns) FROM work ORDER BY id LIMIT ?", [limit], Self.workItem)
			}
			let placeholders = Array(repeating: "?", count: kinds.count).joined(separator: ", ")
			return try connection.query(
				"SELECT \(Self.workColumns) FROM work WHERE kind IN (\(placeholders)) ORDER BY id LIMIT ?",
				kinds.map(\.rawValue) + [limit], Self.workItem)
		}
	}

	static func workItem(from row: SQLiteStatement) -> WorkItem {
		WorkItem(
			id: row.int64(0), kind: WorkKind(rawValue: row.text(1)) ?? .rescan, folderID: row.text(2),
			path: row.text(3), extra: row.optionalText(4), attempts: Int(row.int64(5)), lastError: row.optionalText(6),
			createdAt: Date(timeIntervalSince1970: row.double(7)))
	}

	/// Queued work of the kind with the given extra parameter (e.g. the rename that has this destination)
	public func queuedWork(_ kind: WorkKind, folderID: String, extra: String) throws(IndexError) -> WorkItem? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT \(Self.workColumns) FROM work
				WHERE kind = ? AND folder_id = ? AND extra = ? LIMIT 1
				""", [kind.rawValue, folderID, FileNames.normalized(extra)], Self.workItem)
		}
	}

	/// Queued renames in the folder whose old path (`path`) or new path (`extra`) is the given path, or lies above or
	/// below it (comparing whole path components)
	public func queuedRenames(folderID: String, touching path: String) throws(IndexError) -> [WorkItem] {
		let path = FileNames.normalized(path)
		return try withConnection { connection throws(IndexError) in
			if path.isEmpty {
				return try connection.query(
					"SELECT \(Self.workColumns) FROM work WHERE kind = ? AND folder_id = ? ORDER BY id",
					[WorkKind.finishRename.rawValue, folderID], Self.workItem)
			}
			let (lower, upper) = LocalChangeRecorder.descendantRange(of: path)
			return try connection.query(
				"""
				SELECT \(Self.workColumns) FROM work
				WHERE kind = ?1 AND folder_id = ?2 AND (
					path = ?3 OR (path >= ?4 AND path < ?5) OR (?3 >= path || '/' AND ?3 < path || '0')
					OR extra = ?3 OR (extra >= ?4 AND extra < ?5) OR (?3 >= extra || '/' AND ?3 < extra || '0'))
				ORDER BY id
				""", [WorkKind.finishRename.rawValue, folderID, path, lower, upper], Self.workItem)
		}
	}

	public func completeWork(id: Int64) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM work WHERE id = ?", [id])
		}
	}

	/// Removes queued work of the kind with the given extra parameter (e.g. a fetch of an item that has been fetched)
	public func completeWork(_ kind: WorkKind, extra: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"DELETE FROM work WHERE kind = ? AND extra = ?", [kind.rawValue, FileNames.normalized(extra)])
		}
	}

	public func failWork(id: Int64, error: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE work SET attempts = attempts + 1, last_error = ? WHERE id = ?", [error, id])
		}
	}
}
