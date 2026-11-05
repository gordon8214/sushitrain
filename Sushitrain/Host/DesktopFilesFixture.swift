// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if DEBUG && os(macOS)
	import Foundation
	import SynctrainFileProviderKit
	import SynctrainHostKit

	enum DesktopFilesFixture {
		static func make() throws -> RemoteEnvironment {
			guard TestRun.isActive, !TestRun.allowsFileProvider, let root = EngineLocations.testRoot else {
				throw CocoaError(.fileReadNoPermission)
			}
			let container = SharedContainer(baseURL: root.appending(path: "DesktopBrowserFixture"))
			try container.createDirectories()
			let environment = RemoteEnvironment(container: container, credentials: InMemoryCredentialStore(credentials: [:]))
			let store = environment.store
			try store.saveRemoteEndpoint(
				RemoteEndpoint(serviceID: "settings-fixture", host: "127.0.0.1", port: 9, displayName: "Fixture Gateway"))
			let description = Data(
				#"{"id":"fixture-docs","label":"Documents","rootItemID":"itm_fixtureRoot","permissions":{"read":true,"write":true},"type":"sendreceive"}"#
					.utf8)
			let folder = try JSONDecoder().decode(RemoteFolder.self, from: description)
			let localID = try store.addRemoteFolder(folder, serviceID: "settings-fixture")
			if try store.children(of: store.folder(id: localID)?.rootItemID, limit: 1).isEmpty {
				let revision = RemoteRevision(
					"r1."
						+ Data(repeating: 7, count: 32).base64EncodedString().replacingOccurrences(of: "+", with: "-")
						.replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: ""))
				var entries = [
					RemoteItem(
						id: folder.rootItemID, folderID: folder.id, parentID: nil, name: "", path: "", kind: .directory, modifiedNs: 1,
						writable: true, revision: revision, subtreeGeneration: 1)
				]
				entries.append(
					RemoteItem(
						id: "itm_fixtureDirectory", folderID: folder.id, parentID: folder.rootItemID, name: "Projects", path: "Projects",
						kind: .directory, modifiedNs: 1, writable: true, revision: revision, subtreeGeneration: 1))
				for index in 0..<600 {
					let name = "Notes-\(index).txt"
					entries.append(
						RemoteItem(
							id: "itm_fixture\(index)", folderID: folder.id, parentID: folder.rootItemID, name: name, path: name, kind: .file,
							size: 3, modifiedNs: 1, writable: true, revision: revision,
							contentID: ContentIdentity(blocksHash: Data(repeating: 0, count: 32), size: 3)))
				}
				try store.beginRemoteSnapshot(folderID: localID)
				let itemJSON = try JSONSerialization.jsonObject(with: JSONEncoder().encode(entries))
				let snapshot = try JSONDecoder().decode(
					SnapshotPage.self,
					from: JSONSerialization.data(withJSONObject: ["snapshot": "snp_fixture", "cursor": "g1.1", "items": itemJSON]))
				_ = try store.applyRemoteSnapshotPage(snapshot, folderID: localID, requestedPage: nil)
				let changes = try JSONDecoder().decode(
					ChangesPage.self, from: Data(#"{"changes":[],"cursor":"g1.1","more":false}"#.utf8))
				_ = try store.applyRemoteChangesPage(changes, folderID: localID, after: "g1.1")
			}
			return environment
		}
	}
#endif
