// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// A store with a file on the gateway, and a staged edit of it
struct UploadFixture {
	let store: IndexStore
	let storage: RemoteStorage
	let edit: Data
	let staged: StagedFile
	let itemID: Int64

	init(size: Int = 200_000) throws {
		store = try makeRemoteStore()
		storage = RemoteStorage(baseURL: try makeTemporaryDirectory())
		try storage.createDirectories()
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt")])
		itemID = try #require(try store.item(folderID: "docs", path: "a.txt")).id
		var bytes = Data(count: size)
		for index in 0..<size {
			bytes[index] = UInt8(truncatingIfNeeded: index &* 7)
		}
		edit = bytes
		let source = try makeTemporaryDirectory().appending(path: "a.txt")
		try bytes.write(to: source)
		staged = try RemoteUploader.stage(source, in: storage, modifiedNs: 42)
	}
}

@Suite struct UploaderTests {
	@Test func stagingClonesAndHashes() throws {
		let fixture = try UploadFixture()
		let url = fixture.storage.url(relativePath: fixture.staged.relativePath)
		#expect(try Data(contentsOf: url) == fixture.edit)
		#expect(fixture.staged.size == Int64(fixture.edit.count))
		#expect(fixture.staged.digest == Data(SHA256.hash(data: fixture.edit)).base64URLEncodedString())
		#expect(fixture.staged.relativePath.hasPrefix("Staged/"))
		let permissions = try FileManager.default.attributesOfItem(atPath: url.path(percentEncoded: false))[.posixPermissions]
		#expect((permissions as? NSNumber)?.intValue == 0o444, "staged copies are immutable")
	}

	@Test func resumesFromTheConfirmedOffset() async throws {
		let fixture = try UploadFixture()
		let gateway = TusGateway()
		// The gateway already has the first 70,000 bytes (sent before the process was suspended)
		gateway.state.update { state in
			state.uploads["upl_9"] = TusGateway.Upload(
				length: Int64(fixture.edit.count), data: fixture.edit.prefix(70_000), digest: fixture.staged.digest)
		}
		let client = mockClient { gateway.handle($0) }
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		var claim = try #require(try fixture.store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try fixture.store.recordUploadCreated(
				claim,
				upload: UploadInfo(
					upload: "upl_9", token: "stu_9", offset: 0, length: Int64(fixture.edit.count),
					expires: .distantFuture),
				tokenReference: nil))

		var uploader = RemoteUploader(store: fixture.store, client: client, storage: fixture.storage)
		uploader.chunkSize = 64 << 10
		let done = try await uploader.upload(claim, remoteFolderID: testRemoteFolder)
		#expect(done.record.state == .uploaded && done.record.uploadOffset == Int64(fixture.edit.count))
		#expect(gateway.state.value.uploads["upl_9"]?.data == fixture.edit)

		let offsets = gateway.patches.compactMap { $0.header("Upload-Offset") }
		#expect(offsets == ["70000", "135536", "201072"].prefix(offsets.count).map { $0 })
		#expect(offsets.first == "70000", "no byte the gateway confirmed is sent again")
		for patch in gateway.patches {
			#expect(patch.header("Upload-Checksum") == "sha256 " + Data(SHA256.hash(data: patch.body)).base64EncodedString())
			#expect(patch.body.count <= 64 << 10)
		}
		#expect(gateway.state.value.requests.first?.method == "HEAD")
	}

	@Test func expiredUploadStartsAgainFromTheStagedFile() async throws {
		let fixture = try UploadFixture(size: 1000)
		let gateway = TusGateway()
		let client = mockClient { gateway.handle($0) }
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		var claim = try #require(try fixture.store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try fixture.store.recordUploadCreated(
				claim,
				upload: UploadInfo(upload: "upl_gone", token: "stu", offset: 0, length: 1000, expires: .distantPast),
				tokenReference: nil))
		let uploader = RemoteUploader(store: fixture.store, client: client, storage: fixture.storage)
		let done = try await uploader.upload(claim, remoteFolderID: testRemoteFolder)
		#expect(done.record.uploadID == "upl_1" && done.record.state == .uploaded)
		#expect(gateway.state.value.uploads["upl_1"]?.data == fixture.edit)
	}
}

