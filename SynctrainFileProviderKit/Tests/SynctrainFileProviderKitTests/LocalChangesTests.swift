// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// A record of a change made on this device, as Syncthing indexes it after a scan
func ownFile(_ path: String, seq: Int64, hash: String = "own", size: Int64, modNs: Int64) -> SyncthingRecord {
	SyncthingRecord(
		path: path, seq: seq, type: "file", size: size, modNs: modNs, blocksHash: hash, modifiedBy: "SELF123")
}

extension IndexStore {
	/// Applies records and moves the folder's cursor to the highest sequence, as an incremental pass would
	func syncing(_ records: [SyncthingRecord]) throws {
		try write { writer throws(IndexError) in
			try writer.upsertFolder(testFolder)
			try writer.setLocalShortDeviceID("SELF123")
			try writer.apply(records, folderID: "f1")
			let highest = max(records.map(\.seq).max() ?? 0, try writer.cursor(folderID: "f1"))
			try writer.setCursor(highest, folderID: "f1")
		}
	}

	func root() throws -> IndexItem {
		try #require(try children(of: nil, limit: 1).first)
	}
}

@Suite struct LocalChangesTests {
	@Test func creationIsAdoptedWhenScanned() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let root = try store.root()

		let created = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordCreation(
				folderID: "f1", parentID: root.id, path: "new.txt", kind: .file, size: 5, modifiedNanoseconds: 50,
				isSelected: true)
		}
		#expect(created.pending == .create && created.size == 5)

		// A full pass from before the scan does not know the file, but must not remove it
		try store.write { writer throws(IndexError) in
			let token = try writer.beginSweep(folderID: "f1")
			try writer.apply([file("old.txt", seq: 1)], folderID: "f1", sweepToken: token)
			try writer.finishSweep(folderID: "f1", token: token)
		}
		#expect(try store.item(id: created.id)?.pending == .create)

		try store.syncing([ownFile("new.txt", seq: 2, size: 5, modNs: 50)])
		let adopted = try #require(try store.item(id: created.id))
		#expect(adopted.pending == .none && adopted.blocksHash == "own")
		#expect(adopted.contentRevision == created.contentRevision, "our own write is not a new version")
	}

	@Test func contentChangeBumpsVersionOnce() throws {
		let store = try makeStore()
		try store.syncing([file("a.txt", seq: 1, hash: "v1")])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))

		let modified = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordContentChange(item, size: 7, modifiedNanoseconds: 70)
		}
		#expect(modified.pending == .modify && modified.contentRevision == item.contentRevision + 1)

		// The old version again (a full pass) is ignored; the scanned new version is adopted
		try store.applying([file("a.txt", seq: 1, hash: "v1")])
		#expect(try store.item(id: item.id)?.size == 7)
		try store.syncing([ownFile("a.txt", seq: 2, size: 7, modNs: 70)])
		let adopted = try #require(try store.item(id: item.id))
		#expect(adopted.pending == .none && adopted.contentRevision == modified.contentRevision)
	}

	@Test func renameKeepsIdentifierAndIgnoresOldPath() throws {
		let store = try makeStore()
		try store.syncing([file("a.txt", seq: 1, size: 3, modNs: 30)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))
		let root = try store.root()

		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(item, to: root.id, path: "b.txt")
		}
		#expect(moved.id == item.id && moved.path == "b.txt" && moved.name == "b.txt" && moved.pending == .rename)

		// The old entry, delivered again before Syncthing scanned the rename, does not come back
		try store.applying([file("a.txt", seq: 1, size: 3, modNs: 30)])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)

		try store.syncing([deletion("a.txt", seq: 2), ownFile("b.txt", seq: 3, size: 3, modNs: 30)])
		let adopted = try #require(try store.item(id: item.id))
		#expect(adopted.pending == .none && adopted.path == "b.txt")
		#expect(adopted.contentRevision == item.contentRevision)

		// Something new at the old path after the rename is a new item
		try store.syncing([file("a.txt", seq: 4, hash: "other")])
		#expect(try store.item(folderID: "f1", path: "a.txt").map(\.id) != item.id)
	}

	@Test func directoryRenameMovesDescendants() throws {
		let store = try makeStore()
		try store.syncing([directory("café", seq: 1), file("café/x.txt", seq: 2), directory("café/sub", seq: 3)])
		let dir = try #require(try store.item(folderID: "f1", path: "café"))
		let child = try #require(try store.item(folderID: "f1", path: "café/x.txt"))
		let root = try store.root()

		try store.recordLocalChanges { recorder throws(IndexError) in
			_ = try recorder.recordMove(dir, to: root.id, path: "Archive")
		}
		let movedChild = try #require(try store.item(id: child.id))
		#expect(movedChild.path == "Archive/x.txt" && movedChild.parentID == dir.id)
		#expect(try store.item(folderID: "f1", path: "Archive/sub") != nil)

		// Old entries are ignored, and a full pass before the scan keeps the moved descendants
		try store.write { writer throws(IndexError) in
			let token = try writer.beginSweep(folderID: "f1")
			try writer.apply(
				[directory("café", seq: 1), file("café/x.txt", seq: 2), directory("café/sub", seq: 3)], folderID: "f1",
				sweepToken: token)
			try writer.finishSweep(folderID: "f1", token: token)
		}
		#expect(try store.item(folderID: "f1", path: "café") == nil)
		#expect(try store.item(folderID: "f1", path: "café/x.txt") == nil)
		#expect(try store.item(id: child.id)?.path == "Archive/x.txt")
	}

	@Test func deletionIsNotUndoneByOldRecords() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1), file("d/x.txt", seq: 2)])
		let dir = try #require(try store.item(folderID: "f1", path: "d"))
		let child = try #require(try store.item(folderID: "f1", path: "d/x.txt"))
		let anchor = try store.currentAnchor()

		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(dir) }
		#expect(try store.item(id: dir.id) == nil && store.item(id: child.id) == nil)
		#expect(Set(try store.changes(since: anchor, limit: 10).deletedItemIDs) == [dir.id, child.id])

		try store.applying([directory("d", seq: 1), file("d/x.txt", seq: 2)])
		#expect(try store.item(folderID: "f1", path: "d/x.txt") == nil, "old records do not bring it back")

		// A version created after the deletion does come back, as the same item
		try store.syncing([directory("d", seq: 3), file("d/x.txt", seq: 4, hash: "new")])
		#expect(try store.item(folderID: "f1", path: "d/x.txt")?.id == child.id)
	}

	@Test func collisionsAndDescendants() throws {
		let store = try makeStore()
		try store.syncing([directory("Docs", seq: 1), file("Docs/a.txt", seq: 2), file("Docs/b.txt", seq: 3)])
		let root = try store.root()
		let docs = try #require(try store.item(folderID: "f1", path: "Docs"))
		try store.recordLocalChanges { recorder throws(IndexError) in
			#expect(try recorder.child(of: root.id, named: "docs")?.path == "Docs")
			#expect(try recorder.child(of: root.id, named: "other") == nil)
			#expect(try recorder.descendants(of: docs).map(\.path) == ["Docs/a.txt", "Docs/b.txt"])
		}
	}

	@Test func queuedWork() throws {
		let store = try makeStore()
		try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.enqueue(.select, folderID: "f1", path: "a.txt")
			try recorder.enqueue(.finishRename, folderID: "f1", path: "old.txt", extra: "new.txt")
			try recorder.retargetQueuedWork(.finishRename, folderID: "f1", from: "new.txt", to: "newer.txt")
			try recorder.dequeue(.select, folderID: "f1", path: "a.txt")
		}
		let work = try store.pendingWork()
		#expect(work.map(\.kind) == [.finishRename] && work.first?.extra == "newer.txt")
	}

	@Test func touchingAllItemsReportsThemAgain() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1), file("d/x.txt", seq: 2), file("y.txt", seq: 3)])
		let anchor = try store.currentAnchor()
		try store.write { writer throws(IndexError) in try writer.touchAllItems() }
		let (changes, _) = try store.allChanges(since: anchor, pageSize: 2)
		#expect(changes.updated.count == 4, "the folder root and three items")
		#expect(Set(changes.updated.map(\.changedSequence)).count == 4)
		try store.syncing([file("z.txt", seq: 4)])
		let later = try #require(try store.item(folderID: "f1", path: "z.txt"))
		#expect(later.changedSequence > changes.updated.map(\.changedSequence).max()!)
	}
}
