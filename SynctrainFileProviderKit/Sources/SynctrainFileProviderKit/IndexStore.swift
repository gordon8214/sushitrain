// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The shared index of the folders on gateways, which the File Provider extension serves from and the app shows. Every
/// call opens the database and closes it again.
public struct IndexStore: Sendable {
	public let url: URL

	static let schemaVersion = 1

	static let itemColumns = """
		id, folder_id, path, parent_id, name, kind, size, mod_ns, blocks_hash, content_rev, meta_rev, changed_seq, \
		pending, deleted
		"""

	public init(url: URL) {
		self.url = url
	}

	public init(container: SharedContainer) {
		url = container.indexURL
	}

	func withConnection<T>(_ block: (SQLiteConnection) throws(IndexError) -> T) throws(IndexError) -> T {
		let connection = try SQLiteConnection(url: url)
		try Self.createSchemaIfNeeded(connection)
		return try block(connection)
	}

	func transaction<T>(_ block: (SQLiteConnection) throws(IndexError) -> T) throws(IndexError) -> T {
		try withConnection { connection throws(IndexError) in
			try connection.transaction { () throws(IndexError) in try block(connection) }
		}
	}

	/// Creates the tables of a new database. A database of any other version is not this app's to change.
	static func createSchemaIfNeeded(_ connection: SQLiteConnection) throws(IndexError) {
		let version = try connection.userVersion
		if version == schemaVersion {
			return
		}
		if version != 0 {
			throw .unsupportedSchema(version)
		}
		try connection.transaction { () throws(IndexError) in
			// Another process may have created it in the meantime
			if try connection.userVersion == schemaVersion {
				return
			}
			try createItemSchema(connection)
			try createRemoteSchema(connection)
			try connection.setUserVersion(schemaVersion)
		}
	}

	private static func createItemSchema(_ connection: SQLiteConnection) throws(IndexError) {
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
				content_rev INTEGER NOT NULL DEFAULT 1,
				meta_rev INTEGER NOT NULL DEFAULT 1,
				changed_seq INTEGER NOT NULL,
				pending INTEGER NOT NULL DEFAULT 0,
				deleted INTEGER NOT NULL DEFAULT 0,
				deleted_at REAL
			);
			CREATE UNIQUE INDEX IF NOT EXISTS items_live_path ON items(folder_id, path) WHERE deleted = 0;
			CREATE INDEX IF NOT EXISTS items_tombstone_path ON items(folder_id, path) WHERE deleted = 1;
			CREATE INDEX IF NOT EXISTS items_children ON items(parent_id, deleted, id);
			CREATE INDEX IF NOT EXISTS items_changed ON items(changed_seq);
			CREATE INDEX IF NOT EXISTS items_name_key ON items(folder_id, parent_id, name_key) WHERE deleted = 0;
			CREATE INDEX IF NOT EXISTS items_pending ON items(folder_id, pending) WHERE deleted = 0;
			CREATE INDEX IF NOT EXISTS items_pending_tombstones ON items(folder_id)
				WHERE deleted = 1 AND pending != 0;
			CREATE INDEX IF NOT EXISTS items_child_name ON items(parent_id, name_key) WHERE deleted = 0;
			CREATE TABLE IF NOT EXISTS folders (
				folder_id TEXT PRIMARY KEY,
				root_id INTEGER NOT NULL,
				label TEXT NOT NULL
			);
			CREATE TABLE IF NOT EXISTS item_attributes (
				item_id INTEGER PRIMARY KEY,
				tag_data TEXT,
				favorite_rank INTEGER,
				last_used REAL
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
			contentRevision: row.int64(9),
			metadataRevision: row.int64(10),
			changedSequence: row.int64(11),
			pending: PendingChange(rawValue: Int(row.int64(12))) ?? .none,
			isDeleted: row.bool(13)
		)
	}

	static func folder(from row: SQLiteStatement) -> IndexFolder {
		IndexFolder(folderID: row.text(0), rootItemID: row.int64(1), label: row.text(2))
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
				"SELECT folder_id, root_id, label FROM folders ORDER BY label, folder_id", [], Self.folder)
		}
	}

	public func folder(id: String) throws(IndexError) -> IndexFolder? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT folder_id, root_id, label FROM folders WHERE folder_id = ?", [id], Self.folder)
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
			try IndexPaths.child(connection, of: parentID, named: name)
		}
	}

	/// All live items below a directory. This loads the whole subtree; to check for some, use `hasDescendants(of:)`.
	public func descendants(of item: IndexItem) throws(IndexError) -> [IndexItem] {
		try withConnection { connection throws(IndexError) in try IndexPaths.descendants(connection, of: item) }
	}

	/// Whether there are live items below a directory
	public func hasDescendants(of item: IndexItem) throws(IndexError) -> Bool {
		let (condition, values) = IndexPaths.subtree(below: item.path)
		return try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT EXISTS (SELECT 1 FROM items WHERE folder_id = ? AND deleted = 0 AND \(condition) LIMIT 1)",
				[item.folderID] + values, { $0.bool(0) }) ?? false
		}
	}

	// MARK: - Writing

	/// Runs the block with a writer in a single transaction
	@discardableResult
	func write<T>(_ block: (IndexWriter) throws(IndexError) -> T) throws(IndexError) -> T {
		try transaction { connection throws(IndexError) in
			let writer = try IndexWriter(connection: connection)
			let result = try block(writer)
			try writer.finish()
			return result
		}
	}

	/// Permanently removes tombstones older than the date (see `IndexWriter.pruneTombstones`)
	public func pruneTombstones(before date: Date) throws(IndexError) {
		try write { writer throws(IndexError) in try writer.pruneTombstones(before: date) }
	}

	/// Starts a new generation, which invalidates all anchors handed out before, so that the system reads everything
	/// again. Items keep their identifiers and gateway item mappings. Refused while any operation is unresolved.
	public func reset() throws(IndexError) {
		try transaction { connection throws(IndexError) in
			if try Self.hasUnresolvedRemoteMutations(connection) {
				throw .notAllowed("changes to folders on a gateway are still being sent")
			}
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
}
