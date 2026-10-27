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

	static let schemaVersion = 1

	static let itemColumns = """
		id, folder_id, path, parent_id, name, kind, size, mod_ns, blocks_hash, modified_by, content_rev, meta_rev, \
		changed_seq, selected, implied, pending, deleted
		"""

	public init(url: URL) {
		self.url = url
	}

	public init(container: SharedContainer) {
		self.init(url: container.indexURL)
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

	static func migrate(_ connection: SQLiteConnection) throws(IndexError) {
		let version = try connection.userVersion
		if version == schemaVersion {
			return
		}
		if version > schemaVersion {
			throw .unsupportedSchema(version)
		}

		try connection.transaction { () throws(IndexError) in
			// Another process may have migrated in the meantime
			let version = try connection.userVersion
			if version >= schemaVersion {
				return
			}
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
			try connection.run(
				"INSERT OR IGNORE INTO meta (key, value) VALUES ('generation', ?)", [UUID().uuidString])
			try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('next_seq', '1')")
			try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('pruned_below', '0')")
			try connection.setUserVersion(schemaVersion)
		}
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
				[folderID, path], Self.item)
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
			return try connection.query(
				"""
				SELECT \(Self.itemColumns) FROM items
				WHERE kind = \(ItemKind.folderRoot.rawValue) AND deleted = 0 AND id > ? ORDER BY id LIMIT ?
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
			let generation = try Self.generation(connection)
			let prunedBelow = Int64(try Self.meta(connection, "pruned_below") ?? "0") ?? 0
			let current = try Self.currentSequence(connection)
			if anchor.generation != generation || anchor.sequence < prunedBelow || anchor.sequence > current {
				throw .anchorExpired
			}

			let rows = try connection.query(
				"SELECT \(Self.itemColumns) FROM items WHERE changed_seq > ? ORDER BY changed_seq LIMIT ?",
				[anchor.sequence, limit], Self.item)
			let hasMore = rows.count == limit
			let lastSequence = hasMore ? (rows.last?.changedSequence ?? anchor.sequence) : current
			return ChangeSet(
				updated: rows.filter { !$0.isDeleted },
				deleted: rows.filter(\.isDeleted),
				anchor: SyncAnchor(generation: generation, sequence: max(lastSequence, anchor.sequence)),
				hasMore: hasMore
			)
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
			let writer = try IndexWriter(connection: connection)
			let result = try block(writer)
			try writer.finish()
			return result
		}
	}

	/// Removes all items and folders and starts a new generation, which invalidates all anchors handed out before
	public func reset() throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try connection.run("DELETE FROM items")
			try connection.run("DELETE FROM folders")
			try Self.setMeta(connection, "generation", UUID().uuidString)
			try Self.setMeta(connection, "pruned_below", "0")
		}
	}

	// MARK: - Work queue

	/// Queues work for the app. Identical work that is already queued is not queued again.
	public func enqueue(_ kind: WorkKind, folderID: String, path: String, extra: String? = nil) throws(IndexError) {
		try transaction { connection throws(IndexError) in
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
	}

	public func pendingWork(limit: Int = 100) throws(IndexError) -> [WorkItem] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT id, kind, folder_id, path, extra, attempts, last_error, created_at FROM work ORDER BY id LIMIT ?",
				[limit]
			) { row in
				WorkItem(
					id: row.int64(0), kind: WorkKind(rawValue: row.text(1)) ?? .rescan, folderID: row.text(2),
					path: row.text(3), extra: row.optionalText(4), attempts: Int(row.int64(5)),
					lastError: row.optionalText(6), createdAt: Date(timeIntervalSince1970: row.double(7)))
			}
		}
	}

	public func completeWork(id: Int64) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM work WHERE id = ?", [id])
		}
	}

	public func failWork(id: Int64, error: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE work SET attempts = attempts + 1, last_error = ? WHERE id = ?", [error, id])
		}
	}
}
