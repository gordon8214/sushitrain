// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteStoreTests {
	/// A database as version 4 left it, with items of a Syncthing folder
	@Test func migratesVersion4Database() throws {
		let store = try makeStore()
		let connection = try SQLiteConnection(url: store.url)
		try IndexStore.migrate(connection, to: 4)
		try connection.execute(
			"""
			INSERT INTO meta VALUES ('next_seq', '5') ON CONFLICT(key) DO UPDATE SET value = '5';
			INSERT INTO folders VALUES ('docs', 1, 'Documents', 'sendrecieve', 1, 7);
			INSERT INTO items (id, folder_id, path, parent_id, name, name_key, kind, changed_seq)
				VALUES (1, 'docs', '', NULL, 'Documents', 'documents', 3, 1),
					(2, 'docs', 'Taxes', 1, 'Taxes', 'taxes', 1, 2),
					(3, 'docs', 'Taxes/report.pdf', 2, 'report.pdf', 'report.pdf', 0, 3);
			INSERT INTO item_attributes VALUES (3, 'dGFn', 5, NULL);
			""")
		#expect(try connection.userVersion == 4)
		let generation = try IndexStore.generation(connection)

		let anchor = try store.currentAnchor()
		#expect(anchor.generation == generation && anchor.sequence == 4)
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == IndexStore.schemaVersion)
			let tables = Set(try connection.query("SELECT name FROM sqlite_master WHERE type = 'table'") { $0.text(0) })
			for table in [
				"remote_endpoints", "remote_bindings", "remote_items", "remote_overlays", "remote_cursors",
				"remote_snapshots", "remote_outbox", "remote_transfers", "remote_transfer_slots", "remote_staged_files",
				"remote_migrations",
			] {
				#expect(tables.contains(table), "\(table)")
			}
		}
		let clientID = try store.remoteClientID()
		#expect(UUID(uuidString: clientID) != nil && clientID == clientID.lowercased())
		#expect(try store.remoteClientID() == clientID)
		#expect(try store.item(id: 3)?.path == "Taxes/report.pdf")
		#expect(try store.attributes(of: [3])[3]?.favoriteRank == 5)

		// Binding the folder keeps the rows; the first snapshot adopts them by path
		try store.saveRemoteEndpoint(testEndpoint)
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "docs", accessMode: .remoteAPI, serviceID: testServiceID, remoteFolderID: testRemoteFolder,
				permissions: RemotePermissions(read: true, write: true)), rootGatewayID: testRootGatewayID)
		let page: SnapshotPage = try fixture("snapshot-page.json")
		try store.snapshot(page.items)
		#expect(try store.remoteItem(itemID: 3)?.gatewayID == "itm_hF6RgxMZ6JxNZWvbgMJ4rA")
		#expect(try store.remoteItem(itemID: 2)?.gatewayID == "itm_wG3IzBclHqTA5RCcjR2j2g")
		#expect(try store.item(id: 3)?.blocksHash == "c1.0PQuLA1Ythwbj9UtAT7Ozmyq4hWb4yL8x0Mr5i8QwRI.266240")
		#expect(try store.attributes(of: [3])[3]?.favoriteRank == 5, "tags and favorites stay with the item")
	}

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

	@Test func syncthingRecordsDoNotApplyToRemoteFolders() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		try store.write { writer throws(IndexError) in
			#expect(writer.isRemoteManaged(folderID: "docs"))
			let result = try writer.apply([file("b.txt", seq: 1)], folderID: "docs")
			#expect(result.changed == 0)
			try writer.removeFolder(folderID: "docs")
		}
		#expect(try store.item(folderID: "docs", path: "b.txt") == nil)
		#expect(try store.folder(id: "docs") != nil && store.item(folderID: "docs", path: "a.txt") != nil)
	}

	@Test func resetKeepsRemoteFoldersAndIsRefusedWhilePending() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		try store.applying([file("x.txt", seq: 1)])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("r"))
		#expect(throws: IndexError.self) { try store.reset() }
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		try store.reset()
		#expect(try store.item(id: a.id)?.path == "a.txt", "the remote folder keeps its rows and identifiers")
		#expect(try store.item(folderID: "f1", path: "x.txt") == nil)
		#expect(try store.remoteItem(itemID: a.id)?.gatewayID == "itm_a")
	}

	/// A database as version 5 left it, with an operation in the outbox
	@Test func migratesVersion5Outbox() throws {
		let store = try makeStore()
		let connection = try SQLiteConnection(url: store.url)
		try IndexStore.migrate(connection, to: 5)
		try connection.execute(
			"""
			INSERT INTO remote_outbox (operation_id, folder_id, kind, state, created_at, updated_at)
				VALUES ('op1', 'docs', 'replace', 'staged', 1, 1);
			""")
		#expect(try store.mutation(operationID: "op1")?.conflictPolicy == nil, "kept as a conflict copy")
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == IndexStore.schemaVersion)
			let columns = try connection.query("PRAGMA table_info(remote_outbox)") { $0.text(1) }
			#expect(columns.contains("on_conflict"))
		}
	}

	/// A database as version 6 left it (as a phone runs it), with pending creations: one the system made, a conflict
	/// copy the extension made, and the copy of a cross-host move; and an edit of an existing file
	@Test func migratesVersion6Overlays() throws {
		let store = try makeStore()
		let connection = try SQLiteConnection(url: store.url)
		try IndexStore.migrate(connection, to: 6)
		let copyName = "a.sync-conflict-20261027-101010-ABCDEFG.txt"
		try connection.execute(
			"""
			INSERT INTO meta VALUES ('next_seq', '10') ON CONFLICT(key) DO UPDATE SET value = '10';
			INSERT INTO folders VALUES ('docs', 1, 'Documents', 'sendreceive', 0, 0);
			INSERT INTO remote_endpoints (service_id, host, port, display_name, updated_at)
				VALUES ('\(testServiceID)', 'gateway.test', 20920, 'truenas', 1);
			INSERT INTO remote_bindings (folder_id, access_mode, service_id, remote_folder_id, root_gateway_id, can_read,
				can_write, updated_at)
				VALUES ('docs', 'remoteAPI', '\(testServiceID)', '\(testRemoteFolder)', '\(testRootGatewayID)', 1, 1, 1);
			INSERT INTO items (id, folder_id, path, parent_id, name, name_key, kind, size, changed_seq, pending)
				VALUES (1, 'docs', '', NULL, 'Documents', 'documents', 3, 0, 1, 0),
					(2, 'docs', 'new.txt', 1, 'new.txt', 'new.txt', 0, 6, 2, 1),
					(3, 'docs', '\(copyName)', 1, '\(copyName)', '\(FileNames.collisionKey(copyName))', 0, 6, 3, 1),
					(4, 'docs', 'moved.txt', 1, 'moved.txt', 'moved.txt', 0, 6, 4, 1),
					(5, 'docs', 'b.txt', 1, 'b.txt', 'b.txt', 0, 6, 5, 2);
			INSERT INTO remote_items (gateway_id, item_id, folder_id, parent_gateway_id, path, name, kind, revision,
				placed, updated_at)
				VALUES ('itm_b', 5, 'docs', '\(testRootGatewayID)', 'b.txt', 'b.txt', 'file', 'r1.b', 1, 1);
			INSERT INTO remote_outbox (operation_id, folder_id, service_id, item_id, kind, target_parent_id, target_name,
				item_kind, staged_path, upload_digest, upload_length, state, parent_operation_id, step, created_at,
				updated_at)
				VALUES ('op-new', 'docs', '\(testServiceID)', 2, 'create', 1, 'new.txt', 0, 'Staged/new', 'digest-new', 6,
						'staged', NULL, NULL, 1, 1),
					('op-copy', 'docs', '\(testServiceID)', 3, 'create', 1, '\(copyName)', 0, 'Staged/copy', 'digest-copy',
						6, 'staged', NULL, NULL, 2, 2),
					('op-move', 'other', 'svc_other', 4, 'crossHostMove', 1, 'moved.txt', 0, 'Staged/move', NULL, NULL,
						'staged', NULL, NULL, 3, 3),
					('op-move-1', 'docs', '\(testServiceID)', 4, 'create', 1, 'moved.txt', 0, 'Staged/move', 'digest-move',
						6, 'staged', 'op-move', 1, 3, 3),
					('op-edit', 'docs', '\(testServiceID)', 5, 'replace', NULL, NULL, 0, 'Staged/edit', 'digest-edit', 6,
						'failed', NULL, NULL, 4, 4);
			INSERT INTO remote_overlays (item_id, folder_id, kind, operation_id, staged_path, created_at)
				VALUES (2, 'docs', 1, 'op-new', 'Staged/new', 1), (3, 'docs', 1, 'op-copy', 'Staged/copy', 2),
					(4, 'docs', 1, 'op-move', 'Staged/move', 3), (5, 'docs', 2, 'op-edit', 'Staged/edit', 4);
			""")
		#expect(try connection.userVersion == 6)

		// Opening the store migrates it
		#expect(try store.remoteOverlay(itemID: 2)?.origin == .system)
		#expect(try store.remoteOverlay(itemID: 3)?.origin == .provider)
		#expect(try store.remoteOverlay(itemID: 4)?.origin == nil, "a row the system knows")
		#expect(try store.remoteOverlay(itemID: 5)?.origin == nil)
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == 7)
			let tables = Set(try connection.query("SELECT name FROM sqlite_master WHERE type = 'table'") { $0.text(0) })
			#expect(tables.contains("remote_discards"))
			#expect(try connection.query("PRAGMA table_info(remote_outbox)") { $0.text(1) }.contains("template_identifier"))
			#expect(try connection.query("PRAGMA table_info(remote_overlays)") { $0.text(1) }.contains("origin"))
		}
		let content = try store.remoteLocalContent(of: [2, 3, 4, 5])
		#expect(content[2]?.overlayOrigin == .system && content[3]?.overlayOrigin == .provider)
		#expect(content[4]?.overlayOrigin == nil && content[5]?.overlayOrigin == nil)
		#expect(try store.mutation(operationID: "op-new")?.templateIdentifier == nil)
		#expect(try store.mutation(operationID: "op-edit")?.state == .failed)

		// A creation recorded before the system's identifier was kept is recognized by its place and contents once
		// discarded; a discarded edit by its item
		_ = try store.discardMutation(operationID: "op-new")
		#expect(try store.item(id: 2) == nil)
		let discard = try #require(
			try store.consumeDiscardedCreation(
				templateIdentifier: "template-1", parentItemID: 1, name: "new.txt", kind: .file, digest: "digest-new"))
		#expect(discard.templateIdentifier == nil && discard.operationID == "op-new")
		_ = try store.discardMutation(operationID: "op-edit")
		#expect(try store.consumeDiscardedEdit(itemID: 5, digest: "digest-edit")?.operationID == "op-edit")
		#expect(try store.remoteDiscards().isEmpty)

		// Creations from now on keep the system's identifier
		let created = try store.recordRemoteCreation(
			folderID: "docs", parentItemID: 1, name: "next.txt", kind: .file, modifiedNanoseconds: 1,
			stagedFile: staged("next"), templateIdentifier: "template-2")
		#expect(try store.remoteCreation(templateIdentifier: "template-2")?.itemID == created.item?.id)
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

	/// A switch away from the gateway waits for rejected edits too (they are shown until exported or discarded), and
	/// leaves no overlay behind
	@Test func switchingBackWaitsForRejectedEdits() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 1, stagedFile: staged("e1"))
		let claim = try #require(try store.claimMutation(owner: "app", duration: 60))
		_ = try store.recordRejection(claim, error: "forbidden", currentItem: nil)
		#expect(try store.mutation(operationID: try #require(edit.mutation?.operationID))?.state == .failed)
		let syncthing = RemoteFolderBinding(
			folderID: "docs", accessMode: .syncthing, serviceID: nil, remoteFolderID: nil,
			permissions: RemotePermissions(read: true, write: true))
		#expect(throws: IndexError.notAllowed("changes to this folder were not resolved with its gateway")) {
			try store.bindRemoteFolder(syncthing)
		}
		_ = try store.discardMutation(operationID: claim.operationID)
		// An overlay that outlived its operation
		try store.withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				INSERT INTO remote_overlays (item_id, folder_id, kind, operation_id, staged_path, created_at)
				VALUES (?, 'docs', ?, 'gone', NULL, 0)
				""", [a.id, PendingChange.modify.rawValue])
		}
		try store.bindRemoteFolder(syncthing)
		#expect(try store.remoteOverlay(itemID: a.id) == nil)
		#expect(try store.remoteBinding(folderID: "docs")?.isRemote == false)
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

	@Test func resetIsRefusedWhileAFolderIsSwitched() throws {
		let store = try makeStore()
		try store.write { writer throws(IndexError) in
			try writer.upsertFolder(
				FolderDescription(folderID: "music", label: "Music", folderType: "sendrecieve", isSelective: false))
		}
		try store.beginRemoteMigration(
			folderID: "music",
			journal: RemoteMigration.Journal(direction: .toGateway, serviceID: testServiceID, remoteFolderID: "Music"))
		#expect(throws: IndexError.notAllowed("a folder is being switched between Syncthing and a gateway")) {
			try store.reset()
		}
		try store.finishRemoteMigration(folderID: "music")
		try store.reset()
	}

	/// Edits of a folder accessed through a gateway go to its outbox, never to the Syncthing path
	@Test func syncthingEditsAreRefusedForRemoteFolders() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		#expect(throws: IndexError.self) {
			try store.recordLocalChanges { recorder throws(IndexError) in
				_ = try recorder.recordCreation(
					folderID: "docs", parentID: root, path: "b.txt", kind: .file, size: 1, modifiedNanoseconds: 1,
					isSelected: true)
			}
		}
		#expect(throws: IndexError.self) {
			try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(a) }
		}
		#expect(try store.item(id: a.id) != nil && store.item(folderID: "docs", path: "b.txt") == nil)
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
