// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Directories a gateway on a Mac marks as packages (`RemoteItem.isPackage`)
@Suite struct RemotePackageTests {
	@Test func decodesTheMark() throws {
		let json = """
			{"id": "itm_icon", "folderID": "Documents", "parentID": "itm_root", "name": "AppIcon.icon",
			"path": "AppIcon.icon", "kind": "directory", "modifiedNs": 1, "revision": "r1.x", "package": true}
			"""
		let item = try RemoteJSON.decoder().decode(RemoteItem.self, from: Data(json.utf8))
		#expect(item.isPackage == true)
		let encoded = try JSONSerialization.jsonObject(with: try RemoteJSON.encoder().encode(item)) as? [String: Any]
		#expect(encoded?["package"] as? Bool == true)
		#expect(try fixture("item-directory.json", as: RemoteItem.self).isPackage == nil)
	}

	/// The coding keys are listed by hand: every property has one, and every field survives encoding and decoding
	@Test func everyFieldIsCoded() throws {
		let item = RemoteItem(
			id: "itm_icon", folderID: "Documents", parentID: "itm_root", name: "AppIcon.icon", path: "AppIcon.icon",
			kind: .directory, size: 1, modifiedNs: 2, mode: 493, writable: true, revision: revision("icon"),
			contentID: contentID(Data("x".utf8)), subtreeGeneration: 3, availability: .peers, deleted: true,
			movedToFolderID: "Other", isPackage: true)
		// The JSON names are the property names, but for `isPackage`
		let properties = Set(Mirror(reflecting: item).children.compactMap(\.label))
		let names = Set(RemoteItem.CodingKeys.allCases.map(\.stringValue))
		#expect(properties.subtracting(["isPackage"]).union(["package"]) == names)
		let decoded = try RemoteJSON.decoder().decode(RemoteItem.self, from: try RemoteJSON.encoder().encode(item))
		#expect(decoded == item)
	}

	/// The mark reaches the items the File Provider serves, from snapshots and from changes
	@Test func markedDirectoriesArePackagesInTheIndex() throws {
		let store = try makeRemoteStore()
		var icon = remoteDirectory("itm_icon", "AppIcon.icon")
		icon.isPackage = true
		try store.snapshot([
			icon, remoteFile("itm_json", "AppIcon.icon/icon.json", parent: "itm_icon"),
			remoteDirectory("itm_photos", "Photos"),
		])
		let marked = try #require(try store.item(folderID: "docs", path: "AppIcon.icon"))
		#expect(marked.isPackage)
		#expect(try store.item(folderID: "docs", path: "Photos")?.isPackage == false)
		#expect(try store.item(folderID: "docs", path: "AppIcon.icon/icon.json")?.isPackage == false)
		let root = try #require(try store.children(of: nil, limit: 10).first)
		#expect(try store.children(of: root.id, limit: 10).first { $0.id == marked.id }?.isPackage == true)
		#expect(try store.children(of: root.id, limit: 10).first { $0.name == "Photos" }?.isPackage == false)

		let anchor = try store.currentAnchor()
		var later = remoteDirectory("itm_site", "Site.bbprojectd")
		later.isPackage = true
		try store.changes([later])
		#expect(try store.changes(since: anchor, limit: 10).updated.first { $0.name == "Site.bbprojectd" }?.isPackage == true)
	}

	/// An index from before the mark (version 3) gains the column with nothing marked, and keeps its items
	@Test func earlierIndexesAreUpgraded() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_icon", "AppIcon.icon")])
		let before = try #require(try store.item(folderID: "docs", path: "AppIcon.icon"))
		try store.withConnection { connection throws(IndexError) in
			try connection.execute("ALTER TABLE remote_items DROP COLUMN package")
			try connection.setUserVersion(3)
		}

		#expect(try store.item(folderID: "docs", path: "AppIcon.icon") == before)
		try store.withConnection { connection throws(IndexError) in
			#expect(try connection.userVersion == IndexStore.schemaVersion)
			#expect(try connection.query("PRAGMA table_info(remote_items)") { $0.text(1) }.contains("package"))
		}
		// The folder keeps its cursor: a new snapshot when the location is rebuilt brings the marks
		#expect(try !store.remoteSyncState(folderID: "docs").needsSnapshot)
		try store.resetRemoteCursor(folderID: "docs")
		var icon = remoteDirectory("itm_icon", "AppIcon.icon")
		icon.isPackage = true
		try store.snapshot([icon], cursor: "g1.11")
		#expect(try store.item(folderID: "docs", path: "AppIcon.icon")?.isPackage == true)
		#expect(try store.item(folderID: "docs", path: "AppIcon.icon")?.changedSequence == before.changedSequence)
	}

	/// Processes that open an earlier index at the same time upgrade it once, and all of them can read it. Threads of
	/// their own, because SQLite waits for the write lock by blocking, which tasks must not do.
	@Test func concurrentUpgrades() throws {
		let store = try makeRemoteStore()
		try store.snapshot([remoteDirectory("itm_icon", "AppIcon.icon")])
		try store.withConnection { connection throws(IndexError) in
			try connection.execute("ALTER TABLE remote_items DROP COLUMN package")
			try connection.setUserVersion(3)
		}
		let url = store.url
		let opened = Locked(0)
		DispatchQueue.concurrentPerform(iterations: 4) { _ in
			if (try? IndexStore(url: url).item(folderID: "docs", path: "AppIcon.icon")) != nil {
				opened.update { $0 += 1 }
			}
		}
		#expect(opened.value == 4)
	}
}