@Suite struct OutboxTests {
	@Test func uploadsSubmitsAndPolls() async throws {
		let fixture = try UploadFixture(size: 5000)
		let gateway = TusGateway()
		gateway.state.update {
			$0.pendingPolls = 1
			$0.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: 5000, content: "edit")
		}
		let client = mockClient { gateway.handle($0) }
		let edit = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		var outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "app") { _ in client }
		outbox.pollInterval = 0

		// Submitted and pending, then polled until committed
		let outcomes = try await outbox.run()
		guard outcomes.count == 2, case .waiting(let pending, _) = outcomes[0], case .resolved(let committed) = outcomes[1]
		else {
			Issue.record("unexpected: \(outcomes)")
			return
		}
		#expect(pending.state == .pending && pending.serverOperationID == "op_1")
		#expect(pending.attempts == 0, "a poll is not a failed attempt")
		#expect(gateway.state.value.requests.filter { $0.path == "/v1/operations/op_1" }.count == 1)
		#expect(committed.state == .committed)
		let item = try #require(try fixture.store.item(id: fixture.itemID))
		#expect(item.pending == PendingChange.none && item.size == 5000)
		#expect(try fixture.store.remoteItem(itemID: fixture.itemID)?.revision == revision("after"))
		#expect(gateway.submissions.count == 1)
		let request = try RemoteJSON.decoder().decode(OperationRequest.self, from: gateway.submissions[0].body)
		#expect(request.id == edit.mutation?.operationID && request.kind == .replace)
		#expect(request.baseRevision == revision("itm_aa.txt") && request.onConflict == .keepBoth)
		#expect(request.uploadDigest == fixture.staged.digest)
	}

	@Test func lostResponseIsRecoveredWithTheSameOperation() async throws {
		let fixture = try UploadFixture(size: 3000)
		let gateway = TusGateway()
		gateway.state.update {
			$0.loseResponses = 1
			$0.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: 3000)
		}
		let client = mockClient { gateway.handle($0) }
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		let outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "extension") { _ in client }

		let first = try await outbox.run()
		guard case .waiting(let uncertain, _) = first.first else {
			Issue.record("not waiting: \(first)")
			return
		}
		#expect(uncertain.state == .submitted, "the request may have reached the gateway")

		// Later (here: once the backoff passed), the same request is sent again
		let claim = try #require(
			try fixture.store.claimMutation(owner: "app", duration: 60, now: Date().addingTimeInterval(3600)))
		guard case .resolved(let committed) = try await outbox.process(claim) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed)
		let submissions = gateway.submissions
		#expect(submissions.count == 2 && submissions[0].body == submissions[1].body)
		#expect(gateway.state.value.operations.count == 1, "one operation on the gateway")
		#expect(gateway.state.value.uploads.count == 1, "one upload")
	}

	/// An edit uploaded to the gateway, but not sent (its upload is not on `gateway`, as after it expired)
	func uploadedEdit(_ fixture: UploadFixture, expires: Date) throws -> String {
		let edit = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		var claim = try #require(try fixture.store.claimMutation(owner: "app", duration: 60))
		claim = try #require(
			try fixture.store.recordUploadCreated(
				claim,
				upload: UploadInfo(
					upload: "upl_gone", token: "stu_gone", offset: 0, length: fixture.staged.size, expires: expires),
				tokenReference: nil))
		claim = try #require(try fixture.store.recordUploadOffset(claim, offset: fixture.staged.size))
		#expect(claim.record.state == .uploaded)
		try fixture.store.releaseClaim(claim)
		return try #require(edit.mutation?.operationID)
	}

	/// An operation whose upload expired before it was sent uploads its staged file again, rather than failing
	@Test func expiredUploadIsMadeAgainBeforeTheSubmission() async throws {
		let fixture = try UploadFixture(size: 2000)
		let gateway = TusGateway()
		gateway.state.update { $0.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: 2000) }
		let operationID = try uploadedEdit(fixture, expires: .distantPast)
		let client = mockClient { gateway.handle($0) }
		let outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "app") { _ in client }
		guard case .resolved(let committed) = try await outbox.process(operationID: operationID) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed && committed.uploadID == "upl_1")
		let body = try #require(gateway.submissions.first?.body)
		let request = try RemoteJSON.decoder().decode(OperationRequest.self, from: body)
		#expect(gateway.submissions.count == 1 && request.upload == "upl_1")
		#expect(gateway.state.value.uploads["upl_1"]?.data == fixture.edit)
	}

	/// The gateway forgot the upload sooner than it said: the first submission is answered `404` (nothing stored), and
	/// the staged file is uploaded again for a second one, with the same operation ID
	@Test func unknownUploadOnTheFirstSubmissionIsMadeAgain() async throws {
		let fixture = try UploadFixture(size: 2000)
		let gateway = TusGateway()
		gateway.state.update { $0.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: 2000) }
		let operationID = try uploadedEdit(fixture, expires: .distantFuture)
		let client = mockClient { gateway.handle($0) }
		let outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "app") { _ in client }
		guard case .resolved(let committed) = try await outbox.process(operationID: operationID) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed)
		let requests = try gateway.submissions.map { try RemoteJSON.decoder().decode(OperationRequest.self, from: $0.body) }
		#expect(requests.map(\.upload) == ["upl_gone", "upl_1"])
		#expect(requests.allSatisfy { $0.id == operationID })
	}

	/// A `404` for an upload the gateway still has is about the item: the operation is rejected, once
	@Test func notFoundItemRejectsTheOperation() async throws {
		let fixture = try UploadFixture(size: 1000)
		let gateway = TusGateway()
		let client = mockClient { request in
			request.path == "/v1/operations" ? .error(404, code: "not_found") : gateway.handle(request)
		}
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		let outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "app") { _ in client }
		guard case .resolved(let failed) = try await outbox.run().first else {
			Issue.record("not resolved")
			return
		}
		#expect(failed.state == .failed && failed.lastError == RemoteErrorCode.notFound)
		#expect(gateway.state.value.uploads.count == 1, "uploaded once")
	}

	/// Upload tokens kept for background tasks are removed once the operation is done
	@Test func uploadTokenIsRemovedOnceCommitted() async throws {
		let fixture = try UploadFixture(size: 3000)
		let gateway = TusGateway()
		gateway.state.update { $0.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: 3000) }
		let credentials = InMemoryCredentialStore()
		let client = mockClient { gateway.handle($0) }
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		let outbox = Outbox(store: fixture.store, storage: fixture.storage, owner: "app", credentials: credentials) {
			_ in client
		}
		guard case .resolved(let committed) = try await outbox.run().first else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed && committed.uploadTokenReference == nil)
		#expect(credentials.tokenReferences.isEmpty)
		#expect(try fixture.store.releasedTokenReferences().isEmpty)
	}

	/// A gateway that confirms an append without its bytes would keep the upload looping
	@Test func appendMustAdvanceTheOffset() async throws {
		let fixture = try UploadFixture(size: 1000)
		let gateway = TusGateway()
		let client = mockClient { request in
			guard request.method == "PATCH" else {
				return gateway.handle(request)
			}
			return MockResponse(status: 204, headers: ["Upload-Offset": request.header("Upload-Offset") ?? "0"])
		}
		_ = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		let claim = try #require(try fixture.store.claimMutation(owner: "app", duration: 60))
		let uploader = RemoteUploader(store: fixture.store, client: client, storage: fixture.storage)
		await #expect(throws: RemoteError.protocolViolation("the gateway confirmed offset 0 after 1000 bytes at 0")) {
			try await uploader.upload(claim, remoteFolderID: testRemoteFolder)
		}
		#expect(gateway.patches.isEmpty, "the gateway never saw an append it could have stored")
	}

	@Test func rejectedMoveShowsTheGatewayState() async throws {
		let store = try makeRemoteStore()
		let storage = RemoteStorage(baseURL: try makeTemporaryDirectory())
		try store.snapshot([remoteRoot, remoteFile("itm_a", "a.txt"), remoteDirectory("itm_d", "d")])
		let a = try #require(try store.item(folderID: "docs", path: "a.txt"))
		let d = try #require(try store.item(folderID: "docs", path: "d"))
		let moved = try store.recordRemoteMove(itemID: a.id, toParentItemID: d.id, name: "b.txt")
		#expect(moved.item?.path == "d/b.txt")
		let current = remoteFile("itm_a", "a.txt", rev: "changed elsewhere")
		let client = mockClient { request in
			request.path == "/v1/operations" ? .error(412, code: "revision_mismatch", item: current) : .error(404, code: "x")
		}
		let outbox = Outbox(store: store, storage: storage, owner: "app") { _ in client }
		guard case .resolved(let failed) = try await outbox.run().first else {
			Issue.record("not resolved")
			return
		}
		#expect(failed.state == .failed && failed.lastError == "revision_mismatch")
		#expect(try store.item(id: a.id)?.path == "a.txt", "back where the gateway has it")
		#expect(try store.remoteItem(itemID: a.id)?.revision == revision("changed elsewhere"))
	}
}
