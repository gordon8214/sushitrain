// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// A gateway that keeps versions of one file, and carries out operations after one poll
final class VersionGateway: Sendable {
	struct State: Sendable {
		var features = ["longPoll", "versions"]
		var apiVersion = 1
		/// Polls answered as unavailable before the operation's state, and whether operations never finish
		var failedPolls = 0
		var neverFinishes = false
		/// The error the next submitted operation fails with
		var failure: ErrorDetail?
		/// IDs the version list reports instead of the real ones
		var listedIDs: [String]?
		var versions: [RemoteVersion] = []
		var contents: [String: Data] = [:]
		var submitted: [OperationRequest] = []
		/// Answers to submissions, in order (pending, committed after one poll, otherwise)
		var answers: [MockResponse] = []
		var polls = 0
		var contentRequests = 0
		var refreshes = 0
	}

	let state = Locked(State())
	let file: RemoteItem

	init(file: RemoteItem) {
		self.file = file
	}

	func handle(_ request: MockRequest) -> MockResponse {
		let path = request.path
		switch (request.method, path) {
		case ("GET", "/v1/capabilities"):
			var capabilities = (try? fixture("capabilities.json", as: Capabilities.self))!
			capabilities.features = state.value.features
			capabilities.apiVersion = state.value.apiVersion
			capabilities.serviceID = testServiceID
			return .json(capabilities)
		case ("GET", "/v1/items/\(file.id)/versions"):
			var versions = state.value.versions
			if let ids = state.value.listedIDs {
				for (index, id) in ids.enumerated() {
					versions[index].id = id
				}
			}
			return .json(
				RemoteVersionList(itemID: file.id, revision: file.revision, versioning: .staggered, versions: versions))
		case ("POST", "/v1/operations"):
			let submitted = try? RemoteJSON.decoder().decode(OperationRequest.self, from: request.body)
			return state.update { state in
				if let submitted {
					state.submitted.append(submitted)
				}
				if let failure = state.failure {
					state.failure = nil
					return .json(self.operation(submitted, state: .failed, error: failure), status: 201)
				}
				guard !state.answers.isEmpty else {
					return .json(self.operation(submitted, state: .pending), status: 202)
				}
				return state.answers.removeFirst()
			}
		case ("GET", "/v1/operations/op_1"):
			let (submitted, failed, pending) = state.update { state in
				state.polls += 1
				let failed = state.failedPolls > 0
				state.failedPolls -= failed ? 1 : 0
				return (state.submitted.last, failed, state.neverFinishes)
			}
			if failed {
				return .error(503, code: RemoteErrorCode.contentUnavailable)
			}
			return .json(operation(submitted, state: pending ? .pending : .committed))
		case ("GET", "/v1/folders/\(testRemoteFolder)/changes"):
			state.update { $0.refreshes += 1 }
			return .json(ChangesPage(changes: [], cursor: request.query("cursor") ?? "g1.10", more: false))
		default:
			let prefix = "/v1/items/\(file.id)/versions/"
			if request.method == "GET", path.hasPrefix(prefix), path.hasSuffix("/content") {
				let id = String(path.dropFirst(prefix.count).dropLast("/content".count))
				return state.update { state in
					state.contentRequests += 1
					guard let body = state.contents[id] else {
						return .error(404, code: RemoteErrorCode.versionNotFound)
					}
					return MockResponse(status: 200, body: body)
				}
			}
			return .error(404, code: RemoteErrorCode.notFound)
		}
	}

	func operation(_ request: OperationRequest?, state: RemoteOperationState, error: ErrorDetail? = nil)
		-> RemoteOperation
	{
		RemoteOperation(
			id: request?.id ?? "", operation: "op_1", kind: request?.kind ?? .replace, state: state,
			item: state == .committed ? file : nil, conflictItem: nil, contentID: nil, error: error,
			authorityGeneration: 3)
	}
}

@Suite struct RemoteVersionTests {
	static let file = remoteFile("itm_RgxMZ6JxNZWvbgMJ4rAhF6", "notes.txt", size: 7, content: "current")
	static let older = RemoteVersion(
		id: "ver_1792545380", versionTime: Date(timeIntervalSince1970: 1_792_545_380), modifiedNs: 1_792_545_000_000_000_000,
		size: 5)
	static let newer = RemoteVersion(
		id: "ver_1793150180", versionTime: Date(timeIntervalSince1970: 1_793_150_180), modifiedNs: 1_793_149_999_000_000_000,
		size: 6)

	/// A store with the file, a gateway keeping its versions, and a browser of it
	func browser(write: Bool = true) throws -> (IndexStore, VersionGateway, RemoteVersionBrowser) {
		let store = try makeRemoteStore(write: write)
		try store.snapshot([remoteRoot, Self.file, remoteDirectory("itm_dirdirdirdirdirdirdirdi", "dir")])
		let itemID = try #require(try store.remoteItem(gatewayID: Self.file.id)?.itemID)
		let gateway = VersionGateway(file: Self.file)
		gateway.state.update { state in
			state.versions = [Self.newer, Self.older]
			state.contents = [Self.older.id: Data("older".utf8), Self.newer.id: Data("newer!".utf8)]
		}
		let client = mockClient(handler: gateway.handle)
		var browser = try RemoteVersionBrowser(store: store, itemID: itemID) { _ in client }
		browser.pollDelay = { _ in .milliseconds(1) }
		return (store, gateway, browser)
	}

