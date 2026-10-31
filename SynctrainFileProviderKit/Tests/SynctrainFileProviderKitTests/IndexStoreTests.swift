// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

func makeStore() throws -> IndexStore {
	let dir = FileManager.default.temporaryDirectory.appending(path: "kit-tests-\(UUID().uuidString)")
	try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
	return IndexStore(url: dir.appending(path: "index.sqlite"))
}

extension IndexStore {
	/// Sets columns on a row directly
	func setRaw(_ assignments: String, id: Int64) throws {
		try withConnection { connection throws(IndexError) in
			try connection.run("UPDATE items SET \(assignments) WHERE id = ?", [id])
		}
	}

	func allChanges(since anchor: SyncAnchor, pageSize: Int = 1000) throws -> (ChangeSet, SyncAnchor) {
		var updated: [IndexItem] = []
		var deleted: [IndexItem] = []
		var current = anchor
		while true {
			let page = try changes(since: current, limit: pageSize)
			updated += page.updated
			deleted += page.deleted
			current = page.anchor
			if !page.hasMore {
				return (ChangeSet(updated: updated, deleted: deleted, anchor: current, hasMore: false), current)
			}
		}
	}
}

@Suite struct IndexStoreTests {
	@Test func emptyIndex() throws {
		let store = try makeStore()
		let anchor = try store.currentAnchor()
		#expect(anchor.sequence == 0)
		let changes = try store.changes(since: anchor, limit: 10)
		#expect(changes.updated.isEmpty && changes.deletedItemIDs.isEmpty && !changes.hasMore)
		#expect(try store.folders().isEmpty)
	}

