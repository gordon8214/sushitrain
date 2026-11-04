// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct HistoryTests {
	@Test func migrateExistingPairingsAndKeepHistorySeparateFromFiles() throws {
		let store = try makeRemoteStore()
		// Exercise migration from the previous schema with its existing pairing and folder binding.
		try store.withConnection { connection throws(IndexError) in
			try connection.execute("DROP TABLE history_folders; DROP TABLE gateway_usage;")
			try connection.setUserVersion(1)
		}
		#expect(try store.gatewayUsage(serviceID: testServiceID) == .files)
		#expect(try store.historyEndpoints().isEmpty)
		let endpoint = RemoteEndpoint(serviceID: "history-only", host: "history.test", port: 20920, displayName: "Archive")
		try store.saveRemoteEndpoint(endpoint, usage: .history)
		#expect(try store.gatewayUsage(serviceID: endpoint.serviceID) == .history)
		#expect(try store.remoteBindings().allSatisfy { $0.serviceID != endpoint.serviceID })
		try store.saveHistoryFolders(serviceID: endpoint.serviceID, folderIDs: ["docs"])
		#expect(try store.historyFolderIDs() == ["docs"])
		try store.setGatewayUsage([], serviceID: endpoint.serviceID)
		#expect(try store.historyFolderIDs().isEmpty)
		#expect(try store.remoteEndpoint(serviceID: endpoint.serviceID) != nil)
		try store.saveRemoteEndpoint(endpoint, usage: .files)
		#expect(try store.gatewayUsage(serviceID: endpoint.serviceID) == .files)
		try store.removeRemoteEndpoint(serviceID: endpoint.serviceID)
		#expect(try store.historyEndpoints().isEmpty)
	}

	@Test func legacyPairingKeepsFilesRoleWhenAddedForHistory() throws {
		let store = try makeRemoteStore()
		let endpoint = try #require(try store.remoteEndpoint(serviceID: testServiceID))
		try store.saveRemoteEndpoint(endpoint, usage: .history)
		#expect(try store.gatewayUsage(serviceID: testServiceID) == [.files, .history])
	}

	private func history(
		size: Int64 = 5, unsupported: Bool = false, disableWhileLoading: Bool = false, primary: Bool = false,
		apiVersion: Int = 1, wrongIdentity: Bool = false
	) throws
		-> RemoteHistory
	{
		let store = try makeStore()
		let client = mockClient { request in
			if request.path == "/v1/capabilities" {
				var caps: Capabilities = try! fixture("capabilities.json")
				caps.serviceID = wrongIdentity ? "another-gateway" : testServiceID
				caps.apiVersion = apiVersion
				caps.features = unsupported ? ["versions"] : ["versions", "versions.byPath"]
				return .json(caps)
			}
			if request.path == "/v1/folders/docs/versions" {
				guard request.query("path") == "gone/café ?#.txt" else { return .error(404, code: "not_found") }
				if disableWhileLoading { try! store.setGatewayUsage([], serviceID: testServiceID) }
				var version = RemoteVersionTests.older
				version.size = size
				version.entityTag = "\"archive\""
				return .json(
					RemotePathVersions(folderID: "docs", path: "gone/café ?#.txt", versioning: .simple, versions: [version]))
			}
			if request.path.hasSuffix("/content") {
				guard request.path.hasPrefix("/v1/folders/docs/versions/") else { return .error(404, code: "not_found") }
				guard request.header("If-Match") == "\"archive\"" else { return .error(412, code: "revision_mismatch") }
				return MockResponse(status: 200, headers: ["ETag": "\"archive\""], body: Data("older".utf8))
			}
			return .error(404, code: "not_found")
		}
		try store.saveRemoteEndpoint(client.endpoint, usage: .history)
		let source = HistorySource(
			endpoint: client.endpoint, folderID: "docs", path: "gone/café ?#.txt", itemID: primary ? "stale-item" : nil)
		return RemoteHistory(store: store, source: source, client: client)
	}

	@Test func browseAndDownloadByFolderAndUnicodePath() async throws {
		let history = try history()
		let list = try await history.versions()
		#expect(list.versions.count == 1)
		let destination = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
		defer { try? FileManager.default.removeItem(at: destination) }
		try await history.download(try #require(list.versions.first), to: destination)
		#expect(try String(contentsOf: destination, encoding: .utf8) == "older")
	}

	@Test func oldGatewayAndDisabledSourceAreExplicitErrors() async throws {
		let old = try history(unsupported: true)
		await #expect(throws: HistoryError.self) { try await old.versions() }
		let removed = try history(disableWhileLoading: true)
		await #expect(throws: HistoryError.self) { try await removed.versions() }
	}

	@Test func incompleteDownloadIsRemovedAndRevokedSourceCannotBeRead() async throws {
		let history = try history(size: 100)
		let list = try await history.versions()
		let destination = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
		await #expect(throws: RemoteError.self) {
			try await history.download(try #require(list.versions.first), to: destination)
		}
		#expect(!FileManager.default.fileExists(atPath: destination.path))
		try history.store.setGatewayUsage([], serviceID: history.source.id)
		await #expect(throws: HistoryError.self) { try await history.download(RemoteVersionTests.older, to: destination) }
	}

	@Test func restoreCannotReplaceNewerRevisionOrPendingEdit() throws {
		let (store, _, browser) = try RemoteVersionTests().browser()
		let file = try browser.file()
		let staged = StagedFile(relativePath: "Staged/history", size: 5, digest: "digest", modifiedNs: 1)
		#expect(throws: IndexError.self) {
			try store.recordRemoteContentChange(
				itemID: file.item.id, modifiedNanoseconds: 1, stagedFile: staged, onConflict: .reject,
				expectedRevision: RemoteRevision("outdated"))
		}
		_ = try store.recordRemoteContentChange(itemID: file.item.id, modifiedNanoseconds: 1, stagedFile: staged)
		#expect(throws: IndexError.self) {
			try store.recordRemoteContentChange(
				itemID: file.item.id, modifiedNanoseconds: 1, stagedFile: staged, onConflict: .reject,
				expectedRevision: file.remote.revision)
		}
	}
	@Test func canceledStagingCannotQueueRestore() async throws {
		let (store, _, browser) = try RemoteVersionTests().browser()
		let file = try browser.file()
		let storage = RemoteStorage(baseURL: FileManager.default.temporaryDirectory.appending(path: UUID().uuidString))
		try storage.createDirectories()
		let staged = StagedFile(relativePath: "Staged/canceled-history", size: 5, digest: "digest", modifiedNs: 1)
		try Data("older".utf8).write(to: storage.url(relativePath: staged.relativePath))
		let started = AsyncStream<Void>.makeStream()
		let release = AsyncStream<Void>.makeStream()
		let task = Task {
			try await HistoryRestore.record(
				itemID: file.item.id, binding: browser.binding, expectedRevision: file.remote.revision, modifiedNs: 1, store: store,
				storage: storage
			) {
				await Task.detached {
					started.continuation.yield(())
					for await _ in release.stream { break }
					return staged
				}.value
			}
		}
		for await _ in started.stream { break }
		task.cancel()
		release.continuation.yield(())
		await #expect(throws: CancellationError.self) { try await task.value }
		#expect(try store.item(id: file.item.id)?.pending == PendingChange.none)
		#expect(!FileManager.default.fileExists(atPath: storage.url(relativePath: staged.relativePath).path))
	}

	@Test func backgroundHistorySessionsAreDistinct() {
		let app = "test.synctrain"
		let ids = [
			RemoteSessionIdentifier.app(appBundleIdentifier: app),
			RemoteSessionIdentifier.fileProviderExtension(appBundleIdentifier: app),
			RemoteSessionIdentifier.historyApp(appBundleIdentifier: app),
			RemoteSessionIdentifier.historyActionExtension(appBundleIdentifier: app),
		]
		#expect(Set(ids).count == 4)
	}

	@Test func primaryGatewayUsesPathDownloadForRetainedArchive() async throws {
		let history = try history(primary: true)
		let listing = try await history.versions()
		let destination = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
		defer { try? FileManager.default.removeItem(at: destination) }
		try await history.download(try #require(listing.versions.first), to: destination)
		#expect(try String(contentsOf: destination, encoding: .utf8) == "older")
	}

	@Test func compatibilityErrorsIdentifyWhichSideNeedsAttention() async throws {
		let wrong = try history(wrongIdentity: true)
		await #expect(throws: HistoryError.identityMismatch) { try await wrong.versions() }
		let newer = try history(apiVersion: 2)
		await #expect(throws: HistoryError.clientNeedsUpdate) { try await newer.versions() }
		let older = try history(unsupported: true)
		await #expect(throws: HistoryError.needsUpdate) { try await older.versions() }
	}

	@Test func legacyPrimaryGatewayKeepsItemRoutes() async throws {
		let store = try makeStore()
		let client = mockClient { request in
			if request.path == "/v1/capabilities" {
				var caps: Capabilities = try! fixture("capabilities.json")
				caps.serviceID = testServiceID
				caps.features = ["versions"]
				return .json(caps)
			}
			if request.path == "/v1/items/legacy/versions" {
				return .json(
					RemoteVersionList(
						itemID: "legacy", revision: RemoteRevision("r1"), versioning: .simple, versions: [RemoteVersionTests.older]))
			}
			if request.path == "/v1/items/legacy/versions/\(RemoteVersionTests.older.id)/content" {
				return MockResponse(status: 200, headers: [:], body: Data("older".utf8))
			}
			return .error(404, code: "not_found")
		}
		try store.saveRemoteEndpoint(client.endpoint, usage: .files)
		let source = HistorySource(
			endpoint: client.endpoint, folderID: "docs", path: "file.txt", itemID: "legacy", requiresEnabled: false)
		let history = RemoteHistory(store: store, source: source, client: client)
		let listing = try await history.versions()
		let destination = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
		defer { try? FileManager.default.removeItem(at: destination) }
		try await history.download(try #require(listing.versions.first), to: destination)
		#expect(try String(contentsOf: destination, encoding: .utf8) == "older")
	}

	@Test(arguments: MutationState.allCases) func restorePollingStopsAtEveryTerminalState(_ state: MutationState) throws {
		switch state {
		case .committed: #expect(try HistoryRestore.isCommitted(state))
		case .failed, .conflict: #expect(throws: HistoryError.restoreRejected) { try HistoryRestore.isCommitted(state) }
		case .outcomeUnknown: #expect(throws: HistoryError.restoreOutcomeUnknown) { try HistoryRestore.isCommitted(state) }
		case .canceled: #expect(throws: CancellationError.self) { try HistoryRestore.isCommitted(state) }
		default: #expect(try !HistoryRestore.isCommitted(state))
		}
	}

	@Test func destinationWarningUsesExplicitVersioningState() {
		#expect(
			HistoryRestore.destinationExplanation(name: "Destination", versioning: RemoteVersioning.none).contains(
				"will not be archived"))
		#expect(HistoryRestore.destinationExplanation(name: "Destination", versioning: .simple).contains("is enabled"))
		#expect(
			HistoryRestore.destinationExplanation(name: "Destination", versioning: nil).contains("Waiting for Destination"))
	}

}
