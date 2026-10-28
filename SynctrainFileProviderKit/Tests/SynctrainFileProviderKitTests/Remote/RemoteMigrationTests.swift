// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteMigrationTests {
	/// A store with the Syncthing folder "docs" and one file in it
	func makeSyncthingStore() throws -> IndexStore {
		let store = try makeStore()
		try store.write { writer throws(IndexError) in
			try writer.upsertFolder(
				FolderDescription(folderID: "docs", label: "Documents", folderType: "sendrecieve", isSelective: false))
			_ = try writer.apply([file("a.txt", seq: 1)], folderID: "docs")
		}
		return store
	}

	func toGateway() -> RemoteMigration.Journal {
		RemoteMigration.Journal(
			direction: .toGateway, serviceID: testServiceID, remoteFolderID: testRemoteFolder,
			rootGatewayID: testRootGatewayID, permissions: RemotePermissions(read: true, write: true),
			label: "Documents")
	}

	@Test func journalSurvivesAndAdvances() throws {
		let store = try makeSyncthingStore()
		#expect(try store.remoteMigration(folderID: "docs") == nil)
		#expect(try !store.isRemoteMigrationFenced(folderID: "docs"))

		let started = try store.beginRemoteMigration(folderID: "docs", journal: toGateway())
		#expect(started.state == .fenced && started.journal.remoteFolderID == testRemoteFolder)
		#expect(try store.isRemoteMigrationFenced(folderID: "docs"))
		#expect(try store.beginRemoteMigration(folderID: "docs", journal: toGateway()) == started, "resumes it")
		#expect(throws: IndexError.notAllowed("the folder is being switched the other way")) {
			try store.beginRemoteMigration(folderID: "docs", journal: .init(direction: .toSyncthing))
		}

		try store.updateRemoteMigration(folderID: "docs", state: .engineStopped, lastError: nil)
		let failed = try store.updateRemoteMigration(folderID: "docs", lastError: "the gateway is unreachable")
		#expect(failed.state == .engineStopped && failed.journal.lastError == "the gateway is unreachable")
		// Another process (or a later launch) reads the same journal
		let reread = try #require(try IndexStore(url: store.url).remoteMigrations().first)
		#expect(reread.folderID == "docs" && reread.state == .engineStopped)
		#expect(reread.journal.permissions == RemotePermissions(read: true, write: true))
		#expect(reread.journal.label == "Documents")

		try store.finishRemoteMigration(folderID: "docs")
		#expect(try store.remoteMigrations().isEmpty)
		#expect(try !store.isRemoteMigrationFenced(folderID: "docs"))
		#expect(throws: IndexError.notFound) {
			try store.updateRemoteMigration(folderID: "docs", state: .bound, lastError: nil)
		}
	}

	/// A journal that cannot be read is an error, not a missing switch: no second switch starts over it
	@Test func unreadableJournalIsAnError() throws {
		let store = try makeSyncthingStore()
		try store.withConnection { connection throws(IndexError) in
			try connection.run(
				"INSERT INTO remote_migrations (folder_id, state, journal, updated_at) VALUES ('docs', 'fenced', '{', 0)")
		}
		let unreadable = IndexError.notAllowed("the journal of the switch of folder docs cannot be read")
		#expect(try store.isRemoteMigrationFenced(folderID: "docs"))
		#expect(throws: unreadable) { try store.beginRemoteMigration(folderID: "docs", journal: toGateway()) }
		#expect(throws: unreadable) { try store.remoteMigration(folderID: "docs") }
		try store.finishRemoteMigration(folderID: "docs")
		#expect(try store.beginRemoteMigration(folderID: "docs", journal: toGateway()).state == .fenced)
	}

	/// The fence is checked in the transaction that records an edit, on the Syncthing path as well
	@Test func fencedFolderRefusesEdits() throws {
		let store = try makeSyncthingStore()
		let root = try #require(try store.folder(id: "docs")).rootItemID
		let file = try #require(try store.child(of: root, named: "a.txt"))
		_ = try store.beginRemoteMigration(folderID: "docs", journal: toGateway())
		#expect(throws: IndexError.self) {
			_ = try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordCreation(
					folderID: "docs", parentID: root, path: "new.txt", kind: .file, size: 1, modifiedNanoseconds: 1,
					isSelected: true)
			}
		}
		#expect(throws: IndexError.self) {
			_ = try store.recordLocalChanges { recorder throws(IndexError) in
				try recorder.recordContentChange(file, size: 2, modifiedNanoseconds: 2)
			}
		}
		#expect(throws: IndexError.self) {
			try store.recordLocalChanges { recorder throws(IndexError) in try recorder.recordDeletion(file) }
		}
		#expect(try store.item(id: file.id)?.pending == PendingChange.none, "nothing was recorded")

		// Bound to the gateway but still switching: edits through the gateway wait as well
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "docs", accessMode: .remoteAPI, serviceID: testServiceID, remoteFolderID: testRemoteFolder,
				permissions: RemotePermissions(read: true, write: true)), rootGatewayID: testRootGatewayID)
		let fenced = "the folder is being switched to another way of access; try again when that is done"
		#expect(throws: IndexError.notAllowed(fenced)) {
			_ = try store.recordRemoteDeletion(itemID: file.id)
		}
		// Once switched, the fence no longer applies (this item still needs the gateway's snapshot)
		try store.finishRemoteMigration(folderID: "docs")
		#expect(throws: IndexError.notAllowed("the item is not on the gateway")) {
			_ = try store.recordRemoteDeletion(itemID: file.id)
		}
	}

	@Test func refusedWhileLocalChangesWait() throws {
		let store = try makeSyncthingStore()
		let root = try #require(try store.folder(id: "docs")).rootItemID
		_ = try store.recordLocalChanges { recorder throws(IndexError) in
			try recorder.recordCreation(
				folderID: "docs", parentID: root, path: "new.txt", kind: .file, size: 1, modifiedNanoseconds: 1,
				isSelected: true)
		}
		#expect(try store.localChanges(folderID: "docs") == LocalChangeSummary(pendingItems: 1, queuedWork: 0))
		#expect(
			throws: IndexError.notAllowed("changes made on this device have not been picked up by Syncthing yet")
		) {
			try store.beginRemoteMigration(folderID: "docs", journal: toGateway())
		}
		#expect(try store.remoteMigration(folderID: "docs") == nil, "nothing is fenced")

		// Syncthing indexed the new file; a scan the extension queued still waits
		try store.write { writer throws(IndexError) in
			_ = try writer.apply([file("new.txt", seq: 2, modNs: 1)], folderID: "docs")
		}
		try store.enqueue(.rescan, folderID: "docs", path: "a.txt")
		#expect(try store.localChanges(folderID: "docs") == LocalChangeSummary(pendingItems: 0, queuedWork: 1))
		#expect(throws: IndexError.self) {
			try store.beginRemoteMigration(folderID: "docs", journal: toGateway())
		}
		// Work of other folders, requested downloads and evictions do not matter
		let work = try #require(try store.pendingWork().first)
		try store.completeWork(id: work.id)
		try store.enqueue(.rescan, folderID: "other", path: "x")
		try store.enqueue(.fetch, folderID: "docs", path: "a.txt", extra: "1")
		try store.enqueue(.evict, folderID: "docs", path: "a.txt")
		#expect(try store.localChanges(folderID: "docs").isEmpty)
		#expect(try store.beginRemoteMigration(folderID: "docs", journal: toGateway()).state == .fenced)
	}

	@Test func switchingBackWaitsForUnresolvedEdits() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		#expect(throws: IndexError.notAllowed("the folder is accessed through a gateway already")) {
			try store.beginRemoteMigration(folderID: "docs", journal: toGateway())
		}
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let edit = try store.recordRemoteContentChange(itemID: a.id, modifiedNanoseconds: 2, stagedFile: staged("a"))
		#expect(throws: IndexError.notAllowed("changes to this folder were not resolved with its gateway")) {
			try store.beginRemoteMigration(folderID: "docs", journal: .init(direction: .toSyncthing))
		}
		_ = try store.discardMutation(operationID: try #require(edit.mutation?.operationID))
		let back = try store.beginRemoteMigration(folderID: "docs", journal: .init(direction: .toSyncthing))
		#expect(back.state == .returning && back.journal.direction == .toSyncthing)
	}

	@Test func sessionIdentifiersDeriveFromTheApp() {
		#expect(RemoteSessionIdentifier.app(appBundleIdentifier: "example.Synctrain") == "example.Synctrain.remote.app")
		#expect(
			RemoteSessionIdentifier.fileProviderExtension(appBundleIdentifier: "example.Synctrain")
				== "example.Synctrain.remote.extension")
	}
}
