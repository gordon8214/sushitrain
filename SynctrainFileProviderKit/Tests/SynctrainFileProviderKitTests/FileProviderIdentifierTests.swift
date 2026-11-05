// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct FileProviderIdentifierTests {
	private func bind(_ store: IndexStore) throws {
		try store.saveRemoteEndpoint(testEndpoint)
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "docs", serviceID: testServiceID, remoteFolderID: testRemoteFolder,
				permissions: RemotePermissions(read: true, write: true)),
			folder: FolderDescription(folderID: "docs", label: "Documents"), rootGatewayID: testRootGatewayID)
	}

	private func legacyIndex(in container: SharedContainer) throws -> IndexItem {
		try container.createDirectories()
		let legacy = IndexStore(url: container.fileProviderURL.appending(path: "index.sqlite"))
		try bind(legacy)
		try legacy.snapshot([remoteRoot, remoteDirectory("itm_inbox", "Inbox")])
		let inbox = try #require(try legacy.item(folderID: "docs", path: "Inbox"))
		let connection = try SQLiteConnection(url: legacy.url)
		try connection.setUserVersion(7)
		return inbox
	}

	@Test func existingReplicaKeepsItsInboxIdentifierInsteadOfResolvingToAFile() throws {
		let container = SharedContainer(baseURL: try makeTemporaryDirectory())
		defer { try? FileManager.default.removeItem(at: container.baseURL) }
		let oldInbox = try legacyIndex(in: container)
		let original = try Data(contentsOf: container.fileProviderURL.appending(path: "index.sqlite"))
		// Simulate the partially upgraded device: gateway.sqlite reused Inbox's old row number for a font.
		let current = IndexStore(url: container.indexURL)
		try bind(current)
		try current.snapshot([remoteRoot, remoteFile("itm_font", "CoverAlbumItalic.otf")])
		#expect(try current.item(id: oldInbox.id)?.kind == .file)
		try current.changes([remoteDirectory("itm_inbox", "Inbox")])
		let pending = try current.recordRemoteCreation(
			folderID: "docs", parentItemID: try #require(try current.folder(id: "docs")).rootItemID,
			name: "pending.txt", kind: .file, modifiedNanoseconds: 1, stagedFile: staged("pending"))
		let anchor = try current.currentAnchor()
		let store = IndexStore(container: container)
		let id = try #require(try store.fileProviderItemID(for: String(oldInbox.id)))
		let inbox = try #require(try store.item(id: id))
		#expect(inbox.path == "Inbox" && inbox.kind == .directory && id != oldInbox.id)
		#expect(inbox.fileProviderIdentifier == String(oldInbox.id))
		#expect(inbox.parentFileProviderIdentifier == "1")
		let font = try #require(try store.item(folderID: "docs", path: "CoverAlbumItalic.otf"))
		#expect(font.fileProviderIdentifier == "gateway:\(oldInbox.id)")
		#expect(try store.fileProviderItemID(for: try #require(font.fileProviderIdentifier)) == font.id)
		#expect(try store.fileProviderItemID(for: "999999") == nil)
		#expect(try store.currentAnchor().generation != anchor.generation)
		#expect(try store.mutation(operationID: try #require(pending.mutation).operationID) == pending.mutation)
		#expect(try Data(contentsOf: container.fileProviderURL.appending(path: "index.sqlite")) == original)
		#expect(try IndexStore(container: container).item(id: id)?.fileProviderIdentifier == String(oldInbox.id))
	}

	@Test func freshGatewayIndexClaimsOldIdentifiersAsItsSnapshotArrives() throws {
		let container = SharedContainer(baseURL: try makeTemporaryDirectory())
		defer { try? FileManager.default.removeItem(at: container.baseURL) }
		let oldInbox = try legacyIndex(in: container)
		let store = IndexStore(container: container)
		try bind(store)
		#expect(try store.item(folderID: "docs", path: "")?.fileProviderIdentifier == "1")
		#expect(try store.fileProviderItemID(for: String(oldInbox.id)) == nil)
		try store.snapshot([remoteRoot, remoteFile("itm_font", "a.otf"), remoteDirectory("itm_inbox", "Moved Inbox")])
		let id = try #require(try store.fileProviderItemID(for: String(oldInbox.id)))
		#expect(try store.item(id: id)?.path == "Moved Inbox")
		// Gateway identity follows a rename, independently of the legacy path.
		try store.changes([remoteDirectory("itm_inbox", "Renamed Inbox")])
		#expect(try store.item(id: id)?.fileProviderIdentifier == String(oldInbox.id))
		#expect(try store.item(id: id)?.path == "Renamed Inbox")
		try store.changes([remoteFile("itm_new", "Renamed Inbox/new.txt", parent: "itm_inbox")])
		let child = try #require(try store.item(folderID: "docs", path: "Renamed Inbox/new.txt"))
		#expect(child.parentFileProviderIdentifier == String(oldInbox.id))
		var removed = remoteDirectory("itm_inbox", "Renamed Inbox")
		removed.deleted = true
		try store.changes([removed])
		try store.changes([remoteDirectory("itm_replacement", "Renamed Inbox")])
		let replacement = try #require(try store.item(folderID: "docs", path: "Renamed Inbox"))
		#expect(replacement.id != id)
		#expect(replacement.fileProviderIdentifier == "gateway:\(replacement.id)")
		#expect(try store.fileProviderItemID(for: String(oldInbox.id)) == id)
		#expect(try store.item(id: id) == nil, "a retired identifier must never resolve to a replacement item")
	}

	@Test func gatewayOnlyInstallationsKeepTheirExistingIdentifiers() throws {
		let container = SharedContainer(baseURL: try makeTemporaryDirectory())
		defer { try? FileManager.default.removeItem(at: container.baseURL) }
		let store = IndexStore(container: container)
		try bind(store)
		try store.snapshot([remoteRoot, remoteDirectory("itm_inbox", "Inbox")])
		let inbox = try #require(try store.item(folderID: "docs", path: "Inbox"))
		#expect(inbox.fileProviderIdentifier == String(inbox.id))
		#expect(try store.fileProviderItemID(for: String(inbox.id)) == inbox.id)
		#expect(try store.fileProviderItemID(for: "gateway:\(inbox.id)") == nil)
	}

	@Test func nativeOnlyLegacyIndexCanMigrateWithoutGatewayTables() throws {
		let container = SharedContainer(baseURL: try makeTemporaryDirectory())
		defer { try? FileManager.default.removeItem(at: container.baseURL) }
		try container.createDirectories()
		do {
			let legacy = try SQLiteConnection(url: container.fileProviderURL.appending(path: "index.sqlite"))
			try legacy.execute(
				"""
				CREATE TABLE items (id INTEGER PRIMARY KEY, folder_id TEXT, path TEXT, kind INTEGER, deleted INTEGER);
				INSERT INTO items VALUES (1, 'docs', '', 3, 0), (22871, 'docs', 'Inbox', 1, 0);
				""")
		}
		let store = IndexStore(container: container)
		try bind(store)
		try store.snapshot([remoteRoot, remoteDirectory("itm_inbox", "Inbox")])
		let inbox = try #require(try store.item(folderID: "docs", path: "Inbox"))
		#expect(inbox.fileProviderIdentifier == "22871")
		#expect(try store.fileProviderItemID(for: "22871") == inbox.id)
	}
}
