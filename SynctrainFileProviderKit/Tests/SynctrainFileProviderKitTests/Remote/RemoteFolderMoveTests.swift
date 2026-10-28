// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Items that move to another folder of the same gateway: the source folder's feed has a tombstone that names the
/// destination (`movedToFolderID`), and the item shows up live, with the same ID, in the destination folder's feed
private let picturesFolder = "Pictures"
private let picturesRootID = "itm_9rLXc7mWSGuCF1bOQnpm3A"
private let picturesRoot = RemoteItem(
	id: picturesRootID, folderID: picturesFolder, parentID: nil, name: "", path: "", kind: .directory, size: 0,
	modifiedNs: 1, mode: 493, writable: true, revision: revision("pictures"), subtreeGeneration: 1)

@Suite struct RemoteFolderMoveTests {
	/// The store of `makeRemoteStore`, with the local folder "pics" bound to the gateway's "Pictures" as well (unless
	/// `bindPictures` is false). Both folders have a snapshot (with the items given for each).
	private func makeStore(
		documents: [RemoteItem], pictures: [RemoteItem] = [], bindPictures: Bool = true, readPictures: Bool = true
	) throws -> IndexStore {
		let store = try makeRemoteStore()
		if bindPictures {
			try store.bindRemoteFolder(
				RemoteFolderBinding(
					folderID: "pics", accessMode: .remoteAPI, serviceID: testServiceID,
					remoteFolderID: picturesFolder, permissions: RemotePermissions(read: readPictures, write: true)),
				folder: FolderDescription(folderID: "pics", label: "Pictures", folderType: "sendreceive", isSelective: false),
				rootGatewayID: picturesRootID)
			try store.snapshot([picturesRoot] + pictures, folderID: "pics")
		}
		try store.snapshot([remoteRoot] + documents)
		return store
	}

	/// The item as the destination's feed reports it after the move
	private func moved(_ item: RemoteItem, parent: String = picturesRootID, path: String? = nil) -> RemoteItem {
		var moved = item
		moved.folderID = picturesFolder
		moved.parentID = parent
		moved.path = path ?? item.path
		moved.revision = revision(item.id + "moved")
		return moved
	}

	/// The source's tombstone for the move
	private func tombstone(_ item: RemoteItem, movedTo destination: String = picturesFolder) -> RemoteItem {
		var tombstone = item
		tombstone.deleted = true
		tombstone.movedToFolderID = destination
		tombstone.revision = revision(item.id + "gone")
		return tombstone
	}

	@Test func decodesWhereATombstoneMovedTo() throws {
		let json = """
			{"changes": [
				{"id": "itm_a", "folderID": "Documents", "parentID": "\(testRootGatewayID)", "name": "a.txt",
					"path": "a.txt", "kind": "file", "modifiedNs": 1, "revision": "r1.a", "deleted": true,
					"movedToFolderID": "Pictures"},
				{"id": "itm_b", "folderID": "Documents", "parentID": "\(testRootGatewayID)", "name": "b.txt",
					"path": "b.txt", "kind": "file", "modifiedNs": 1, "revision": "r1.b", "deleted": true}
			], "cursor": "g1.11", "more": false}
			"""
		let page = try RemoteJSON.decoder().decode(ChangesPage.self, from: Data(json.utf8))
		#expect(page.changes.map(\.movedToFolderID) == ["Pictures", nil])
		let encoded = String(decoding: try RemoteJSON.encoder().encode(page.changes[1]), as: UTF8.self)
		#expect(!encoded.contains("movedToFolderID"), "absent elsewhere")
	}