	/// A new database gets the whole schema at once, with nothing left of what the extension's earlier path needed
	@Test func newDatabaseHasTheCurrentSchema() throws {
		let store = try makeStore()
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == IndexStore.schemaVersion)
			let tables = Set(try connection.query("SELECT name FROM sqlite_master WHERE type = 'table'") { $0.text(0) })
			let expected = ["items", "folders", "item_attributes", "meta", "remote_bindings", "remote_discards"]
			#expect(tables.isSuperset(of: expected))
			#expect(!tables.contains("work") && !tables.contains("remote_migrations"))
			#expect(try connection.query("PRAGMA table_info(remote_outbox)") { $0.text(1) }.contains("template_identifier"))
			#expect(try connection.query("PRAGMA table_info(remote_overlays)") { $0.text(1) }.contains("origin"))
			#expect(try !connection.query("PRAGMA table_info(remote_bindings)") { $0.text(1) }.contains("access_mode"))
		}
	}

	/// One connection answers for every path, in chunks, and normalizes them as the index stores them
	@Test func liveKindsAnswersForEveryPathAtOnce() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteDirectory("itm_docs", "docs"), remoteFile("itm_a", "docs/a.txt", parent: "itm_docs"),
		])

		#expect(try store.liveKinds(folderID: "docs", at: []).isEmpty)
		#expect(
			try store.liveKinds(folderID: "docs", at: ["docs", "docs/a.txt"]) == [
				"docs": .directory, "docs/a.txt": .file,
			])
		#expect(try store.liveKinds(folderID: "docs", at: ["docs/._a.txt", "other"]).isEmpty)
		#expect(try store.liveKinds(folderID: "other", at: ["docs"]).isEmpty, "another folder's path")
		// More paths than one statement takes
		let many = (0..<1_200).map { "docs/f\($0)" } + ["docs/a.txt"]
		#expect(try store.liveKinds(folderID: "docs", at: many) == ["docs/a.txt": .file])
	}

	/// The index names that queries pick with `INDEXED BY`
	@Test func indexesNamedByQueriesExist() throws {
		let store = try makeStore()
		try store.withConnection { connection throws(IndexError) in
			let indexes = Set(try connection.query("SELECT name FROM sqlite_master WHERE type = 'index'") { $0.text(0) })
			#expect(indexes.isSuperset(of: ["items_live_path", "items_child_name", "remote_items_waiting"]))
		}
	}

	/// A database of another version is not touched, least of all emptied
	@Test func databasesOfAnotherVersionAreRefused() throws {
		let store = try makeStore()
		let connection = try SQLiteConnection(url: store.url)
		try connection.setUserVersion(IndexStore.schemaVersion + 1)
		#expect(throws: IndexError.unsupportedSchema(IndexStore.schemaVersion + 1)) { try store.folders() }
	}

	@Test func folderRootsWithTheSameLabel() throws {
		let store = try makeStore()
		let folders = [
			FolderDescription(folderID: "f2", label: "Photos"),
			FolderDescription(folderID: "f1", label: "Photos"),
			FolderDescription(folderID: "f3", label: "photos"),
		]
		let ids = try store.write { writer throws(IndexError) in
			try folders.map { f throws(IndexError) in try writer.upsertFolder(f) }
		}
		func names() throws -> [String?] { try ids.map { try store.item(id: $0)?.name } }
		#expect(try names() == ["Photos (f2)", "Photos", "photos (f3)"])

		var renamed = folders[1]
		renamed.label = "Camera"
		try store.write { writer throws(IndexError) in try writer.upsertFolder(renamed) }
		#expect(try names() == ["Photos", "Camera", "photos (f3)"])

		try store.write { writer throws(IndexError) in try writer.removeFolder(folderID: "f2") }
		#expect(try store.item(id: ids[2])?.name == "photos")
		#expect(try store.folder(id: "f3")?.label == "photos")
	}

	@Test func paging() throws {
		let store = try makeRemoteStore()
		let start = try store.currentAnchor()
		try store.snapshot((1...7).map { remoteFile("itm_\($0)", "f\($0)") })
		let (all, end) = try store.allChanges(since: start, pageSize: 3)
		#expect(all.updated.filter { $0.kind == .file }.count == 7)
		#expect(end == (try store.currentAnchor()))

		let roots = try store.children(of: nil, limit: 10)
		var page = try store.children(of: roots[0].id, limit: 3)
		var seen = page.map(\.path)
		while let last = page.last {
			page = try store.children(of: roots[0].id, after: last.id, limit: 3)
			seen += page.map(\.path)
		}
		#expect(seen.count == 7)
		#expect(try store.liveItems(limit: 100).count == 8)
	}

	@Test func anchorExpiry() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a")])
		let early = try store.currentAnchor()
		try store.snapshot([remoteFile("itm_b", "b")], cursor: "g2.1")
		let later = try store.currentAnchor()

		try store.pruneTombstones(before: Date().addingTimeInterval(1))
		#expect(throws: IndexError.anchorExpired) { try store.changes(since: early, limit: 10) }
		#expect(try store.changes(since: later, limit: 10).updated.isEmpty)
		#expect(try store.currentAnchor() == later, "pruning never moves the current anchor back")

		try store.reset()
		#expect(throws: IndexError.anchorExpired) { try store.changes(since: later, limit: 10) }
	}

	@Test func anchorEncoding() throws {
		let anchor = SyncAnchor(generation: UUID(), sequence: 0x0102_0304_0506_0708)
		#expect(SyncAnchor(data: anchor.data) == anchor)
		#expect(SyncAnchor(data: Data([1, 2, 3])) == nil)
		#expect(ItemIdentifiers.itemID(fromPage: ItemIdentifiers.page(after: 1234)) == 1234)
		#expect(ItemIdentifiers.itemID(from: ItemIdentifiers.string(for: 42)) == 42)
		#expect(ItemIdentifiers.itemID(from: "NSFileProviderRootContainerItemIdentifier") == nil)
	}

	@Test func folders() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteFile("itm_a", "a"), remoteDirectory("itm_d", "d"), remoteFile("itm_b", "d/b", parent: "itm_d"),
		])
		let root = try #require(try store.children(of: nil, limit: 10).first)
		let anchor = try store.currentAnchor()

		try store.write { writer throws(IndexError) in
			try writer.upsertFolder(FolderDescription(folderID: "docs", label: "Pictures"))
		}
		#expect(try store.item(id: root.id)?.name == "Pictures")
		#expect(try store.folder(id: "docs")?.label == "Pictures")
		#expect(try store.changes(since: anchor, limit: 10).updated.map(\.id) == [root.id])

		try store.write { writer throws(IndexError) in try writer.removeFolder(folderID: "docs") }
		#expect(try store.folders().isEmpty)
		#expect(try store.liveItems(limit: 10).isEmpty)
		#expect(try store.changes(since: anchor, limit: 10).deletedItemIDs.count == 4)
	}

	@Test func attributesAreKeptPerItem() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let unranked = Int64(bitPattern: UInt64.max)
		let tagged = ItemAttributes(
			tagData: Data([1, 2, 3]), favoriteRank: unranked, lastUsedDate: Date(timeIntervalSince1970: 100))
		try store.setAttributes(tagged, of: a.id)

		#expect(try store.attributes(of: [a.id, b.id]) == [a.id: tagged])
		#expect(UInt64(bitPattern: try #require(try store.attributes(of: [a.id])[a.id]?.favoriteRank)) == UInt64.max)

		// Clearing every attribute removes the row
		try store.setAttributes(ItemAttributes(), of: a.id)
		#expect(try store.attributes(of: [a.id]).isEmpty)

		// Attributes of items that are gone for good are removed with them
		try store.setAttributes(tagged, of: b.id)
		try store.snapshot([remoteFile("itm_a", "a.txt")], cursor: "g2.1")
		try store.pruneTombstones(before: Date().addingTimeInterval(60))
		#expect(try store.attributes(of: [b.id]).isEmpty)
	}
}
