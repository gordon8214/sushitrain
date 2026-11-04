// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct SharedIndexInitializationTests {
	@Test func historySourcesInitializeSharedStorageBeforeFilesStartup() throws {
		let root = URL.temporaryDirectory.appending(path: "history-first-use-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: root) }
		let container = SharedContainer(baseURL: root)
		let store = IndexStore(container: container)
		#expect(!FileManager.default.fileExists(atPath: container.fileProviderURL.path))
		#expect(try store.historyEndpoints().isEmpty)

		let clientID = try store.remoteClientID()
		let endpoint = RemoteEndpoint(serviceID: "history-only", host: "history.test", port: 20920, displayName: "Archive")
		try store.saveRemoteEndpoint(endpoint, usage: .history)
		try store.saveHistoryFolders(serviceID: endpoint.serviceID, folderIDs: ["docs"])

		let reopened = IndexStore(container: container)
		#expect(try reopened.historyEndpoints() == [endpoint])
		#expect(try reopened.remoteClientID() == clientID)
		#expect(try reopened.historyFolderIDs() == ["docs"])
		#expect(try !reopened.hasFilesGateways())
		#expect(try reopened.remoteBindings().isEmpty)
		#expect(try reopened.folders().isEmpty)
	}

	@Test func pairingCanBeTheFirstAccessToSharedStorage() throws {
		let root = URL.temporaryDirectory.appending(path: "pairing-first-use-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: root) }
		let container = SharedContainer(baseURL: root)
		let clientID = try IndexStore(container: container).remoteClientID()
		#expect(!clientID.isEmpty)
		#expect(try IndexStore(container: container).remoteClientID() == clientID)
	}

	@Test func blockedSharedDirectoryReportsUnavailableAndCanBeRetried() throws {
		let root = URL.temporaryDirectory.appending(path: "blocked-history-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: root) }
		try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
		let container = SharedContainer(baseURL: root)
		let blocker = root.appending(path: "FileProvider", directoryHint: .notDirectory)
		let contents = Data("Keep this file".utf8)
		try contents.write(to: blocker)
		let store = IndexStore(container: container)
		do {
			_ = try store.historyEndpoints()
			Issue.record("A file blocking the shared directory must prevent opening the index")
		}
		catch IndexError.unavailable(let message) {
			#expect(!message.isEmpty)
		}
		#expect(try Data(contentsOf: blocker) == contents)
		try FileManager.default.removeItem(at: blocker)
		#expect(try store.historyEndpoints().isEmpty)
	}

	@Test func explicitDatabaseURLStillRequiresItsParentDirectory() throws {
		let root = URL.temporaryDirectory.appending(path: "explicit-index-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: root) }
		let store = IndexStore(url: root.appending(path: "index.sqlite"))
		do {
			_ = try store.historyEndpoints()
			Issue.record("An explicit database URL must not create an arbitrary parent directory")
		}
		catch IndexError.unavailable(let message) {
			#expect(!message.isEmpty)
		}
		#expect(!FileManager.default.fileExists(atPath: root.path))
	}
}
