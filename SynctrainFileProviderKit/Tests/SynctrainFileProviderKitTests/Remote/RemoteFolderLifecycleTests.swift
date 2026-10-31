// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct RemoteFolderLifecycleTests {
	func folder(_ id: String = testRemoteFolder, label: String = "Documents", write: Bool = true) -> RemoteFolder {
		RemoteFolder(
			id: id, label: label, rootItemID: testRootGatewayID, permissions: RemotePermissions(read: true, write: write),
			type: .sendReceive, selective: nil, state: nil)
	}

	@Test func addingAFolderBindsItAndAddsItsRoot() throws {
		let store = try makeStore()
		try store.saveRemoteEndpoint(testEndpoint)
		let folderID = try store.addRemoteFolder(folder(), serviceID: testServiceID)
		#expect(folderID == testRemoteFolder)

		let binding = try #require(try store.remoteBinding(folderID: folderID))
		#expect(binding.serviceID == testServiceID && binding.remoteFolderID == testRemoteFolder)
		#expect(binding.permissions == RemotePermissions(read: true, write: true))
		#expect(try store.remoteRootGatewayID(folderID: folderID) == testRootGatewayID)
		#expect(try store.folder(id: folderID)?.label == "Documents")
		#expect(try store.addRemoteFolder(folder(), serviceID: testServiceID) == folderID, "added once")
		#expect(try store.remoteBindings().count == 1)
	}

	@Test func aFolderOfTheSameNameGetsItsOwnID() throws {
		let store = try makeStore()
		try store.saveRemoteEndpoint(testEndpoint)
		var other = testEndpoint
		other.serviceID = "svc_someOtherGatewayXYZ"
		try store.saveRemoteEndpoint(other)

		let first = try store.addRemoteFolder(folder(), serviceID: testServiceID)
		let second = try store.addRemoteFolder(folder(), serviceID: other.serviceID)
		#expect(first == testRemoteFolder)
		#expect(second == "\(testRemoteFolder)~\(other.serviceID.suffix(8))")
		#expect(try store.remoteBindings().count == 2)
	}

	@Test func removingAFolderReportsItsItemsDeletedAndForgetsItsMetadata() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let before = try store.currentAnchor()
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))

		#expect(try store.removeRemoteFolder(folderID: "docs").isEmpty)

		#expect(try store.remoteBinding(folderID: "docs") == nil)
		#expect(try store.folder(id: "docs") == nil)
		#expect(try store.remoteItem(itemID: item.id) == nil)
		#expect(try store.remoteSyncState(folderID: "docs").cursor == nil)
		let changes = try store.changes(since: before, limit: 100)
		#expect(changes.deletedItemIDs.contains(item.id), "Files learns that the item is gone")

		// Nothing keeps the gateway paired to the folder anymore
		try store.removeRemoteEndpoint(serviceID: testServiceID)
		#expect(try store.remoteEndpoints().isEmpty)
	}

	@Test func removingAFolderIsRefusedWhileEditsAreUnresolved() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let staged = StagedFile(relativePath: "Staged/edit", size: 3, digest: "digest", modifiedNs: 1)
		_ = try store.recordRemoteContentChange(itemID: item.id, modifiedNanoseconds: 1, stagedFile: staged)

		#expect(throws: IndexError.notAllowed("changes to this folder were not resolved with its gateway")) {
			try store.removeRemoteFolder(folderID: "docs")
		}
		#expect(try store.remoteBinding(folderID: "docs") != nil)
		#expect(try store.item(id: item.id) != nil)
	}

	@Test func removingAFolderReturnsTheFilesItLeavesBehind() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let staged = StagedFile(relativePath: "Staged/edit", size: 3, digest: "digest", modifiedNs: 1)
		let change = try store.recordRemoteContentChange(itemID: item.id, modifiedNanoseconds: 1, stagedFile: staged)
		let operationID = try #require(change.mutation?.operationID)
		_ = try store.discardMutation(operationID: operationID)

		#expect(try store.removeRemoteFolder(folderID: "docs") == ["Staged/edit"])
		#expect(try store.mutation(operationID: operationID) == nil)
	}

	@Test func aPartialDownloadOfARemovedFolderIsRemoved() throws {
		let root = FileManager.default.temporaryDirectory.appending(path: "synctrain-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: root) }
		let storage = RemoteStorage(container: SharedContainer(baseURL: root))
		try storage.createDirectories()
		let directory = storage.transferURL("transfer")
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		try Data("partial".utf8).write(to: directory.appending(path: "content.partial"))

		RemoteUploader.removeReleasedStagedFiles(
			["\(RemoteStorage.transfersDirectory)/transfer"], store: try makeStore(), storage: storage)
		#expect(!FileManager.default.fileExists(atPath: directory.path(percentEncoded: false)))
	}

	@Test func aChangeOfWritePermissionReportsTheFoldersItems() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt"), remoteFile("itm_b", "b.txt")])
		let before = try store.currentAnchor()

		try store.updateRemotePermissions(RemotePermissions(read: true, write: true), folderID: "docs")
		#expect(try store.changes(since: before, limit: 10).updated.isEmpty, "nothing changed")

		try store.updateRemotePermissions(RemotePermissions(read: true, write: false), folderID: "docs")
		#expect(try store.remoteBinding(folderID: "docs")?.permissions.write == false)
		let changed = try store.changes(since: before, limit: 10).updated.map(\.path)
		#expect(Set(changed).isSuperset(of: ["a.txt", "b.txt"]), "what can be done with them changed")
	}

	@Test func tombstonesArePrunedFromTheStore() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteFile("itm_a", "a.txt")])
		let item = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let before = try store.currentAnchor()
		try store.removeRemoteFolder(folderID: "docs")
		#expect(try store.changes(since: before, limit: 10).deletedItemIDs.contains(item.id))

		try store.pruneTombstones(before: Date().addingTimeInterval(60))
		#expect(throws: IndexError.anchorExpired) { try store.changes(since: before, limit: 10) }
	}
}
