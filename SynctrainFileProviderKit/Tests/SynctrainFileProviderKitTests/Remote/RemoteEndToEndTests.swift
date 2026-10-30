// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Runs the whole client against a live gateway. Set `SYNCTRAIN_GATEWAY_URL` (`https://host:port`) and
/// `SYNCTRAIN_GATEWAY_CREDENTIAL` (an `stc_…` credential of a paired client); optionally `SYNCTRAIN_GATEWAY_SPKI` (the
/// pin, for a self-signed certificate) and `SYNCTRAIN_GATEWAY_FOLDER` (a writable folder; the first one otherwise).
/// The test creates, replaces, renames and deletes one file named `synctrain-e2e-<uuid>.bin` in the folder's root.
@Suite(
	.enabled(
		if: ProcessInfo.processInfo.environment["SYNCTRAIN_GATEWAY_URL"] != nil
			&& ProcessInfo.processInfo.environment["SYNCTRAIN_GATEWAY_CREDENTIAL"] != nil,
		"needs SYNCTRAIN_GATEWAY_URL and SYNCTRAIN_GATEWAY_CREDENTIAL"),
	.serialized, .timeLimit(.minutes(5)))
struct RemoteEndToEndTests {
	let environment = ProcessInfo.processInfo.environment

	func makeClient() async throws -> RemoteFileClient {
		let address = try #require(environment["SYNCTRAIN_GATEWAY_URL"])
		let url = try #require(URL(string: address))
		let host = try #require(url.host(percentEncoded: false))
		let credential = try #require(environment["SYNCTRAIN_GATEWAY_CREDENTIAL"])
		let pin = environment["SYNCTRAIN_GATEWAY_SPKI"]
		// The service ID is only known from the capabilities
		let probe = RemoteFileClient(
			endpoint: RemoteEndpoint(
				serviceID: "unknown", host: host, port: url.port ?? 443, pinnedSPKIHash: pin, displayName: host),
			credentials: InMemoryCredentialStore(credentials: ["unknown": credential]))
		let capabilities = try await probe.capabilities()
		#expect(capabilities.apiVersion == 1 && capabilities.serviceID.hasPrefix("svc_"))
		return RemoteFileClient(
			endpoint: RemoteEndpoint(
				serviceID: capabilities.serviceID, host: host, port: url.port ?? 443, pinnedSPKIHash: pin,
				displayName: capabilities.serviceName),
			credentials: InMemoryCredentialStore(credentials: [capabilities.serviceID: credential]))
	}

	/// Runs the outbox until nothing is unresolved
	func settle(_ outbox: Outbox, store: IndexStore) async throws {
		let deadline = Date().addingTimeInterval(120)
		while try store.hasUnresolvedMutations() {
			try #require(Date() < deadline, "the operations did not settle")
			let outcomes = try await outbox.run()
			if outcomes.isEmpty {
				try await Task.sleep(for: .milliseconds(500))
			}
		}
	}

	func stage(_ bytes: Data, storage: RemoteStorage) throws -> StagedFile {
		let source = try makeTemporaryDirectory().appending(path: "edit")
		try bytes.write(to: source)
		return try RemoteUploader.stage(source, in: storage, modifiedNs: Int64(Date().timeIntervalSince1970 * 1e9))
	}

	@Test func browseDownloadAndEdit() async throws {
		let client = try await makeClient()
		let serviceID = client.endpoint.serviceID
		let store = try makeStore()
		let storage = RemoteStorage(baseURL: try makeTemporaryDirectory())
		try storage.createDirectories()
		try store.saveRemoteEndpoint(client.endpoint)
		let sync = RemoteSync(store: store, client: client, scheduler: TransferScheduler(store: store, owner: "e2e"))
		try await sync.refreshCapabilities()

		let folders = try await sync.refreshFolders()
		let folder = try #require(
			folders.first { $0.id == environment["SYNCTRAIN_GATEWAY_FOLDER"] } ?? folders.first { $0.permissions.write },
			"no writable folder")
		try store.bindRemoteFolder(
			RemoteFolderBinding(
				folderID: "e2e", serviceID: serviceID, remoteFolderID: folder.id,
				permissions: folder.permissions),
			folder: FolderDescription(
				folderID: "e2e", label: folder.label),
			rootGatewayID: folder.rootItemID)
		try await sync.refresh(folderID: "e2e")
		#expect(try store.remoteSyncState(folderID: "e2e").cursor != nil, "the snapshot completed")
		let root = try #require(try store.folder(id: "e2e")).rootItemID

		let outbox = Outbox(store: store, storage: storage, owner: "e2e") { $0 == serviceID ? client : nil }
		let name = "synctrain-e2e-\(UUID().uuidString.lowercased()).bin"
		var bytes = Data(count: 3 << 20)
		for index in stride(from: 0, to: bytes.count, by: 4096) {
			bytes[index] = UInt8(truncatingIfNeeded: index / 4096)
		}

		// Create
		let created = try store.recordRemoteCreation(
			folderID: "e2e", parentItemID: root, name: name, kind: .file, modifiedNanoseconds: 1_000_000_000,
			stagedFile: try stage(bytes, storage: storage))
		let itemID = try #require(created.item?.id)
		try await settle(outbox, store: store)
		#expect(try store.mutation(operationID: try #require(created.mutation?.operationID))?.state == .committed)
		let state = try #require(try store.remoteItem(itemID: itemID))
		#expect(state.size == Int64(bytes.count) && state.contentID?.size == Int64(bytes.count))

		// Download (into other storage, so that the staged copy is not used)
		let downloader = RemoteDownloader(
			store: store, client: client, storage: RemoteStorage(baseURL: try makeTemporaryDirectory()),
			scheduler: TransferScheduler(store: store, owner: "e2e"), credentials: InMemoryCredentialStore(),
			owner: "e2e")
		let downloaded = try await downloader.download(try #require(RemoteDownloader.Request(state)))
		#expect(try Data(contentsOf: downloaded) == bytes)

		// Replace
		bytes.append(Data("more".utf8))
		let replaced = try store.recordRemoteContentChange(
			itemID: itemID, modifiedNanoseconds: 2_000_000_000, stagedFile: try stage(bytes, storage: storage))
		try await settle(outbox, store: store)
		#expect(try store.mutation(operationID: try #require(replaced.mutation?.operationID))?.state == .committed)
		#expect(try store.remoteItem(itemID: itemID)?.size == Int64(bytes.count))
		#expect(try store.remoteItem(itemID: itemID)?.revision != state.revision)

		// Rename (the item keeps its identifier)
		_ = try store.recordRemoteMove(itemID: itemID, toParentItemID: root, name: "renamed-" + name)
		try await settle(outbox, store: store)
		#expect(try store.item(id: itemID)?.name == "renamed-" + name)
		#expect(try store.remoteItem(itemID: itemID)?.gatewayID == state.gatewayID)

		// Delete
		let deleted = try store.recordRemoteDeletion(itemID: itemID)
		try await settle(outbox, store: store)
		#expect(try store.mutation(operationID: try #require(deleted.mutation?.operationID))?.state == .committed)
		try await sync.refresh(folderID: "e2e")
		#expect(try store.item(id: itemID) == nil)
	}
}
