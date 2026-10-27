// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Hands a connection to another thread (the test makes sure only one thread uses it at a time)
private final class ConnectionBox: @unchecked Sendable {
	let connection: SQLiteConnection

	init(_ connection: SQLiteConnection) {
		self.connection = connection
	}
}

@Suite struct SQLiteTests {
	@Test func textWithNULRoundTrips() throws {
		let store = try makeStore()
		try store.withConnection { connection throws(IndexError) in
			let text = "a.jpg\0x\u{e9}"
			#expect(try connection.queryOne("SELECT ?", [text]) { $0.text(0) } == text)
			#expect(try connection.queryOne("SELECT length(CAST(? AS BLOB))", [text]) { $0.int64(0) } == 9)
			#expect(try connection.queryOne("SELECT ? = 'a.jpg'", [text]) { $0.bool(0) } == false)
			#expect(try connection.queryOne("SELECT ?", [""]) { $0.isNull(0) } == false)
		}
	}

	/// Opening the index while the other process holds a lock waits for it instead of failing right away
	@Test func openingWaitsForLocks() throws {
		let store = try makeStore()
		_ = try store.currentAnchor()
		let holder = ConnectionBox(try SQLiteConnection(url: store.url))
		try holder.connection.execute("BEGIN EXCLUSIVE")
		let released = DispatchSemaphore(value: 0)
		DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) {
			try? holder.connection.execute("COMMIT")
			released.signal()
		}
		let start = Date()
		#expect(try store.currentAnchor().sequence == 0)
		#expect(Date().timeIntervalSince(start) > 0.2)
		released.wait()
	}

	/// A batch of the app does not spill to the database file before it commits, which would lock the extension out
	@Test func batchesDoNotLockReadersOut() throws {
		let store = try makeStore()
		let records = (0..<6000).map {
			file("Photos 2024/Camera Roll/IMG_\($0).HEIC", seq: Int64($0 + 1), hash: String(repeating: "q", count: 44))
		}
		try store.applying(records)

		let writer = try SQLiteConnection(url: store.url)
		#expect(try writer.queryOne("PRAGMA cache_spill") { $0.int64(0) } == Int64(SQLiteConnection.spillThresholdPages))
		try writer.execute("BEGIN IMMEDIATE")
		let indexWriter = try IndexWriter(connection: writer)
		let token = try indexWriter.beginSweep(folderID: "f1")
		// Rows spread over the whole table, as in a full pass
		let scattered = stride(from: 0, to: records.count, by: 12).map { index in
			var record = records[index]
			record.full = false
			return record
		}
		try indexWriter.apply(scattered, folderID: "f1", sweepToken: token)
		try indexWriter.apply((0..<500).map { file("new/\($0).txt", seq: Int64(10_000 + $0)) }, folderID: "f1")

		let start = Date()
		#expect(try store.item(folderID: "f1", path: "Photos 2024") != nil)
		#expect(Date().timeIntervalSince(start) < 1)
		try writer.execute("ROLLBACK")
	}

	/// Version 2 as the app shipped it on this branch, with rows the extension recorded in composed form
	@Test func migratesVersion2Database() throws {
		let store = try makeStore()
		let connection = try SQLiteConnection(url: store.url)
		try connection.execute(
			"""
			CREATE TABLE items (
				id INTEGER PRIMARY KEY AUTOINCREMENT, folder_id TEXT NOT NULL, path TEXT NOT NULL, parent_id INTEGER,
				name TEXT NOT NULL, name_key TEXT NOT NULL, kind INTEGER NOT NULL, size INTEGER NOT NULL DEFAULT 0,
				mod_ns INTEGER NOT NULL DEFAULT 0, blocks_hash TEXT, modified_by TEXT, st_seq INTEGER NOT NULL DEFAULT 0,
				content_rev INTEGER NOT NULL DEFAULT 1, meta_rev INTEGER NOT NULL DEFAULT 1, changed_seq INTEGER NOT NULL,
				selected INTEGER NOT NULL DEFAULT 0, implied INTEGER NOT NULL DEFAULT 0, pending INTEGER NOT NULL DEFAULT 0,
				pending_size INTEGER, pending_mod_ns INTEGER, deleted INTEGER NOT NULL DEFAULT 0, deleted_at REAL,
				sweep_mark INTEGER NOT NULL DEFAULT 0
			);
			CREATE UNIQUE INDEX items_live_path ON items(folder_id, path) WHERE deleted = 0;
			CREATE INDEX items_tombstone_path ON items(folder_id, path) WHERE deleted = 1;
			CREATE INDEX items_children ON items(parent_id, deleted, id);
			CREATE INDEX items_changed ON items(changed_seq);
			CREATE INDEX items_name_key ON items(folder_id, parent_id, name_key) WHERE deleted = 0;
			CREATE TABLE folders (
				folder_id TEXT PRIMARY KEY, root_id INTEGER NOT NULL, label TEXT NOT NULL, folder_type TEXT NOT NULL,
				selective INTEGER NOT NULL, cursor INTEGER NOT NULL DEFAULT 0
			);
			CREATE TABLE work (
				id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, folder_id TEXT NOT NULL, path TEXT NOT NULL,
				extra TEXT, attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT, created_at REAL NOT NULL
			);
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			ALTER TABLE items ADD COLUMN pending_path TEXT;
			ALTER TABLE items ADD COLUMN pending_after INTEGER NOT NULL DEFAULT 0;
			INSERT INTO meta VALUES ('generation', '6F2A1C1E-8C7B-4E57-9B8F-6A0F7C5D2E11'), ('next_seq', '10'),
				('pruned_below', '0');
			INSERT INTO folders VALUES ('f1', 1, 'Photos', 'sendrecieve', 0, 5);
			""")
		let rows: [(id: Int64, path: String, parent: Int64?, kind: ItemKind, pending: PendingChange, deleted: Bool)] = [
			(1, "", nil, .folderRoot, .none, false),
			(2, "Cafe\u{301}", 1, .directory, .none, false),  // Syncthing's entry
			(3, "Caf\u{e9}", 1, .directory, .create, false),  // the same directory, as the extension recorded it
			(4, "Caf\u{e9}/x.txt", 3, .file, .create, false),
			(5, "R\u{e9}sum\u{e9}.txt", 1, .file, .create, false),
			(6, "d", 1, .directory, .rename, false),
			(7, "d/y.txt", 6, .file, .rename, false),  // below a renamed directory, without an old path
			(8, "Caf\u{e9}.old", 1, .file, .delete, true),
		]
		for row in rows {
			let name = row.id == 1 ? "Photos" : FileNames.lastComponent(row.path)
			try connection.run(
				"""
				INSERT INTO items (id, folder_id, path, parent_id, name, name_key, kind, changed_seq, pending, deleted,
					pending_path)
				VALUES (?, 'f1', ?, ?, ?, ?, ?, ?, ?, ?, ?)
				""",
				[
					row.id, row.path, row.parent, name, FileNames.collisionKey(name), row.kind.rawValue, row.id,
					row.pending.rawValue, row.deleted, row.id == 6 ? "c" : nil,
				])
		}
		try connection.setUserVersion(2)

		let generation = try #require(UUID(uuidString: "6F2A1C1E-8C7B-4E57-9B8F-6A0F7C5D2E11"))
		let anchor = SyncAnchor(generation: generation, sequence: 9)
		#expect(try store.currentAnchor().sequence > 9)
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == IndexStore.schemaVersion)
			let tables = try connection.query("SELECT name FROM sqlite_master WHERE type = 'table'") { $0.text(0) }
			#expect(tables.contains("item_attributes"))
			let indexes = try connection.query("SELECT name FROM sqlite_master WHERE type = 'index'") { $0.text(0) }
			for index in ["items_implied", "items_pending", "items_pending_tombstones", "items_child_name"] {
				#expect(indexes.contains(index))
			}
			let tombstonePath = try connection.queryOne("SELECT path FROM items WHERE id = 8") { $0.text(0) }
			#expect(tombstonePath.map { Array($0.utf8) } == Array("Cafe\u{301}.old".utf8))
		}

		// The twin is gone, and what was below it is below Syncthing's entry
		#expect(try store.item(id: 3) == nil && store.item(id: 2) != nil)
		let child = try #require(try store.item(id: 4))
		#expect(Array(child.path.utf8) == Array("Cafe\u{301}/x.txt".utf8) && child.parentID == 2)
		let resume = try #require(try store.item(id: 5))
		#expect(Array(resume.path.utf8) == Array("Re\u{301}sume\u{301}.txt".utf8))
		#expect(Array(resume.name.utf8) == Array("Re\u{301}sume\u{301}.txt".utf8))
		#expect(try store.item(id: 7)?.pending == PendingChange.none)
		#expect(try store.item(id: 6)?.pending == .rename)

		let changes = try store.changes(since: anchor, limit: 10)
		#expect(Set(changes.updated.map(\.id)) == [4, 5] && changes.deletedItemIDs.contains(3))
	}

	@Test func queriesUseIndexes() throws {
		let store = try makeStore()
		try store.applying([file("a/b.txt", seq: 1)])
		let plans = [
			"items_children": """
			SELECT id FROM items WHERE parent_id IS NULL AND deleted = 0 AND id > 0 AND kind = 3 ORDER BY id LIMIT 10
			""",
			"items_child_name": "SELECT id FROM items WHERE parent_id = 1 AND name_key = 'x' AND deleted = 0 LIMIT 1",
			"items_implied": """
			SELECT id FROM items AS i INDEXED BY items_implied WHERE folder_id = 'f1' AND implied = 1 AND deleted = 0 AND pending = 0
				AND NOT EXISTS (SELECT 1 FROM items AS c WHERE c.parent_id = i.id AND c.deleted = 0)
			""",
			"items_pending": "SELECT id FROM items WHERE folder_id = 'f1' AND deleted = 0 AND pending = 4",
			"items_pending_tombstones": """
			SELECT id FROM items WHERE folder_id = 'f1' AND deleted = 1 AND pending != 0 AND pending = 3
				AND pending_path IS NOT NULL
			""",
			"items_tombstone_path": """
			SELECT id FROM items WHERE folder_id = 'f1' AND path = 'x' AND deleted = 1 AND pending = 3
			ORDER BY changed_seq DESC LIMIT 1
			""",
		]
		try store.withConnection { connection throws(IndexError) in
			for (index, sql) in plans {
				let plan = try connection.query("EXPLAIN QUERY PLAN " + sql) { $0.text(3) }.joined(separator: "; ")
				#expect(plan.contains("INDEX \(index) "), "\(sql) -> \(plan)")
				#expect(!plan.contains("SCAN i ") && !plan.hasPrefix("SCAN items"), "\(sql) -> \(plan)")
			}
		}
	}
}
