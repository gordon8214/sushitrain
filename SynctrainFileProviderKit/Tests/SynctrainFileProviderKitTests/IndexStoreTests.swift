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

func file(_ path: String, seq: Int64, hash: String = "h", size: Int64 = 1, modNs: Int64 = 1, selected: Bool = true)
	-> SyncthingRecord
{
	SyncthingRecord(
		path: path, seq: seq, type: "file", size: size, modNs: modNs, blocksHash: hash, modifiedBy: "PEER123",
		selected: selected)
}

func directory(_ path: String, seq: Int64) -> SyncthingRecord {
	SyncthingRecord(path: path, seq: seq, type: "directory", modNs: 1)
}

func deletion(_ path: String, seq: Int64, type: String = "file") -> SyncthingRecord {
	SyncthingRecord(path: path, seq: seq, type: type, deleted: true)
}

let testFolder = FolderDescription(folderID: "f1", label: "Photos", folderType: "sendrecieve", isSelective: true)

extension IndexStore {
	@discardableResult
	func applying(_ records: [SyncthingRecord], folderID: String = "f1") throws -> ApplyResult {
		try write { writer throws(IndexError) in
			try writer.upsertFolder(testFolder)
			return try writer.apply(records, folderID: folderID)
		}
	}

	/// Sets columns on a row directly, to simulate changes made by the extension
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

	@Test func applyBuildsTree() throws {
		let store = try makeStore()
		let start = try store.currentAnchor()
		try store.applying([directory("docs", seq: 1), file("docs/a.txt", seq: 2), file("b.txt", seq: 3)])

		let roots = try store.children(of: nil, limit: 10)
		#expect(roots.map(\.name) == ["Photos"])
		#expect(roots[0].kind == .folderRoot && !roots[0].isSelected)

		let rootChildren = try store.children(of: roots[0].id, limit: 10)
		#expect(Set(rootChildren.map(\.path)) == ["docs", "b.txt"])

		let docs = try #require(try store.item(folderID: "f1", path: "docs"))
		let a = try #require(try store.item(folderID: "f1", path: "docs/a.txt"))
		#expect(a.parentID == docs.id && a.name == "a.txt" && a.blocksHash == "h")
		#expect(try store.children(of: docs.id, limit: 10).map(\.id) == [a.id])

		let (changes, _) = try store.allChanges(since: start)
		#expect(changes.updated.count == 4)
		#expect(Set(changes.updated.map(\.changedSequence)).count == 4, "every change has its own sequence")
	}

	@Test func reapplyingIsNoOp() throws {
		let store = try makeStore()
		let records = [directory("docs", seq: 1), file("docs/a.txt", seq: 2)]
		try store.applying(records)
		let anchor = try store.currentAnchor()
		let result = try store.applying(records)
		#expect(result.changed == 0)
		#expect(try store.changes(since: anchor, limit: 10).updated.isEmpty)
	}

