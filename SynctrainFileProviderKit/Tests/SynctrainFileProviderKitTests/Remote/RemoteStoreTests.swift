// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteStoreTests {
	@Test func snapshotAndChangesProjectIntoItems() throws {
		let store = try makeRemoteStore()
		let start = try store.currentAnchor()
		let snapshot: SnapshotPage = try fixture("snapshot-page.json")
		try store.beginRemoteSnapshot(folderID: "docs")
		let first = try store.applyRemoteSnapshotPage(snapshot, folderID: "docs", requestedPage: nil)
		#expect(first.stored && first.changed == 2)
		#expect(try store.remoteSyncState(folderID: "docs").cursor == nil, "no cursor until the snapshot completes")

		// The last page, then the changes after the snapshot's start
		let last = SnapshotPage(snapshot: snapshot.snapshot, cursor: snapshot.cursor, items: [], next: nil)
		#expect(try store.applyRemoteSnapshotPage(last, folderID: "docs", requestedPage: snapshot.next).stored)
		#expect(try store.remoteSyncState(folderID: "docs").snapshot?.catchUpCursor == "g3.1200")
		let changes: ChangesPage = try fixture("changes-page.json")
		let applied = try store.applyRemoteChangesPage(changes, folderID: "docs", after: "g3.1200")
		#expect(applied.completedSnapshot)
		let state = try store.remoteSyncState(folderID: "docs")
		#expect(state.cursor == "g3.1207" && state.snapshot == nil)

		let folder = try #require(try store.folder(id: "docs"))
		let taxes = try #require(try store.item(folderID: "docs", path: "Taxes"))
		let report = try #require(try store.item(folderID: "docs", path: "Taxes/report.pdf"))
		#expect(taxes.parentID == folder.rootItemID && taxes.kind == .directory)
		#expect(report.parentID == taxes.id && report.size == 266_240 && report.pending == .none)
		#expect(try store.remoteItem(itemID: report.id)?.revision.rawValue.hasPrefix("r1.wxfG") == true)
		#expect(try store.remoteItem(itemID: folder.rootItemID)?.gatewayID == testRootGatewayID)
		#expect(try store.item(folderID: "docs", path: "Taxes/old.txt") == nil)

		let (all, _) = try store.allChanges(since: start)
		#expect(Set(all.updated.map(\.path)).isSuperset(of: ["Taxes", "Taxes/report.pdf"]))
	}

	@Test func renameAndMoveKeepIdentifiers() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteRoot, remoteDirectory("itm_dir", "a"), remoteFile("itm_x", "a/x.txt", parent: "itm_dir"),
			remoteDirectory("itm_b", "b"),
		])
		let x = try #require(try store.item(folderID: "docs", path: "a/x.txt"))
		let dir = try #require(try store.item(folderID: "docs", path: "a"))
		let anchor = try store.currentAnchor()

		// A rename keeps the row, and only the metadata revision changes
		try store.changes([remoteFile("itm_x", "a/y.txt", parent: "itm_dir")])
		let renamed = try #require(try store.item(id: x.id))
		#expect(renamed.path == "a/y.txt" && renamed.name == "y.txt")
		#expect(renamed.contentRevision == x.contentRevision && renamed.metadataRevision > x.metadataRevision)
		#expect(try store.changes(since: anchor, limit: 10).updated.map(\.id) == [x.id])

		// New contents change the content revision
		try store.changes([remoteFile("itm_x", "a/y.txt", parent: "itm_dir", size: 4, content: "abcd")])
		let edited = try #require(try store.item(id: x.id))
		#expect(edited.contentRevision == x.contentRevision + 1 && edited.size == 4)

		// Moving a directory moves what is below it
		try store.changes([remoteDirectory("itm_dir", "b/a", parent: "itm_b")])
		#expect(try store.item(id: dir.id)?.path == "b/a")
		#expect(try store.item(id: x.id)?.path == "b/a/y.txt")
		#expect(try store.item(id: x.id)?.parentID == dir.id)
	}

	@Test func tombstonesAndRevivalByGatewayIDOnly() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let anchor = try store.currentAnchor()
		try store.changes([remoteFile("itm_a", "a.txt", deleted: true)])
		#expect(try store.item(id: a.id) == nil)
		#expect(try store.changes(since: anchor, limit: 10).deletedItemIDs == [a.id])

		// Another item at the same path is a new item
		try store.changes([remoteFile("itm_b", "a.txt")])
		let b = try #require(try store.item(folderID: "docs", path: "a.txt"))
		#expect(b.id != a.id)

		// A symbolic link is hidden; when it becomes a file again, the same row comes back
		try store.changes([
			RemoteItem(
				id: "itm_b", folderID: testRemoteFolder, parentID: testRootGatewayID, name: "a.txt", path: "a.txt",
				kind: .symlink, modifiedNs: 1, revision: revision("link"))
		])
		#expect(try store.item(id: b.id) == nil)
		try store.changes([remoteFile("itm_b", "a.txt")])
		#expect(try store.item(id: b.id) != nil)
	}

	@Test func childrenBeforeParentsWaitForThem() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_x", "d/x.txt", parent: "itm_d"), remoteRoot, remoteDirectory("itm_d", "d")])
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		#expect(try store.item(folderID: "docs", path: "d/x.txt")?.parentID == d.id)
		#expect(try store.waitingRemoteItems(folderID: "docs").isEmpty)
	}

	@Test func caseCollisionsWaitWhileTheOtherItemExists() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "Readme"), remoteFile("itm_b", "README")])
		#expect(try store.item(folderID: "docs", path: "Readme") != nil)
		#expect(try store.waitingRemoteItems(folderID: "docs").map(\.gatewayID) == ["itm_b"])
		try store.changes([remoteFile("itm_a", "Readme", deleted: true)])
		#expect(try store.item(folderID: "docs", path: "README") != nil)
	}

	@Test func snapshotRemovesWhatItDidNotShow() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		try store.resetRemoteCursor(folderID: "docs")
		#expect(try store.remoteSyncState(folderID: "docs").needsSnapshot)
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")], cursor: "g2.1")
		#expect(try store.item(id: b.id) == nil)
		#expect(try store.item(folderID: "docs", path: "a.txt") != nil)
	}

	@Test func cursorAdvancesOnlyWithItsPage() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")], cursor: "g1.5")
		// A page for another position is not stored (another process moved the cursor)
		let stale = try store.applyRemoteChangesPage(
			ChangesPage(changes: [remoteFile("itm_n", "n.txt")], cursor: "g1.9", more: false), folderID: "docs",
			after: "g1.4")
		#expect(!stale.stored)
		#expect(try store.item(folderID: "docs", path: "n.txt") == nil)
		#expect(try store.remoteSyncState(folderID: "docs").cursor == "g1.5")

		// A snapshot page requested with another page token is not stored either
		try store.beginRemoteSnapshot(folderID: "docs")
		let page = SnapshotPage(snapshot: "snp_x", cursor: "g1.6", items: [remoteFile("itm_m", "m.txt")], next: "pg_2")
		#expect(!(try store.applyRemoteSnapshotPage(page, folderID: "docs", requestedPage: "pg_1").stored))
		#expect(try store.applyRemoteSnapshotPage(page, folderID: "docs", requestedPage: nil).stored)
		#expect(!(try store.applyRemoteSnapshotPage(page, folderID: "docs", requestedPage: nil).stored), "only once")
		#expect(try store.remoteSyncState(folderID: "docs").snapshot?.nextPage == "pg_2")
	}

	@Test func overlaysProtectLocalChanges() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 5_000, stagedFile: staged("e1"))
		#expect(edit.item?.pending == .modify && edit.item?.size == 6)
		#expect(edit.item.map { $0.contentRevision > a.contentRevision } == true)

		// A refresh updates the remote metadata, but the row keeps showing the local edit
		try store.changes([remoteFile("itm_a", "a.txt", rev: "other", size: 9, content: "other")])
		let shown = try #require(try store.item(id: a.id))
		#expect(shown.size == 6 && shown.modifiedNanoseconds == 5_000 && shown.pending == .modify)
		#expect(try store.remoteItem(itemID: a.id)?.size == 9)

		// A change-feed reset keeps the overlay, the operation and the staged file; so does a snapshot without it
		try store.resetRemoteCursor(folderID: "docs")
		try store.snapshot([remoteRoot], cursor: "g2.1")
		#expect(try store.item(id: a.id)?.pending == .modify)
		#expect(try store.mutation(operationID: try #require(edit.mutation?.operationID))?.state == .staged)
		#expect(try store.remoteOverlay(itemID: a.id)?.stagedPath == "Staged/e1")
		#expect(try store.releasableStagedFiles().isEmpty)
	}

	@Test func resetKeepsRemoteFoldersAndIsRefusedWhilePending() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("r"))
		#expect(throws: IndexError.self) { try store.reset() }
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		try store.reset()
		#expect(try store.item(id: a.id)?.path == "a.txt", "the remote folder keeps its rows and identifiers")
		#expect(try store.remoteItem(itemID: a.id)?.gatewayID == "itm_a")
	}

	/// An item fetched outside the change feed (here: the current item of a `412`) may predate a change the feed
	/// stored since; it is stored only over the revision the request was based on
	@Test func outOfBandItemsDoNotOverwriteNewerChanges() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt", rev: "1")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let captured = remoteFile("itm_a", "a.txt", rev: "2", size: 4, content: "two!")
		try store.changes([remoteFile("itm_a", "a.txt", rev: "3", size: 5, content: "three")])
		let stale = try store.applyRemoteItems([captured], serviceID: testServiceID, knownRevisions: ["itm_a": revision("1")])
		#expect(stale.changed == 0)
		#expect(try store.remoteItem(itemID: a.id)?.revision == revision("3"))
		#expect(try store.item(id: a.id)?.size == 5)
		// Without a known revision only new items are stored
		_ = try store.applyRemoteItems(
			[captured, remoteFile("itm_n", "n.txt", rev: "n")], serviceID: testServiceID)
		#expect(try store.remoteItem(itemID: a.id)?.revision == revision("3"))
		#expect(try store.item(folderID: "docs", path: "n.txt") != nil)
		// Over the revision the request named, it is stored
		let current = try store.applyRemoteItems(
			[remoteFile("itm_a", "a.txt", rev: "4", size: 4, content: "four")], serviceID: testServiceID,
			knownRevisions: ["itm_a": revision("3")])
		#expect(try current.changed == 1 && store.remoteItem(itemID: a.id)?.revision == revision("4"))
	}

	/// Another process's snapshot in progress is kept, not replaced
	@Test func beginningASnapshotKeepsTheOneInProgress() throws {
		let store = try makeRemoteStore()
		let first = try #require(try store.beginRemoteSnapshot(folderID: "docs").snapshot)
		let page = SnapshotPage(snapshot: "snp_1", cursor: "g1.1", items: [remoteRoot], next: "pg_2")
		#expect(try store.applyRemoteSnapshotPage(page, folderID: "docs", requestedPage: nil).stored)
		let again = try #require(try store.beginRemoteSnapshot(folderID: "docs").snapshot)
		#expect(again.mark == first.mark && again.snapshotID == "snp_1" && again.nextPage == "pg_2")
		try store.abandonRemoteSnapshot(folderID: "docs")
		#expect(try store.beginRemoteSnapshot(folderID: "docs").snapshot?.mark == first.mark + 1)
	}

	/// The rows below a deleted directory keep their identifiers while the deletion is not confirmed, even when
	/// tombstones are pruned meanwhile
	@Test func pruningKeepsRowsWithOverlays() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteDirectory("itm_d", "d"), remoteFile("itm_x", "d/x.txt", parent: "itm_d")])
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let x = try #require(try store.item(folderID: "docs", path: "d/x.txt"))
		_ = try store.recordRemoteDeletion(itemID: d.id)
		try store.write { writer throws(IndexError) in
			try writer.pruneTombstones(before: Date().addingTimeInterval(3600))
		}
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		guard case .ready(let submitted, _, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		_ = try store.recordRejection(submitted, error: "revision_mismatch", currentItem: nil)
		#expect(try store.item(id: d.id)?.path == "d")
		#expect(try store.item(id: x.id)?.path == "d/x.txt", "the same row comes back")
	}

	/// The items below a directory that is not shown wait (without being tried again and again), and are shown with it
	@Test func itemsBelowAHiddenDirectoryAreShownWithIt() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteRoot, remoteDirectory("itm_h", ".stversions"),
			remoteFile("itm_x", ".stversions/x.txt", parent: "itm_h"),
			remoteDirectory("itm_s", ".stversions/sub", parent: "itm_h"),
			remoteFile("itm_y", ".stversions/sub/y.txt", parent: "itm_s"),
		])
		#expect(try store.item(folderID: "docs", path: ".stversions") == nil)
		#expect(Set(try store.waitingRemoteItems(folderID: "docs").map(\.gatewayID)) == ["itm_x", "itm_s", "itm_y"])
		try store.changes([remoteDirectory("itm_h", "versions")])
		let shown = try #require(try store.item(folderID: "docs", path: "versions"))
		#expect(try store.item(folderID: "docs", path: "versions/x.txt")?.parentID == shown.id)
		#expect(try store.item(folderID: "docs", path: "versions/sub/y.txt") != nil)
		#expect(try store.waitingRemoteItems(folderID: "docs").isEmpty)
	}

	@Test func localContentOfAPage() throws {
		let store = try makeRemoteStore()
		try store.snapshot([
			remoteRoot, remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt"), remoteDirectory("itm_d", "d"),
		])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let b = try #require(try store.item(folderID: "docs", path: "b.txt"))
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let root = try #require(try store.folder(id: "docs")).rootItemID

		// An unconfirmed edit of a, and a new file
		_ = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("e1"))
		let created = try #require(
			try store.recordRemoteCreation(
				folderID: "docs", parentItemID: root, name: "n.txt", kind: .file, modifiedNanoseconds: 1,
				stagedFile: staged("n")
			).item)
		// b's current version came from a committed edit
		let edit = try store.recordRemoteContentChange(itemID: b.id, modifiedNanoseconds: 3, stagedFile: staged("b1"))
		let committed = remoteFile("itm_b", "b.txt", rev: "after-b1", size: 6, content: "b1b1b1")
		var claim = try #require(
			try store.claimMutation(
				owner: "app", duration: 60, operationID: try #require(edit.mutation?.operationID)))
		claim = try #require(
			try store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_b", token: "stu_b", offset: 6, length: 6, expires: .distantFuture),
				tokenReference: nil))
		claim = try #require(try store.recordUploadOffset(claim, offset: 6))
		guard case .ready(let submitted, _, _) = try store.prepareSubmission(claim, authorityGeneration: 3) else {
			Issue.record("not submitted")
			return
		}
		_ = try store.recordOperation(
			submitted, operation(submitted.record, state: .committed, item: committed, content: committed.contentID))

		let content = try store.remoteLocalContent(of: [a.id, b.id, d.id, created.id, root, 9999])
		#expect(content[a.id]?.overlayStagedPath == "Staged/e1" && content[a.id]?.contentID == contentID(Data("abc".utf8)))
		#expect(content[b.id]?.overlayStagedPath == nil && content[b.id]?.committedStagedPath == "Staged/b1")
		#expect(content[b.id]?.contentID == committed.contentID)
		#expect(content[created.id]?.overlayStagedPath == "Staged/n" && content[created.id]?.contentID == nil)
		#expect(content[d.id]?.contentID == nil && content[d.id]?.overlayStagedPath == nil)
		#expect(content[9999] == nil)
		#expect(try store.remoteLocalContent(of: []).isEmpty)
	}
}
