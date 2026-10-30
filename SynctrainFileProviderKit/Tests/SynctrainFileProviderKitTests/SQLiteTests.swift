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
		try store.write { writer throws(IndexError) in
			try writer.upsertFolder(FolderDescription(folderID: "f1", label: "Photos"))
		}
		try store.withConnection { connection throws(IndexError) in
			try connection.transaction { () throws(IndexError) in
				for index in 0..<6000 {
					let name = "IMG_\(index).HEIC"
					try connection.run(
						"""
						INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, blocks_hash, changed_seq)
						VALUES ('f1', ?, 1, ?, ?, 0, ?, ?)
						""", [name, name, name.lowercased(), String(repeating: "q", count: 44), index + 2])
				}
			}
		}

		let writer = try SQLiteConnection(url: store.url)
		#expect(try writer.queryOne("PRAGMA cache_spill") { $0.int64(0) } == Int64(SQLiteConnection.spillThresholdPages))
		try writer.execute("BEGIN IMMEDIATE")
		// Rows spread over the whole table
		try writer.run("UPDATE items SET meta_rev = meta_rev + 1, changed_seq = changed_seq + 10000 WHERE id % 12 = 0")
		for index in 0..<500 {
			let name = "new-\(index).txt"
			try writer.run(
				"""
				INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, changed_seq)
				VALUES ('f1', ?, 1, ?, ?, 0, ?)
				""", [name, name, name, 20_000 + index])
		}

		let start = Date()
		#expect(try store.item(folderID: "f1", path: "IMG_1.HEIC") != nil)
		#expect(Date().timeIntervalSince(start) < 1)
		try writer.execute("ROLLBACK")
	}

	@Test func queriesUseIndexes() throws {
		let store = try makeStore()
		try store.write { writer throws(IndexError) in
			try writer.upsertFolder(FolderDescription(folderID: "f1", label: "Photos"))
		}
		let plans = [
			"items_children": """
			SELECT id FROM items WHERE parent_id IS NULL AND deleted = 0 AND id > 0 AND kind = 3 ORDER BY id LIMIT 10
			""",
			"items_child_name": "SELECT id FROM items WHERE parent_id = 1 AND name_key = 'x' AND deleted = 0 LIMIT 1",
			"items_pending": "SELECT id FROM items WHERE folder_id = 'f1' AND deleted = 0 AND pending = 4",
			"items_pending_tombstones": """
			SELECT id FROM items WHERE folder_id = 'f1' AND deleted = 1 AND pending != 0 AND pending = 3
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
