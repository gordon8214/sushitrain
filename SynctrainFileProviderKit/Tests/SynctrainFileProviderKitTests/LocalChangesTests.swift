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

	/// The pending state of rows, including tombstones, at a path
	func pendingStates(_ path: String, folderID: String = "f1") throws -> [(deleted: Bool, pending: PendingChange)] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT deleted, pending FROM items WHERE folder_id = ? AND path = ? ORDER BY id",
				[folderID, FileNames.normalized(path)]
			) { ($0.bool(0), PendingChange(rawValue: Int($0.int64(1))) ?? .none) }
		}
	}

	func fullPass(_ records: [SyncthingRecord]) throws {
		try write { writer throws(IndexError) in
			let token = try writer.beginSweep(folderID: "f1")
			try writer.apply(records, folderID: "f1", sweepToken: token)
			try writer.finishSweep(folderID: "f1", token: token)
		}
	}

	func create(_ path: String, kind: ItemKind = .file, size: Int64 = 5, modNs: Int64 = 50) throws -> IndexItem {
		let parentPath = FileNames.parentPath(path)
		let parent = try #require(try item(folderID: "f1", path: parentPath))
		return try recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordCreation(
				folderID: "f1", parentID: parent.id, path: path, kind: kind, size: size, modifiedNanoseconds: modNs,
				isSelected: true)
		}
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

	@Test func moveToAnotherFolder() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1), file("d/x.txt", seq: 2, size: 3, modNs: 30)])
		let other = FolderDescription(folderID: "f2", label: "Docs", folderType: "sendrecieve", isSelective: false)
		let otherRootID = try store.write { writer throws(IndexError) in
			let id = try writer.upsertFolder(other)
			try writer.setCursor(10, folderID: "f2")
			return id
		}
		let dir = try #require(try store.item(folderID: "f1", path: "d"))
		let child = try #require(try store.item(folderID: "f1", path: "d/x.txt"))

		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(dir, toFolder: "f2", parentID: otherRootID, path: "Moved")
		}
		#expect(moved.id == dir.id && moved.folderID == "f2" && moved.path == "Moved" && moved.pending == .create)
		let movedChild = try #require(try store.item(id: child.id))
		#expect(movedChild.folderID == "f2" && movedChild.path == "Moved/x.txt" && movedChild.parentID == dir.id)

		// Records in the first folder from before the move do not bring the items back
		try store.applying([directory("d", seq: 1), file("d/x.txt", seq: 2, size: 3, modNs: 30)])
		#expect(try store.item(folderID: "f1", path: "d") == nil)
		#expect(try store.item(folderID: "f1", path: "d/x.txt") == nil)

		// Syncthing scanning the new folder confirms the move
		try store.write { writer throws(IndexError) in
			try writer.apply(
				[directory("Moved", seq: 11), ownFile("Moved/x.txt", seq: 12, size: 3, modNs: 30)], folderID: "f2")
		}
		#expect(try store.item(id: child.id)?.pending == PendingChange.none)
		#expect(try store.item(id: dir.id)?.pending == PendingChange.none)
	}

	// MARK: - Regressions

	/// The extension used to record composed names, which never matched Syncthing's decomposed entries
	@Test func composedNamesAreAdoptedFromSyncthing() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let created = try store.create("Caf\u{e9}.txt")
		#expect(Array(created.path.utf8) == Array("Cafe\u{301}.txt".utf8))

		let result = try store.write { writer throws(IndexError) in
			try writer.setLocalShortDeviceID("SELF123")
			return try writer.apply([ownFile("Cafe\u{301}.txt", seq: 2, size: 5, modNs: 50)], folderID: "f1")
		}
		#expect(result.collisions == 0)
		#expect(try store.item(id: created.id)?.pending == PendingChange.none)
		try store.syncing([deletion("Cafe\u{301}.txt", seq: 3)])
		#expect(try store.item(id: created.id) == nil)
	}

	@Test func noTwinDirectoriesForComposedNames() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let root = try store.root()
		let created = try store.create("Caf\u{e9}", kind: .directory)
		try store.syncing([
			SyncthingRecord(path: "Cafe\u{301}", seq: 2, type: "directory", modNs: 1, modifiedBy: "SELF123"),
			file("Cafe\u{301}/x.txt", seq: 3),
		])
		let children = try store.children(of: root.id, limit: 10)
		#expect(children.count == 2)
		#expect(try store.item(id: created.id)?.pending == PendingChange.none)
		#expect(try store.item(folderID: "f1", path: "Caf\u{e9}/x.txt")?.parentID == created.id)
	}

	/// Syncthing indexed a new file before the app applied that record, and the user deleted it meanwhile
	@Test func deletedCreationIsNotRevivedByItsScan() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let created = try store.create("new.txt")
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(created) }
		#expect(try store.pendingStates("new.txt").map(\.pending) == [.delete])

		try store.syncing([ownFile("new.txt", seq: 2, size: 5, modNs: 50)])
		#expect(try store.item(folderID: "f1", path: "new.txt") == nil)
		// Syncthing's record of the deletion confirms it
		try store.syncing([deletion("new.txt", seq: 3)])
		#expect(try store.pendingStates("new.txt").map(\.pending) == [PendingChange.none])
		// A version created after that is shown
		try store.syncing([file("new.txt", seq: 4, size: 6, modNs: 60)])
		#expect(try store.item(folderID: "f1", path: "new.txt")?.id == created.id)
	}

	/// A file deleted before Syncthing ever saw it leaves nothing to confirm: a full pass without it clears the marker
	@Test func fullPassClearsDeletionsSyncthingNeverSaw() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let temporary = try store.create("temp.txt")
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(temporary) }

		// A marker recorded while a pass is running is kept: the pass may predate Syncthing's record of the file
		let token = try store.write { writer throws(IndexError) in try writer.beginSweep(folderID: "f1") }
		let late = try store.create("late.txt")
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(late) }
		try store.write { writer throws(IndexError) in
			try writer.apply([file("old.txt", seq: 1)], folderID: "f1", sweepToken: token)
			try writer.finishSweep(folderID: "f1", token: token)
		}
		#expect(try store.pendingStates("temp.txt").map(\.pending) == [PendingChange.none])
		#expect(try store.pendingStates("late.txt").map(\.pending) == [.delete])
		try store.fullPass([file("old.txt", seq: 1)])
		#expect(try store.pendingStates("late.txt").map(\.pending) == [PendingChange.none])
	}

	/// The recorder works with the item as it is in the index now, not with the caller's earlier copy
	@Test func recorderUsesCurrentState() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let created = try store.create("new.txt")
		// The app adopts Syncthing's record of the creation after the extension read the item
		try store.syncing([ownFile("new.txt", seq: 2, size: 5, modNs: 50)])
		let modified = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordContentChange(created, size: 7, modifiedNanoseconds: 70)
		}
		#expect(modified.pending == .modify)

		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(created) }
		#expect(try store.item(id: created.id) == nil)
		#expect(throws: IndexError.notFound) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordContentChange(created, size: 8, modifiedNanoseconds: 80)
			}
		}
		// Deleting what is already gone changes nothing
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(created) }
	}

	/// The extension read the item before the app applied a newer record of it (in a pass that has not finished): those
	/// records predate the change even though the folder's cursor has not moved past them
	@Test func changesFollowRecordsAlreadyApplied() throws {
		let store = try makeStore()
		try store.applying([file("a.txt", seq: 5, hash: "v5", size: 5, modNs: 5)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(item) }
		try store.applying([file("a.txt", seq: 4, hash: "v4", size: 4, modNs: 4)])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)
	}

	@Test func recorderChecksParentsAndNames() throws {
		let store = try makeStore()
		try store.syncing([directory("Docs", seq: 1), file("Docs/a.txt", seq: 2), file("b.txt", seq: 3)])
		let root = try store.root()
		let docs = try #require(try store.item(folderID: "f1", path: "Docs"))
		let a = try #require(try store.item(folderID: "f1", path: "Docs/a.txt"))
		let b = try #require(try store.item(folderID: "f1", path: "b.txt"))

		#expect(throws: IndexError.nameCollision(existing: docs.id)) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordCreation(
					folderID: "f1", parentID: root.id, path: "docs", kind: .directory, size: 0, modifiedNanoseconds: 1,
					isSelected: true)
			}
		}
		#expect(throws: IndexError.nameCollision(existing: a.id)) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordMove(b, to: docs.id, path: "Docs/A.TXT")
			}
		}
		#expect(throws: IndexError.notAllowed("the parent is not a directory in folder f1")) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordCreation(
					folderID: "f1", parentID: b.id, path: "b.txt/x", kind: .file, size: 0, modifiedNanoseconds: 1,
					isSelected: true)
			}
		}
		// The path must be in the parent
		#expect(throws: IndexError.self) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordCreation(
					folderID: "f1", parentID: docs.id, path: "elsewhere/x", kind: .file, size: 0, modifiedNanoseconds: 1,
					isSelected: true)
			}
		}
		#expect(throws: IndexError.notAllowed("a directory cannot be moved into itself")) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				let sub = try recorder.recordCreation(
					folderID: "f1", parentID: docs.id, path: "Docs/sub", kind: .directory, size: 0, modifiedNanoseconds: 1,
					isSelected: true)
				return try recorder.recordMove(docs, to: sub.id, path: "Docs/sub/Docs")
			}
		}
		// Renaming to a name that differs only in case is fine
		let renamed = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(a, to: docs.id, path: "Docs/A.txt")
		}
		#expect(renamed.name == "A.txt" && renamed.pending == .rename)
	}

	/// A directory renamed and renamed back is what it was, and so is everything below it
	@Test func renameBackRestoresState() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1), file("d/x.txt", seq: 2), file("f.txt", seq: 3, size: 3, modNs: 30)])
		let dir = try #require(try store.item(folderID: "f1", path: "d"))
		let child = try #require(try store.item(folderID: "f1", path: "d/x.txt"))
		let f = try #require(try store.item(folderID: "f1", path: "f.txt"))
		let root = try store.root()

		try store.recordLocalChanges { recorder throws(IndexError) in
			let moved = try recorder.recordMove(dir, to: root.id, path: "e")
			#expect(try recorder.item(id: child.id)?.pending == PendingChange.none)
			let back = try recorder.recordMove(moved, to: root.id, path: "d")
			#expect(back.pending == PendingChange.none && back.path == "d")
			// A file whose contents changed while it had another name still has that change pending
			let renamed = try recorder.recordMove(f, to: root.id, path: "g.txt")
			let written = try recorder.recordContentChange(renamed, size: 4, modifiedNanoseconds: 40)
			#expect(try recorder.recordMove(written, to: root.id, path: "f.txt").pending == .modify)
		}
		#expect(try store.item(id: child.id)?.pending == PendingChange.none)
		#expect(try store.item(id: child.id)?.path == "d/x.txt")

		// Syncthing saw nothing change; a full pass keeps everything, and the directory can move to another folder
		var unchanged = [directory("d", seq: 1), file("d/x.txt", seq: 2)]
		for index in unchanged.indices {
			unchanged[index].full = false
		}
		try store.fullPass(unchanged + [file("f.txt", seq: 3, size: 3, modNs: 30)])
		#expect(try store.item(id: dir.id) != nil && store.item(id: child.id) != nil)
		let other = FolderDescription(folderID: "f2", label: "Docs", folderType: "sendrecieve", isSelective: false)
		let otherRoot = try store.write { writer throws(IndexError) in try writer.upsertFolder(other) }
		let current = try #require(try store.item(id: dir.id))
		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(current, toFolder: "f2", parentID: otherRoot, path: "d")
		}
		#expect(moved.folderID == "f2")
	}

	/// Below a renamed directory, entries keep their state; the directory's rename protects them
	@Test func entriesBelowRenamedDirectory() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1), file("d/x.txt", seq: 2, size: 2, modNs: 20)])
		let dir = try #require(try store.item(folderID: "f1", path: "d"))
		let child = try #require(try store.item(folderID: "f1", path: "d/x.txt"))
		let root = try store.root()
		try store.recordLocalChanges { recorder throws(IndexError) in
			_ = try recorder.recordMove(dir, to: root.id, path: "e")
		}

		// An old deletion of something that was at the new path once does not remove the moved entry
		try store.fullPass([directory("d", seq: 1), file("d/x.txt", seq: 2, size: 2, modNs: 20), deletion("e/x.txt", seq: 1)])
		#expect(try store.item(id: child.id)?.path == "e/x.txt")
		#expect(try store.item(folderID: "f1", path: "d") == nil)

		// Syncthing scans the rename
		try store.syncing([
			deletion("d", seq: 3, type: "directory"), deletion("d/x.txt", seq: 4),
			SyncthingRecord(path: "e", seq: 5, type: "directory", modNs: 1, modifiedBy: "SELF123"),
			ownFile("e/x.txt", seq: 6, hash: "h", size: 2, modNs: 20),
		])
		let adopted = try #require(try store.item(id: dir.id))
		#expect(adopted.pending == PendingChange.none)
		let movedChild = try #require(try store.item(id: child.id))
		#expect(movedChild.contentRevision == child.contentRevision && movedChild.pending == PendingChange.none)
		try store.fullPass([directory("e", seq: 5), ownFile("e/x.txt", seq: 6, hash: "h", size: 2, modNs: 20)])
		#expect(try store.item(id: child.id) != nil)
	}

	/// A file renamed and then deleted keeps its old path, which old records must not bring back
	@Test func renameThenDeleteKeepsOldPath() throws {
		let store = try makeStore()
		try store.syncing([file("a.txt", seq: 1, size: 3, modNs: 30)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))
		let root = try store.root()
		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(item, to: root.id, path: "b.txt")
		}
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(moved) }
		try store.fullPass([file("a.txt", seq: 1, size: 3, modNs: 30)])
		#expect(try store.item(folderID: "f1", path: "a.txt") == nil)

		// Syncthing indexes the removal of the old path (it never saw the new one); a full pass then clears the marker
		try store.syncing([deletion("a.txt", seq: 2)])
		#expect(try store.pendingStates("b.txt").map(\.pending) == [.delete])
		try store.fullPass([])
		#expect(try store.pendingStates("b.txt").map(\.pending) == [PendingChange.none])
	}

	/// A new item moved before Syncthing's record of it was applied leaves its old path behind as a deletion
	@Test func movedCreationLeavesNoGhost() throws {
		let store = try makeStore()
		try store.syncing([file("old.txt", seq: 1)])
		let root = try store.root()
		let created = try store.create("draft.txt")
		_ = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(created, to: root.id, path: "final.txt")
		}
		try store.syncing([ownFile("draft.txt", seq: 2, size: 5, modNs: 50)])
		#expect(try store.item(folderID: "f1", path: "draft.txt") == nil)
		#expect(try store.item(id: created.id)?.path == "final.txt")
	}

	@Test func setBasedDeletionAndCrossFolderMove() throws {
		let store = try makeStore()
		try store.syncing([directory("d", seq: 1)] + (0..<300).map { file("d/f\($0).txt", seq: Int64($0 + 2)) })
		let dir = try #require(try store.item(folderID: "f1", path: "d"))
		let fresh = try store.create("d/new.txt")
		let anchor = try store.currentAnchor()

		let other = FolderDescription(folderID: "f2", label: "Docs", folderType: "sendrecieve", isSelective: false)
		let otherRoot = try store.write { writer throws(IndexError) in try writer.upsertFolder(other) }
		let moved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordMove(dir, toFolder: "f2", parentID: otherRoot, path: "m")
		}
		#expect(moved.folderID == "f2" && moved.pending == .create)
		// Every moved item is reported (its capabilities follow the new folder), and the old folder keeps markers for
		// all of them, including the new one
		let (afterMove, _) = try store.allChanges(since: anchor)
		#expect(afterMove.updated.filter { $0.folderID == "f2" && $0.kind != .folderRoot }.count == 302)
		#expect(try store.pendingStates("d/new.txt").map(\.pending) == [.delete])
		#expect(try store.item(id: fresh.id)?.path == "m/new.txt")
		try store.applying([ownFile("d/new.txt", seq: 400, size: 5, modNs: 50)])
		#expect(try store.item(folderID: "f1", path: "d/new.txt") == nil)

		let beforeDeletion = try store.currentAnchor()
		let current = try #require(try store.item(id: dir.id))
		try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(current) }
		let (deleted, _) = try store.allChanges(since: beforeDeletion, pageSize: 50)
		#expect(deleted.deletedItemIDs.count == 302 && Set(deleted.deletedItemIDs).count == 302)
		#expect(try store.pendingStates("m/f7.txt", folderID: "f2").map(\.pending) == [.delete])
	}

	@Test func descendantQueries() throws {
		let store = try makeStore()
		try store.syncing([
			directory("d", seq: 1), file("d/a.txt", seq: 2, size: 2, modNs: 20),
			file("d/sub/b.txt", seq: 3, size: 3, modNs: 30, selected: false), directory("empty", seq: 4),
			file("dz.txt", seq: 5),
		])
		let root = try store.root()
		let d = try #require(try store.item(folderID: "f1", path: "d"))
		let sub = try #require(try store.item(folderID: "f1", path: "d/sub"))
		let empty = try #require(try store.item(folderID: "f1", path: "empty"))
		#expect(try store.hasDescendants(of: d) && store.hasDescendants(of: d, unselectedOnly: true))
		#expect(try !store.hasDescendants(of: empty) && store.hasDescendants(of: root))
		#expect(try store.hasDescendants(of: sub, unselectedOnly: true))
		try store.syncing([file("d/sub/b.txt", seq: 6, size: 3, modNs: 30)])
		#expect(try !store.hasDescendants(of: d, unselectedOnly: true))
		#expect(
			try store.descendantFiles(of: d) == [
				"d/a.txt": FileVersion(size: 2, modifiedNanoseconds: 20),
				"d/sub/b.txt": FileVersion(size: 3, modifiedNanoseconds: 30),
			])
		#expect(try store.descendantFiles(of: root).count == 3)
		#expect(try store.descendants(of: root).count == 6)
	}

	@Test func queuedRenamesTouchingPaths() throws {
		let store = try makeStore()
		try store.enqueue(.finishRename, folderID: "f1", path: "a/b", extra: "a/c")
		try store.enqueue(.finishRename, folderID: "f1", path: "Caf\u{e9}", extra: "x/y")
		try store.enqueue(.finishRename, folderID: "f2", path: "a/b", extra: "a/c")
		try store.enqueue(.select, folderID: "f1", path: "a/b")
		func touching(_ path: String) throws -> [String] {
			try store.queuedRenames(folderID: "f1", touching: path).map(\.path)
		}
		for path in ["a/b", "a", "a/b/x", "a/c", "a/c/d/e"] {
			#expect(try touching(path) == ["a/b"], "\(path)")
		}
		for path in ["a/bc", "a/c0", "z", "a/b0"] {
			#expect(try touching(path).isEmpty, "\(path)")
		}
		#expect(try touching("Cafe\u{301}/z").count == 1)
		#expect(try touching("x").count == 1)
		#expect(try touching("").count == 2)

		#expect(try store.pendingWork(kinds: [.select]).map(\.kind) == [.select])
		#expect(try store.pendingWork(kinds: [.select, .finishRename]).count == 4)
		#expect(try store.pendingWork(kinds: []).isEmpty)
		#expect(try store.pendingWork(limit: 2).count == 2)
		#expect(try store.queuedWork(.finishRename, folderID: "f1", extra: "a/c")?.path == "a/b")
	}

	/// Syncthing's record of an earlier save on this device does not undo a newer save that is still the file on disk
	@Test func earlierOwnSaveKeepsNewerChangePending() throws {
		let (store, fileURL) = try makeStoreWithFolder()
		try store.syncing([ownFile("a.txt", seq: 1, hash: "v1", size: 1, modNs: 10)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))

		// Saved twice through Files; the second save is on disk
		try store.recordLocalChanges { recorder throws(IndexError) in
			_ = try recorder.recordContentChange(item, size: 2, modifiedNanoseconds: 20)
		}
		try Data("third".utf8).write(to: fileURL)
		let onDisk = try #require(FileFingerprint(path: fileURL.path(percentEncoded: false)))
		let saved = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordContentChange(item, size: onDisk.size, modifiedNanoseconds: onDisk.modifiedNanoseconds)
		}

		try store.syncing([ownFile("a.txt", seq: 2, hash: "v2", size: 2, modNs: 20)])
		let pending = try #require(try store.item(id: item.id))
		#expect(pending.pending == .modify && pending.size == onDisk.size)
		#expect(pending.contentRevision == saved.contentRevision, "no new version to fetch")

		try store.syncing([ownFile("a.txt", seq: 3, hash: "v3", size: onDisk.size, modNs: onDisk.modifiedNanoseconds)])
		let adopted = try #require(try store.item(id: item.id))
		#expect(adopted.pending == .none && adopted.blocksHash == "v3")
		#expect(adopted.contentRevision == saved.contentRevision)
	}

	/// When the newer save is no longer the file on disk, Syncthing's records win as usual
	@Test func earlierOwnSaveWinsWhenFileWasReplaced() throws {
		let (store, fileURL) = try makeStoreWithFolder()
		try store.syncing([ownFile("a.txt", seq: 1, hash: "v1", size: 1, modNs: 10)])
		let item = try #require(try store.item(folderID: "f1", path: "a.txt"))
		try store.recordLocalChanges { recorder throws(IndexError) in
			_ = try recorder.recordContentChange(item, size: 3, modifiedNanoseconds: 30)
		}
		// Something else wrote the file since
		try Data("something else".utf8).write(to: fileURL)

		try store.syncing([ownFile("a.txt", seq: 2, hash: "v2", size: 2, modNs: 20)])
		let current = try #require(try store.item(id: item.id))
		#expect(current.pending == .none && current.blocksHash == "v2" && current.size == 2)
	}

	private func makeStoreWithFolder() throws -> (IndexStore, URL) {
		let base = FileManager.default.temporaryDirectory.appending(path: "kit-tests-\(UUID().uuidString)")
		let container = SharedContainer(baseURL: base)
		try container.createDirectories()
		let fileURL = container.fileURL(folderID: "f1", path: "a.txt")
		try FileManager.default.createDirectory(
			at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
		return (IndexStore(container: container), fileURL)
	}
}