	@Test func onlyFilesOfGatewayFoldersHaveVersions() throws {
		let (store, _, _) = try browser()
		let directory = try #require(try store.remoteItem(gatewayID: "itm_dirdirdirdirdirdirdirdi")?.itemID)
		#expect(throws: RemoteVersionBrowser.Failure.notAvailable) {
			try RemoteVersionBrowser(store: store, itemID: directory) { _ in mockClient { _ in .error(500, code: "x") } }
		}
		#expect(throws: RemoteVersionBrowser.Failure.notAvailable) {
			try RemoteVersionBrowser(store: store, itemID: 999_999) { _ in mockClient { _ in .error(500, code: "x") } }
		}
	}

	@Test func listsVersionsOfGatewaysThatSupportThem() async throws {
		let (_, gateway, browser) = try browser()
		let list = try await browser.versions()
		#expect(list.versions.map(\.id) == [Self.newer.id, Self.older.id] && list.versioning == .staggered)

		gateway.state.update { $0.features = ["longPoll"] }
		await #expect(throws: RemoteVersionBrowser.Failure.unsupported) {
			try await browser.versions()
		}
	}

	@Test func versioningOfCustomPathsIsKnown() throws {
		let decoded = try RemoteJSON.decoder().decode([RemoteVersioning].self, from: Data(#"["customPath","x"]"#.utf8))
		#expect(decoded == [.customPath, .unknown("x")] && RemoteVersioning.customPath.rawValue == "customPath")
	}

	@Test func versionsWithUnchangedContentAreHidden() {
		let second: Int64 = 1_000_000_000
		let versions = [
			RemoteVersion(id: "ver_5", versionTime: .now, modifiedNs: 40 * second, size: 10),  // the current content
			RemoteVersion(id: "ver_4", versionTime: .now, modifiedNs: 30 * second, size: 10),
			RemoteVersion(id: "ver_3", versionTime: .now, modifiedNs: 30 * second, size: 10),  // metadata changed only
			RemoteVersion(id: "ver_2", versionTime: .now, modifiedNs: 30 * second, size: 12),
			RemoteVersion(id: "ver_1", versionTime: .now, modifiedNs: 20 * second, size: 12),
		]
		let shown = RemoteVersionBrowser.distinct(versions, currentModifiedNs: 40 * second + 123, currentSize: 10)
		#expect(shown.map(\.id) == ["ver_4", "ver_2", "ver_1"])
	}

	@Test func copiesAreNamedAfterTheVersion() {
		let utc = TimeZone(identifier: "UTC")!
		let name = RemoteVersionBrowser.copyName(of: "Notes.final.txt", version: Self.older, taken: [], timeZone: utc)
		#expect(name == "Notes.final (2026-10-21 01.10).txt")
		let taken = Set([name].map(FileNames.collisionKey))
		let next = RemoteVersionBrowser.copyName(
			of: "Notes.final.txt", version: Self.older, taken: taken, timeZone: utc)
		#expect(next == name.replacingOccurrences(of: ").txt", with: ") 2.txt"))
		#expect(RemoteVersionBrowser.copyName(of: "Makefile", version: Self.older, taken: []).hasSuffix(")"))
		// Long names are shortened, on character boundaries, to fit with a number
		let long = String(repeating: "é", count: 200) + ".txt"
		let short = RemoteVersionBrowser.copyName(of: long, version: Self.older, taken: [])
		#expect(short.utf8.count <= 253 && short.hasSuffix(").txt") && short.hasPrefix("éé"))
	}

	@Test func downloadsAVersionOnce() async throws {
		let (_, gateway, browser) = try browser()
		let directory = try makeTemporaryDirectory()
		defer { try? FileManager.default.removeItem(at: directory) }
		let url = try await browser.download(Self.older, into: directory)
		#expect(url.lastPathComponent == "notes.txt")
		#expect(try String(contentsOf: url, encoding: .utf8) == "older")
		_ = try await browser.download(Self.older, into: directory)
		#expect(gateway.state.value.contentRequests == 1)

		let gone = RemoteVersion(id: "ver_1", versionTime: .now, modifiedNs: 0, size: 1)
		await #expect(throws: RemoteVersionBrowser.Failure.remote(.versionNotFound)) {
			try await browser.download(gone, into: directory)
		}
		let malformed = RemoteVersion(id: "../../escape", versionTime: .now, modifiedNs: 0, size: 1)
		await #expect(throws: RemoteVersionBrowser.Failure.remote(.protocolViolation("malformed version or item ID"))) {
			try await browser.download(malformed, into: directory)
		}
	}

	@Test func restoringWaitsForTheGatewayAndRefreshes() async throws {
		let (_, gateway, browser) = try browser()
		let operation = try await browser.restore(Self.older)
		#expect(operation.state == .committed)
		let state = gateway.state.value
		let request = try #require(state.submitted.first)
		#expect(request.kind == .replace && request.itemID == Self.file.id && request.baseRevision == Self.file.revision)
		#expect(request.sourceVersion == Self.older.id && request.sourceItemID == nil && request.onConflict == .reject)
		#expect(request.upload == nil && request.folderID == testRemoteFolder)
		#expect(state.polls == 1 && state.refreshes == 1)
	}

	@Test func restoringNeedsAWritableFolderWithoutLocalChanges() async throws {
		let (_, _, readOnly) = try browser(write: false)
		await #expect(throws: RemoteVersionBrowser.Failure.readOnly) {
			try await readOnly.restore(Self.older)
		}

		let (store, gateway, browser) = try browser()
		_ = try store.recordRemoteContentChange(
			itemID: browser.itemID, modifiedNanoseconds: 5_000, stagedFile: staged("edit"))
		await #expect(throws: RemoteVersionBrowser.Failure.pendingChange) {
			try await browser.restore(Self.older)
		}
		#expect(gateway.state.value.submitted.isEmpty)
	}

	@Test func restoringAChangedFileIsRejected() async throws {
		let (_, gateway, browser) = try browser()
		gateway.state.update { $0.answers = [.error(412, code: RemoteErrorCode.revisionMismatch, item: Self.file)] }
		await #expect(throws: RemoteVersionBrowser.Failure.remote(.revisionMismatch(current: Self.file))) {
			try await browser.restore(Self.older)
		}
	}

	@Test func failedOperationsReportTheirError() async throws {
		let (_, gateway, browser) = try browser()
		gateway.state.update { $0.failure = ErrorDetail(code: RemoteErrorCode.versionNotFound, message: "gone") }
		await #expect(throws: RemoteVersionBrowser.Failure.remote(.versionNotFound)) {
			try await browser.restore(Self.older)
		}
	}

	@Test func operationsOutlastPassingFailures() async throws {
		let (_, gateway, browser) = try browser()
		gateway.state.update { $0.failedPolls = 2 }
		let operation = try await browser.restore(Self.older)
		#expect(operation.state == .committed && gateway.state.value.polls == 3)
		#expect(gateway.state.value.submitted.count == 1)
	}

	@Test func operationsThatDontFinishInTimeHaveAnUnknownOutcome() async throws {
		var (_, gateway, browser) = try browser()
		gateway.state.update { $0.neverFinishes = true }
		browser.pollDelay = { _ in .milliseconds(20) }
		browser.operationDeadline = .milliseconds(100)
		await #expect(throws: RemoteVersionBrowser.Failure.outcomeUnknown) {
			try await browser.restore(Self.older)
		}
		#expect(gateway.state.value.submitted.count == 1 && gateway.state.value.refreshes == 0)
	}

	@Test func malformedVersionsAndNewerGatewaysAreRefused() async throws {
		let (_, gateway, browser) = try browser()
		gateway.state.update { $0.listedIDs = ["../escape"] }
		await #expect(throws: RemoteVersionBrowser.Failure.self) {
			try await browser.versions()
		}
		gateway.state.update {
			$0.listedIDs = nil
			$0.apiVersion = 2
		}
		await #expect(throws: RemoteVersionBrowser.Failure.outdated) {
			try await browser.versions()
		}
	}

	@Test func copiesTryAnotherNameWhenTaken() async throws {
		let (_, gateway, browser) = try browser()
		gateway.state.update { $0.answers = [.error(409, code: RemoteErrorCode.nameConflict)] }
		let operation = try await browser.saveCopy(of: Self.newer)
		#expect(operation.state == .committed)
		let submitted = gateway.state.value.submitted
		#expect(submitted.count == 2 && submitted[0].name != submitted[1].name && submitted[0].id != submitted[1].id)
		let request = try #require(submitted.last)
		#expect(request.kind == .create && request.itemKind == .file && request.parentID == testRootGatewayID)
		#expect(request.sourceItemID == Self.file.id && request.sourceVersion == Self.newer.id && request.upload == nil)
		#expect(request.name?.hasPrefix("notes (") == true && request.name?.hasSuffix(") 2.txt") == true)
	}

	@Test func requestsWithoutVersionsEncodeAsBefore() throws {
		let replace = OperationRequest.replace(
			id: "5d2c9e10-8f7b-4a3e-b1c2-3e4f5a6b7c8d", folderID: "Documents", itemID: "itm_x", baseRevision: revision("x"),
			upload: "upl_x", uploadDigest: "d", modifiedNs: 1)
		let keys = try #require(try JSONSerialization.jsonObject(with: replace.canonicalJSON()) as? [String: Any]).keys
		#expect(!keys.contains("sourceVersion") && !keys.contains("sourceItemID"))
	}
}