	/// The source's feed first: its tombstone leaves the row, which the destination's feed then moves
	@Test func tombstoneFirstKeepsTheRowForTheDestination() throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a])
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let anchor = try store.currentAnchor()

		let result = try store.changes([tombstone(a)])
		#expect(result.changed == 0)
		#expect(try store.item(id: row.id)?.folderID == "docs", "still shown where it was")
		#expect(try store.remoteItem(itemID: row.id)?.isDeleted == false)

		try store.changes([moved(a)], folderID: "pics")
		let shown = try #require(try store.item(id: row.id))
		let rootRow = try #require(try store.folder(id: "pics")).rootItemID
		#expect(shown.folderID == "pics" && shown.path == "a.txt" && shown.parentID == rootRow)
		#expect(try store.remoteItem(itemID: row.id)?.folderID == "pics")
		let changes = try store.changes(since: anchor, limit: 100)
		#expect(!changes.deleted.contains { $0.id == row.id }, "the system never sees it deleted")
		#expect(changes.updated.contains { $0.id == row.id })
	}

	/// The destination's feed first: the row moves there, and the source's tombstone leaves it alone
	@Test func destinationFirstThenTombstoneKeepsTheRow() throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a])
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let anchor = try store.currentAnchor()

		try store.changes([moved(a)], folderID: "pics")
		#expect(try store.item(id: row.id)?.folderID == "pics")
		let result = try store.changes([tombstone(a)])
		#expect(result.changed == 0)
		#expect(try store.item(id: row.id)?.folderID == "pics")
		#expect(try store.remoteItem(itemID: row.id)?.isDeleted == false)
		#expect(!(try store.changes(since: anchor, limit: 100).deleted.contains { $0.id == row.id }))
	}

	/// Without the destination (not bound here, or not readable), the tombstone is a deletion
	@Test(arguments: [false, true]) func tombstoneWithoutTheDestinationIsADeletion(boundUnreadable: Bool) throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a], bindPictures: boundUnreadable, readPictures: false)
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		try store.changes([tombstone(a)])
		#expect(try store.item(id: row.id) == nil)
		#expect(try store.remoteItem(gatewayID: "itm_a")?.isDeleted == true)
	}

	/// An item stored in another folder already is not the source's to delete, wherever the tombstone says it went
	@Test func tombstoneOfAnItemInAnotherFolderIsIgnored() throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a])
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		try store.changes([moved(a)], folderID: "pics")
		try store.changes([tombstone(a, movedTo: "Elsewhere")])
		#expect(try store.item(id: row.id)?.folderID == "pics")
	}

	/// An ordinary tombstone (without a destination) is a deletion, also of an item the gateway moved meanwhile
	@Test func ordinaryTombstoneDeletes() throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a])
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		try store.changes([remoteFile("itm_a", "a.txt", rev: "gone", deleted: true)])
		#expect(try store.item(id: row.id) == nil)
	}

	/// A directory takes what is inside it along, also when the destination's feed reports only the directory: the
	/// source's tombstones and later snapshots no longer concern those items
	@Test(arguments: [false, true]) func movedDirectoryKeepsItsContents(tombstonesFirst: Bool) throws {
		let d = remoteDirectory("itm_d", "d")
		let x = remoteFile("itm_x", "d/x.txt", parent: "itm_d")
		let store = try makeStore(documents: [d, x, remoteFile("itm_b", "b.txt")])
		let dRow = try #require(try store.item(folderID: "docs", path: "d"))
		let xRow = try #require(try store.item(folderID: "docs", path: "d/x.txt"))
		let anchor = try store.currentAnchor()

		if tombstonesFirst {
			try store.changes([tombstone(d), tombstone(x)])
			#expect(try store.item(id: xRow.id)?.folderID == "docs")
		}
		try store.changes([moved(d, path: "d")], folderID: "pics")
		if !tombstonesFirst {
			try store.changes([tombstone(d), tombstone(x)])
		}
		#expect(try store.item(id: dRow.id)?.folderID == "pics")
		let shown = try #require(try store.item(id: xRow.id))
		#expect(shown.folderID == "pics" && shown.path == "d/x.txt" && shown.parentID == dRow.id)
		#expect(try store.remoteItem(itemID: xRow.id)?.folderID == "pics")

		// A fresh snapshot of the source does not have them; they stay in the destination
		try store.resetRemoteCursor(folderID: "docs")
		try store.snapshot([remoteRoot, remoteFile("itm_b", "b.txt")], cursor: "g1.20")
		#expect(try store.item(id: xRow.id)?.folderID == "pics")
		#expect(try store.item(id: dRow.id)?.folderID == "pics")
		let changes = try store.changes(since: anchor, limit: 100)
		#expect(!changes.deleted.contains { $0.id == dRow.id || $0.id == xRow.id })
	}

	/// The tombstone counts as seen by the source's snapshot in progress, whose completion does not remove the item
	/// before the destination's feed moves it (here the snapshot's pages no longer have it)
	@Test func snapshotOfTheSourceKeepsAMovedItem() throws {
		let a = remoteFile("itm_a", "a.txt")
		let store = try makeStore(documents: [a])
		let row = try #require(try store.item(folderID: "docs", path: "a.txt"))
		try store.resetRemoteCursor(folderID: "docs")
		try store.beginRemoteSnapshot(folderID: "docs")
		_ = try store.applyRemoteSnapshotPage(
			SnapshotPage(snapshot: "snp_docs", cursor: "g1.30", items: [remoteRoot], next: nil), folderID: "docs",
			requestedPage: nil)
		let completed = try store.applyRemoteChangesPage(
			ChangesPage(changes: [tombstone(a)], cursor: "g1.31", more: false), folderID: "docs", after: "g1.30")
		#expect(completed.completedSnapshot)
		#expect(try store.item(id: row.id)?.folderID == "docs")
		try store.changes([moved(a)], folderID: "pics")
		#expect(try store.item(id: row.id)?.folderID == "pics")
	}
}