	@Test func contentAndMetadataRevisions() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 1)])
		let before = try #require(try store.item(folderID: "f1", path: "a.txt"))

		try store.applying([file("a.txt", seq: 2, modNs: 5)])
		let touched = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(touched.contentRevision == before.contentRevision)
		#expect(touched.metadataRevision > before.metadataRevision)

		try store.applying([file("a.txt", seq: 3, hash: "new", modNs: 5)])
		let modified = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(modified.contentRevision > touched.contentRevision)
		#expect(modified.id == before.id)

		try store.applying([file("a.txt", seq: 4, hash: "new", modNs: 5, selected: false)])
		let deselected = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(!deselected.isSelected && deselected.contentRevision == modified.contentRevision)
	}

	@Test func deletionAndRevival() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 1)])
		let original = try #require(try store.item(folderID: "f1", path: "a.txt"))
		let anchor = try store.currentAnchor()

		try store.applying([deletion("a.txt", seq: 2)])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)
		let (deleted, afterDeletion) = try store.allChanges(since: anchor)
		#expect(deleted.deletedItemIDs == [original.id] && deleted.updated.isEmpty)

		// A deletion of something we do not have is ignored
		#expect(try store.applying([deletion("never.txt", seq: 3)]).changed == 0)

		// The same path coming back with the same type reuses the identifier and forces a new download
		try store.applying([file("a.txt", seq: 4, hash: "again")])
		let revived = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(revived.id == original.id && revived.contentRevision > original.contentRevision)
		let (update, _) = try store.allChanges(since: afterDeletion)
		#expect(update.updated.map(\.id) == [original.id] && update.deletedItemIDs.isEmpty)

		// A type change yields a new identifier
		try store.applying([directory("a.txt", seq: 5)])
		let replaced = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(replaced.id != original.id && replaced.kind == .directory)
	}

	@Test func hiddenRecordsAndSymlinksAreRemoved() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 1), file("b.txt", seq: 2)])
		var hidden = file("a.txt", seq: 3)
		hidden.hidden = true
		try store.applying([hidden, SyncthingRecord(path: "link", seq: 4, type: "symlink")])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)
		#expect(try store.item(folderID: "f1", path: "link") == nil)
		#expect(try store.item(folderID: "f1", path: "b.txt") != nil)
	}

	@Test func impliedDirectories() throws {
		let store = try makeStore()
		try store.applying([file("a/b/c.txt", seq: 1)])
		let a = try #require(try store.item(folderID: "f1", path: "a"))
		let b = try #require(try store.item(folderID: "f1", path: "a/b"))
		let c = try #require(try store.item(folderID: "f1", path: "a/b/c.txt"))
		#expect(a.isImplied && b.isImplied && b.parentID == a.id && c.parentID == b.id)
		#expect(a.id < b.id && b.id < c.id, "parents are created first")

		// A real entry upgrades the implied directory in place
		try store.applying([directory("a", seq: 2)])
		let realA = try #require(try store.item(folderID: "f1", path: "a"))
		#expect(realA.id == a.id && !realA.isImplied)

		// Removing the last file removes the implied directory but keeps the real one
		try store.applying([deletion("a/b/c.txt", seq: 3)])
		#expect(try store.item(folderID: "f1", path: "a/b") == nil)
		#expect(try store.item(folderID: "f1", path: "a") != nil)
	}

	@Test func directoryDeletedBeforeItsChildren() throws {
		let store = try makeStore()
		try store.applying([directory("d", seq: 1), file("d/x", seq: 2)])
		// Records arrive in path order, so the directory's tombstone comes before its children's
		try store.applying([deletion("d", seq: 3, type: "directory"), deletion("d/x", seq: 4)])
		#expect(try store.item(folderID: "f1", path: "d") == nil)
		#expect(try store.item(folderID: "f1", path: "d/x") == nil)
	}

	@Test func nameCollisions() throws {
		let store = try makeStore()
		try store.applying([file("Report.txt", seq: 1)])
		let result = try store.applying([file("report.TXT", seq: 2), file("Cafe\u{301}.txt", seq: 3)])
		#expect(result.collisions == 1)
		#expect(try store.item(folderID: "f1", path: "report.TXT") == nil)

		// Paths are stored decomposed, as Syncthing stores them on Apple platforms: a composed record is the same entry
		let cafe = try #require(try store.item(folderID: "f1", path: "Cafe\u{301}.txt"))
		#expect(Array(cafe.path.utf8) == Array("Cafe\u{301}.txt".utf8))
		let composed = try store.applying([file("Caf\u{e9}.txt", seq: 4, hash: "new")])
		#expect(composed.collisions == 0 && composed.changed == 1)
		#expect(try store.item(folderID: "f1", path: "Caf\u{e9}.txt")?.id == cafe.id)
	}

	/// Implied directories obey the same rule as entries: `docs/b.txt` does not add a `docs` next to `Docs`
	@Test func impliedDirectoriesDoNotCollide() throws {
		let store = try makeStore()
		let result = try store.applying([
			directory("Docs", seq: 1), file("Docs/a.txt", seq: 2), file("docs/b.txt", seq: 3),
			file("docs/sub/c.txt", seq: 4), file("Cafe\u{301}/x.txt", seq: 5), file("Caf\u{e9}/y.txt", seq: 6),
		])
		#expect(result.collisions == 2)
		let root = try store.root()
		let names = try store.children(of: root.id, limit: 10).map(\.name)
		#expect(names.count == 2 && names.contains("Docs"))
		let cafe = try #require(try store.item(folderID: "f1", path: "Cafe\u{301}"))
		#expect(try store.children(of: cafe.id, limit: 10).count == 2)
	}

	@Test func recordsWithNULAreSkipped() throws {
		let store = try makeStore()
		try store.applying([file("a.jpg", seq: 1)])
		let result = try store.applying([file("a.jpg\0x", seq: 2, hash: "other"), file("..", seq: 3), file("b//c", seq: 4)])
		#expect(result.changed == 0 && result.collisions == 0)
		#expect(try store.item(folderID: "f1", path: "a.jpg")?.blocksHash == "h")
		#expect(try store.liveItems(limit: 10).count == 2)
	}

	/// A deletion delivered as a partial record (in a full pass) removes a pending item instead of asking for more
	@Test func partialDeletionOfPendingItem() throws {
		let store = try makeStore()
		try store.applying([file("doc.txt", seq: 1)])
		let item = try #require(try store.item(folderID: "f1", path: "doc.txt"))
		try store.setRaw(
			"pending = \(PendingChange.modify.rawValue), pending_size = 1, pending_mod_ns = 1, pending_after = 1", id: item.id)
		var gone = deletion("doc.txt", seq: 2)
		gone.full = false
		let result = try store.applying([gone])
		#expect(result.needsFullRecords.isEmpty)
		#expect(try store.item(id: item.id) == nil)
	}

	/// A file changed through the File Provider where Syncthing has a directory skips the records below it, instead of
	/// failing the whole batch
	@Test func pendingFileInPlaceOfDirectory() throws {
		let store = try makeStore()
		try store.syncing([file("other.txt", seq: 1)])
		let root = try store.root()
		try store.recordLocalChanges { recorder throws(IndexError) in
			_ = try recorder.recordCreation(
				folderID: "f1", parentID: root.id, path: "a", kind: .file, size: 1, modifiedNanoseconds: 1,
				isSelected: true)
		}
		let result = try store.applying([file("a/b.txt", seq: 2), file("z.txt", seq: 3)])
		#expect(result.collisions == 1)
		#expect(try store.item(folderID: "f1", path: "z.txt") != nil)
		#expect(try store.item(folderID: "f1", path: "a")?.kind == .file)
	}

	/// What items allow depends on the type and mode of their folder
	@Test func folderModeChangeReportsAllItems() throws {
		let store = try makeStore()
		try store.applying([file("a", seq: 1), file("d/b", seq: 2)])
		var anchor = try store.currentAnchor()

		var changed = testFolder
		changed.label = "Pictures"
		try store.write { writer throws(IndexError) in try writer.upsertFolder(changed) }
		let root = try store.root()
		#expect(try store.changes(since: anchor, limit: 10).updated.map(\.id) == [root.id])

		anchor = try store.currentAnchor()
		changed.folderType = IndexFolder.typeReceiveOnly
		try store.write { writer throws(IndexError) in try writer.upsertFolder(changed) }
		let (types, _) = try store.allChanges(since: anchor)
		#expect(types.updated.count == 4 && Set(types.updated.map(\.changedSequence)).count == 4)

		anchor = try store.currentAnchor()
		changed.isSelective = false
		try store.write { writer throws(IndexError) in try writer.upsertFolder(changed) }
		let (modes, _) = try store.allChanges(since: anchor)
		#expect(modes.updated.count == 4)
	}

	@Test func folderRootsWithTheSameLabel() throws {
		let store = try makeStore()
		let folders = [
			FolderDescription(folderID: "f2", label: "Photos", folderType: "sendrecieve", isSelective: false),
			FolderDescription(folderID: "f1", label: "Photos", folderType: "sendrecieve", isSelective: false),
			FolderDescription(folderID: "f3", label: "photos", folderType: "sendrecieve", isSelective: false),
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

	@Test func adoptionClearsImplied() throws {
		let store = try makeStore()
		try store.syncing([file("a/b.txt", seq: 1)])
		let a = try #require(try store.item(folderID: "f1", path: "a"))
		#expect(a.isImplied)
		let root = try store.root()
		_ = try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordMove(a, to: root.id, path: "c") }
		try store.syncing([
			deletion("a/b.txt", seq: 2), directory("c", seq: 3), ownFile("c/b.txt", seq: 4, hash: "h", size: 1, modNs: 1),
		])
		let c = try #require(try store.item(id: a.id))
		#expect(c.pending == PendingChange.none && !c.isImplied && c.path == "c")
		#expect(try store.item(folderID: "f1", path: "c/b.txt")?.pending == PendingChange.none)
	}

	@Test func revivedItemForgetsPendingState() throws {
		let store = try makeStore()
		try store.syncing([file("a.txt", seq: 1, size: 3, modNs: 30)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(item) }
		try store.syncing([file("a.txt", seq: 5, hash: "n", size: 9, modNs: 90)])
		let revived = try #require(try store.item(folderID: "f1", path: "a.txt"))
		#expect(revived.id == item.id && revived.pending == PendingChange.none)
		let columns = try store.withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT pending_size IS NULL AND pending_mod_ns IS NULL AND pending_path IS NULL AND pending_after = 0 FROM items WHERE id = ?",
				[revived.id]
			) { $0.bool(0) }
		}
		#expect(columns == true)
	}

	/// Unconfirmed deletions made through the File Provider are not pruned: they keep old records from bringing the
	/// items back
	@Test func pruningKeepsUnconfirmedDeletions() throws {
		let store = try makeStore()
		try store.syncing([file("a.txt", seq: 1), file("b.txt", seq: 2)])
		let a = try #require(try store.item(folderID: "f1", path: "a.txt"))
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(a) }
		try store.syncing([deletion("b.txt", seq: 3)])
		try store.write { writer throws(IndexError) in try writer.pruneTombstones(before: Date().addingTimeInterval(1)) }
		let remaining = try store.withConnection { connection throws(IndexError) in
			try connection.query("SELECT id FROM items WHERE deleted = 1") { $0.int64(0) }
		}
		#expect(remaining == [a.id])
		try store.applying([file("a.txt", seq: 1)])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)
	}

	@Test func partialRecords() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 7), file("b.txt", seq: 8)])
		var sameA = file("a.txt", seq: 7)
		sameA.full = false
		sameA.blocksHash = nil
		var changedB = file("b.txt", seq: 9)
		changedB.full = false
		var newC = file("c.txt", seq: 10)
		newC.full = false
		let result = try store.applying([sameA, changedB, newC])
		#expect(result.needsFullRecords == ["b.txt", "c.txt"])
		#expect(try store.item(folderID: "f1", path: "c.txt") == nil)
		#expect(try store.item(folderID: "f1", path: "a.txt")?.blocksHash == "h")
	}

	@Test func sweepRemovesUnseenItems() throws {
		let store = try makeStore()
		try store.applying([file("keep.txt", seq: 1), file("gone.txt", seq: 2), file("dir/child.txt", seq: 3)])
		let pendingCreate = try #require(try store.item(folderID: "f1", path: "gone.txt"))
		try store.applying([file("pending.txt", seq: 4)])
		let pending = try #require(try store.item(folderID: "f1", path: "pending.txt"))
		try store.setRaw("pending = \(PendingChange.create.rawValue)", id: pending.id)

		try store.write { writer throws(IndexError) in
			let token = try writer.beginSweep(folderID: "f1")
			var keep = file("keep.txt", seq: 1)
			keep.full = false
			var child = file("dir/child.txt", seq: 3)
			child.full = false
			try writer.apply([keep, child], folderID: "f1", sweepToken: token)
			try writer.finishSweep(folderID: "f1", token: token)
		}
		#expect(try store.item(folderID: "f1", path: "keep.txt") != nil)
		#expect(try store.item(folderID: "f1", path: "dir/child.txt") != nil)
		#expect(try store.item(folderID: "f1", path: "dir")?.isImplied == true)
		#expect(try store.item(id: pendingCreate.id) == nil)
		#expect(try store.item(id: pending.id) != nil, "pending changes survive sweeps")
		#expect(try store.children(of: nil, limit: 10).count == 1, "the folder root survives sweeps")
	}

	@Test func pendingRowsAndOwnWrites() throws {
		let store = try makeStore()
		try store.write { writer throws(IndexError) in try writer.setLocalShortDeviceID("SELF123") }
		try store.applying([file("doc.txt", seq: 1, hash: "v1", size: 1, modNs: 1)])
		let item = try #require(try store.item(folderID: "f1", path: "doc.txt"))
		try store.setRaw(
			"""
			pending = \(PendingChange.modify.rawValue), pending_size = 2, pending_mod_ns = 20, size = 2, mod_ns = 20,
				pending_after = 3
			""", id: item.id)

		// Records from before the change (e.g. in a full pass before our write was scanned) do not override it
		try store.applying([file("doc.txt", seq: 2, hash: "remote", size: 1, modNs: 1)])
		#expect(try store.item(id: item.id)?.pending == .modify)
		try store.applying([deletion("doc.txt", seq: 3)])
		#expect(try store.item(id: item.id)?.pending == .modify, "local modification beats remote deletion")

		// Our own write is adopted without counting as a new version
		var own = file("doc.txt", seq: 4, hash: "v2", size: 2, modNs: 20)
		own.modifiedBy = "SELF123"
		try store.applying([own])
		let adopted = try #require(try store.item(id: item.id))
		#expect(adopted.pending == .none && adopted.blocksHash == "v2")
		#expect(adopted.contentRevision == item.contentRevision)
	}

	@Test func pendingChangeYieldsToNewerRecord() throws {
		let store = try makeStore()
		try store.write { writer throws(IndexError) in try writer.setLocalShortDeviceID("SELF123") }
		try store.applying([file("doc.txt", seq: 1, hash: "v1")])
		let item = try #require(try store.item(folderID: "f1", path: "doc.txt"))
		try store.setRaw(
			"pending = \(PendingChange.modify.rawValue), pending_size = 2, pending_mod_ns = 20, pending_after = 1",
			id: item.id)

		// Syncthing indexed another version after our change (e.g. it resolved a conflict): that is what is there now
		try store.applying([file("doc.txt", seq: 2, hash: "remote", size: 3, modNs: 30)])
		let resolved = try #require(try store.item(id: item.id))
		#expect(resolved.pending == .none && resolved.blocksHash == "remote" && resolved.size == 3)
		#expect(resolved.contentRevision > item.contentRevision)
	}

	@Test func paging() throws {
		let store = try makeStore()
		let start = try store.currentAnchor()
		try store.applying((1...7).map { file("f\($0)", seq: Int64($0)) })
		let (all, end) = try store.allChanges(since: start, pageSize: 3)
		#expect(all.updated.count == 8)
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
		let store = try makeStore()
		try store.applying([file("a", seq: 1)])
		let early = try store.currentAnchor()
		try store.applying([deletion("a", seq: 2), file("b", seq: 3)])
		let later = try store.currentAnchor()

		try store.write { writer throws(IndexError) in try writer.pruneTombstones(before: Date().addingTimeInterval(1)) }
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
		let store = try makeStore()
		try store.applying([file("a", seq: 1), file("d/b", seq: 2)])
		let root = try #require(try store.children(of: nil, limit: 10).first)
		let anchor = try store.currentAnchor()

		var renamed = testFolder
		renamed.label = "Pictures"
		try store.write { writer throws(IndexError) in try writer.upsertFolder(renamed) }
		#expect(try store.item(id: root.id)?.name == "Pictures")
		#expect(try store.folder(id: "f1")?.label == "Pictures")
		#expect(try store.changes(since: anchor, limit: 10).updated.map(\.id) == [root.id])

		try store.write { writer throws(IndexError) in
			try writer.setCursor(42, folderID: "f1")
			#expect(try writer.cursor(folderID: "f1") == 42)
			try writer.removeFolder(folderID: "f1")
		}
		#expect(try store.folders().isEmpty)
		#expect(try store.liveItems(limit: 10).isEmpty)
		#expect(try store.changes(since: anchor, limit: 10).deletedItemIDs.count == 4)
	}

	@Test func workQueue() throws {
		let store = try makeStore()
		try store.enqueue(.rescan, folderID: "f1", path: "a")
		try store.enqueue(.rescan, folderID: "f1", path: "a")
		try store.enqueue(.fetch, folderID: "f1", path: "b", extra: "12")
		var work = try store.pendingWork()
		#expect(work.map(\.kind) == [.rescan, .fetch])
		#expect(work[1].extra == "12")

		try store.failWork(id: work[0].id, error: "busy")
		work = try store.pendingWork()
		#expect(work[0].attempts == 1 && work[0].lastError == "busy")

		try store.completeWork(id: work[0].id)
		#expect(try store.pendingWork().map(\.kind) == [.fetch])
	}

	@Test func attributesAreKeptPerItem() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 1), file("b.txt", seq: 2)])
		let a = try #require(try store.item(folderID: "f1", path: "a.txt"))
		let b = try #require(try store.item(folderID: "f1", path: "b.txt"))
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
		try store.applying([deletion("b.txt", seq: 3)])
		try store.write { writer throws(IndexError) in try writer.pruneTombstones(before: Date().addingTimeInterval(60)) }
		#expect(try store.attributes(of: [b.id]).isEmpty)
	}
}
