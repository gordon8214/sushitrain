// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import Testing

@testable import SynctrainFileProviderKit

/// Waits until the condition holds (false when it did not within the time)
func eventually(within timeout: Duration = .seconds(10), _ condition: () throws -> Bool) async rethrows -> Bool {
	let clock = ContinuousClock()
	let deadline = clock.now.advanced(by: timeout)
	while clock.now < deadline {
		if try condition() {
			return true
		}
		try? await Task.sleep(for: .milliseconds(20))
	}
	return try condition()
}

/// An edit of the fixture's file whose upload the gateway has in part, with the upload's token kept
struct PartialUpload {
	let fixture: UploadFixture
	let gateway = TusGateway()
	let host = uniqueHost()
	let credentials = InMemoryCredentialStore()
	let client: RemoteFileClient
	let operationID: String
	static let uploadID = "upl_9"
	static let tokenReference = "upload:upl_9"

	init(size: Int = 200_000, sent: Int = 70_000) throws {
		fixture = try UploadFixture(size: size)
		let gateway = gateway
		client = mockClient(host: host) { gateway.handle($0) }
		let edit = fixture.edit
		let digest = fixture.staged.digest
		gateway.state.update { state in
			state.uploads[Self.uploadID] = TusGateway.Upload(
				length: Int64(edit.count), data: edit.prefix(sent), digest: digest)
			state.resultItem = remoteFile("itm_a", "a.txt", rev: "after", size: Int64(edit.count))
		}
		let recorded = try fixture.store.recordRemoteContentChange(
			itemID: fixture.itemID, modifiedNanoseconds: 42, stagedFile: fixture.staged)
		operationID = try #require(recorded.mutation?.operationID)
		credentials.setToken("stu_9", reference: Self.tokenReference)
		var claim = try #require(try fixture.store.claimMutation(owner: "earlier", duration: 60))
		claim = try #require(
			try fixture.store.recordUploadCreated(
				claim,
				upload: UploadInfo(
					upload: Self.uploadID, token: "stu_9", offset: 0, length: Int64(edit.count),
					expires: .distantFuture),
				tokenReference: Self.tokenReference))
		claim = try #require(try fixture.store.recordUploadOffset(claim, offset: Int64(sent)))
		try fixture.store.releaseClaim(claim)
	}

	var store: IndexStore { fixture.store }
	var storage: RemoteStorage { fixture.storage }

	/// A session that runs its tasks against the mocked gateway (background sessions cannot be mocked)
	func transfers(identifier: String = "test.remote.app", onFinished: (@Sendable (TransferRecord) -> Void)? = nil)
		-> BackgroundTransfers
	{
		let host = host
		let gateway = gateway
		return BackgroundTransfers(
			identifier: identifier, store: store, storage: storage, credentials: credentials,
			configuration: { MockURLProtocol.configuration(host: host) { gateway.handle($0) } },
			onTransferFinished: onFinished)
	}

	func outbox(owner: String = "app", handoff: BackgroundHandoff? = nil) -> Outbox {
		let client = client
		var outbox = Outbox(store: store, storage: storage, owner: owner, credentials: credentials) { _ in client }
		outbox.pollInterval = 0
		outbox.handoff = handoff
		return outbox
	}

	/// A transfer as if a task of the session ran the rest of the upload
	func runningTask(session: String = "test.remote.extension") throws -> TransferRecord {
		let registered = try store.claimUploadTransfer(
			operationID: operationID, itemID: fixture.itemID, uploadID: Self.uploadID,
			tokenReference: Self.tokenReference, totalSize: Int64(fixture.edit.count), owner: "worker", duration: 60)
		let transfer = try #require(registered.transfer)
		let assigned = try store.assignTask(
			transferID: transfer.transferID, sessionIdentifier: session, taskIdentifier: 7,
			taskDescription: BackgroundTransfers.taskDescription(kind: .upload, transferID: transfer.transferID),
			offset: 70_000, claimedBy: "worker")
		#expect(assigned)
		return try #require(try store.transfer(id: transfer.transferID))
	}
}

@Suite struct BackgroundUploadTests {
	/// The app is about to lose its execution time: the rest of the upload goes to a task of its session, which sends it
	/// in one `PATCH` without a checksum, carrying the upload's token. Once the task finished, the operation is
	/// submitted, and nothing of the transfer is left.
	@Test func handoffSendsTheRestFromABackgroundTask() async throws {
		let upload = try PartialUpload()
		let finished = Locked<[TransferRecord]>([])
		let transfers = upload.transfers { record in finished.update { $0.append(record) } }
		let handoff = BackgroundHandoff(transfers: transfers, minimumRemainder: 1000)
		handoff.request()
		let outbox = upload.outbox(handoff: handoff)

		guard case .waiting(let waiting, let notBefore) = try await outbox.process(operationID: upload.operationID)
		else {
			Issue.record("not waiting for the task")
			return
		}
		#expect(waiting.state == .uploading && waiting.claimOwner == nil, "the claim is released")
		#expect((notBefore ?? .distantPast) > Date(), "looked at again later, or when the task ended")
		let transferID = try #require(handoff.handedOver[upload.operationID])
		#expect(await eventually { !finished.value.isEmpty }, "the task finished")
		let record = try #require(finished.value.first)
		#expect(record.transferID == transferID && record.state == .completed && record.taskDescription == nil)
		#expect(try upload.store.mutation(operationID: upload.operationID)?.notBefore == nil, "the task's end expedites it")

		let patches = upload.gateway.patches
		#expect(patches.count == 1, "no chunk was sent in the foreground")
		#expect(patches.first?.header("Upload-Offset") == "70000")
		#expect(patches.first?.header("Upload-Checksum") == nil, "the gateway keeps what an interrupted task sent")
		#expect(patches.first?.header("Authorization") == "Bearer stu_9", "the upload's token, not the credential")
		#expect(patches.first?.body == upload.fixture.edit.suffix(from: 70_000))
		#expect(upload.gateway.state.value.uploads[PartialUpload.uploadID]?.data == upload.fixture.edit)

		// The next process to look at it submits it (as the app does after the session's events)
		guard case .resolved(let committed) = try await upload.outbox().process(operationID: upload.operationID) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed && upload.gateway.submissions.count == 1)
		#expect(try upload.store.uploadTransfers(operationID: upload.operationID).isEmpty)
		let directory = upload.storage.transferURL(transferID)
		#expect(!FileManager.default.fileExists(atPath: directory.path(percentEncoded: false)), "the rest is removed")
		#expect(upload.credentials.tokenReferences.isEmpty, "the token is removed once nothing refers to it")
	}

	/// While a task (here: of the other process) sends the upload, the outbox sends no chunk and starts no other task;
	/// once the task ended, the foreground goes on from the offset the gateway confirms
	@Test func operationWaitsForTheTaskThatSendsIt() async throws {
		let upload = try PartialUpload()
		let transfer = try upload.runningTask()
		let appTransfers = upload.transfers()
		let handoff = BackgroundHandoff(transfers: appTransfers, minimumRemainder: 1000)
		handoff.request()

		guard case .waiting = try await upload.outbox(handoff: handoff).process(operationID: upload.operationID) else {
			Issue.record("not waiting")
			return
		}
		#expect(upload.gateway.state.value.requests.isEmpty, "nothing was sent")
		#expect(try upload.store.claimMutation(owner: "extension", duration: 60) == nil, "it waits for the task")
		let second = try upload.store.claimUploadTransfer(
			operationID: upload.operationID, itemID: nil, uploadID: PartialUpload.uploadID, tokenReference: nil,
			totalSize: 1, owner: "other", duration: 60)
		#expect(second.transfer == nil, "one task per upload")
		let record = try #require(try upload.store.mutation(operationID: upload.operationID))
		#expect(try upload.store.runningBackgroundUpload(of: record)?.transferID == transfer.transferID)

		// The task was cut off: what it sent stays, and the foreground sends the rest
		upload.gateway.state.update { state in
			state.uploads[PartialUpload.uploadID]?.data = upload.fixture.edit.prefix(150_000)
		}
		try upload.store.finishTask(transferID: transfer.transferID, state: .interrupted, error: "the network was lost")
		try upload.store.expediteMutation(operationID: upload.operationID)
		guard case .resolved(let committed) = try await upload.outbox().process(operationID: upload.operationID) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed)
		#expect(upload.gateway.patches.map { $0.header("Upload-Offset") } == ["150000"])
		#expect(try upload.store.uploadTransfers(operationID: upload.operationID).isEmpty)
	}

	/// A background task takes the worker's claim over in the update that records it; a foreground worker cannot claim a
	/// transfer a task runs, and a worker that lost its claim starts nothing
	@Test func aTransferHasOneTaskAtATime() async throws {
		let upload = try PartialUpload()
		let transfers = upload.transfers()
		let registered = try upload.store.claimUploadTransfer(
			operationID: upload.operationID, itemID: nil, uploadID: PartialUpload.uploadID, tokenReference: nil,
			totalSize: 10, owner: "a", duration: 60)
		let transfer = try #require(registered.transfer)
		let file = upload.storage.url(relativePath: upload.fixture.staged.relativePath)
		let request = try upload.client.appendRequest(
			uploadID: PartialUpload.uploadID, offset: 70_000, checksum: nil, token: "stu_9")
		#expect(throws: RemoteError.unavailable(retryAfter: 5)) {
			try transfers.startUpload(
				transferID: transfer.transferID, request: request, file: file, offset: 70_000, claimedBy: "b")
		}
		#expect(try upload.store.transfer(id: transfer.transferID)?.taskDescription == nil, "nothing was recorded")
		#expect(
			try !upload.store.claimTransfer(id: transfer.transferID, owner: "b", duration: 60), "a's claim is current")
		try transfers.startUpload(
			transferID: transfer.transferID, request: request, file: file, offset: 70_000, claimedBy: "a")
		let running = try #require(try upload.store.transfer(id: transfer.transferID))
		#expect(running.sessionIdentifier == transfers.identifier && running.owner == transfers.identifier)
		#expect(
			try !upload.store.claimTransfer(
				id: transfer.transferID, owner: "c", duration: 60, now: Date().addingTimeInterval(600)),
			"a task runs it")
		#expect(try !upload.store.renewTransferClaim(id: transfer.transferID, owner: "a", duration: 60))
		#expect(try await eventually { try upload.store.transfer(id: transfer.transferID)?.state == .completed })
	}

	/// After a relaunch, a process reconnects to its session: transfers whose task the session no longer has are
	/// interrupted, which lets their operations go on
	@Test func reconnectingFindsTasksThatWereLost() async throws {
		let upload = try PartialUpload()
		let transfer = try upload.runningTask(session: "test.remote.relaunched")
		let relaunched = upload.transfers(identifier: "test.remote.relaunched")
		let lost = try await relaunched.reconcile()
		#expect(lost.map(\.transferID) == [transfer.transferID])
		let reconciled = try #require(try upload.store.transfer(id: transfer.transferID))
		#expect(reconciled.state == .interrupted && reconciled.taskDescription == nil && reconciled.owner == nil)
		let record = try #require(try upload.store.mutation(operationID: upload.operationID))
		#expect(try upload.store.runningBackgroundUpload(of: record) == nil)
		#expect(try upload.store.claimMutation(owner: "app", duration: 60)?.operationID == upload.operationID)
	}

	/// A newer edit replaces the upload a task sends: the task no longer holds the operation back, and once it ended,
	/// its transfer, its copy of the rest and the old upload's token are removed
	@Test func supersededTransferIsRemovedWithItsToken() async throws {
		let upload = try PartialUpload()
		let transfer = try upload.runningTask()
		let directory = upload.storage.transferURL(transfer.transferID)
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		try Data("rest".utf8).write(to: directory.appending(path: BackgroundTransfers.uploadRestFileName))

		let source = try makeTemporaryDirectory().appending(path: "newer.txt")
		try Data("newer".utf8).write(to: source)
		let newer = try RemoteUploader.stage(source, in: upload.storage)
		_ = try upload.store.recordRemoteContentChange(
			itemID: upload.fixture.itemID, modifiedNanoseconds: 43, stagedFile: newer)
		let record = try #require(try upload.store.mutation(operationID: upload.operationID))
		#expect(record.uploadID == nil && record.uploadTokenReference == nil)
		#expect(try upload.store.runningBackgroundUpload(of: record) == nil, "the task sends an upload no longer used")
		upload.store.removeReleasedTokens(from: upload.credentials)
		#expect(upload.credentials.tokenReferences == [PartialUpload.tokenReference], "the task's transfer refers to it")
		#expect(try upload.store.removeIdleUploadTransfers().isEmpty, "the task still runs")

		try upload.store.finishTask(transferID: transfer.transferID, state: .interrupted, error: "canceled")
		let outbox = upload.outbox()
		outbox.removeIdleTransfers()
		outbox.removeReleasedTokens()
		#expect(try upload.store.transfer(id: transfer.transferID) == nil)
		#expect(!FileManager.default.fileExists(atPath: directory.path(percentEncoded: false)))
		#expect(upload.credentials.tokenReferences.isEmpty)
	}

	/// A task answered from another origin may have sent the token there: the upload starts again, with a new one
	@Test func taskAnsweredFromElsewhereRestartsTheUpload() async throws {
		let upload = try PartialUpload()
		let transfer = try upload.runningTask()
		try upload.store.finishTask(
			transferID: transfer.transferID, state: .failed,
			error: "\(BackgroundTransfers.protocolViolationPrefix): the response came from another origin")
		try upload.store.expediteMutation(operationID: upload.operationID)
		guard case .resolved(let committed) = try await upload.outbox().process(operationID: upload.operationID) else {
			Issue.record("not resolved")
			return
		}
		#expect(committed.state == .committed && committed.uploadID == "upl_1")
		#expect(upload.gateway.state.value.uploads["upl_1"]?.data == upload.fixture.edit)
		#expect(!upload.credentials.tokenReferences.contains(PartialUpload.tokenReference))
	}

	/// The extension hands an upload over once it ran for longer than its budget, and only a rest worth a task
	@Test func foregroundBudget() throws {
		let upload = try PartialUpload()
		let handoff = BackgroundHandoff(transfers: upload.transfers(), minimumRemainder: 100, foregroundBudget: 60)
		let start = Date()
		#expect(!handoff.shouldHandOver(remaining: 1000, uploadingSince: start, now: start.addingTimeInterval(59)))
		#expect(handoff.shouldHandOver(remaining: 1000, uploadingSince: start, now: start.addingTimeInterval(60)))
		#expect(!handoff.shouldHandOver(remaining: 99, uploadingSince: start, now: start.addingTimeInterval(600)))
		let requested = BackgroundHandoff(transfers: upload.transfers(), minimumRemainder: 100)
		#expect(!requested.shouldHandOver(remaining: 1000, uploadingSince: start, now: start.addingTimeInterval(600)))
		requested.request()
		#expect(requested.shouldHandOver(remaining: 1000, uploadingSince: start))
		#expect(!requested.shouldHandOver(remaining: 99, uploadingSince: start))
	}

	/// A request interrupts the chunk in flight rather than waiting for it, without canceling the caller
	@Test func requestInterruptsTheChunkInFlight() async throws {
		let upload = try PartialUpload()
		let handoff = BackgroundHandoff(transfers: upload.transfers())
		let started = Locked(false)
		let chunk = Task { () -> RemoteError? in
			do throws(RemoteError) {
				_ = try await handoff.interruptible { () async throws(RemoteError) -> Int in
					started.value = true
					do {
						try await Task.sleep(for: .seconds(30))
					}
					catch {
						throw .canceled
					}
					return 1
				}
				return nil
			}
			catch {
				return Task.isCancelled ? nil : error
			}
		}
		#expect(await eventually { started.value })
		handoff.request()
		#expect(await chunk.value == .canceled)
		// After a request, nothing more is sent this way
		await #expect(throws: RemoteError.canceled) {
			try await handoff.interruptible { () async throws(RemoteError) -> Int in
				do {
					try await Task.sleep(for: .seconds(30))
				}
				catch {
					throw .canceled
				}
				return 1
			}
		}
	}

	/// The system's handler for a session's events is called exactly once, after they were delivered
	@Test func eventsHandlersAreCalledOnce() async throws {
		let upload = try PartialUpload()
		let transfers = upload.transfers()
		let calls = Locked(0)
		transfers.handleEvents { calls.update { $0 += 1 } }
		transfers.handleEvents { calls.update { $0 += 10 } }
		#expect(transfers.isConnected)
		transfers.urlSessionDidFinishEvents(forBackgroundURLSession: transfers.session)
		transfers.urlSessionDidFinishEvents(forBackgroundURLSession: transfers.session)
		#expect(await eventually { calls.value == 11 })
		try await Task.sleep(for: .milliseconds(100))
		#expect(calls.value == 11)
		transfers.disconnect()
		#expect(!transfers.isConnected)
	}

	/// A new instance of the extension does not wait out the claims an instance that was terminated left
	@Test func earlierInstancesClaimsAreReleased() throws {
		let store = try makeStore()
		let (download, _) = try store.downloadTransfer(itemID: 1, contentID: ContentIdentity("c1.x.10"), totalSize: 10)
		let (current, _) = try store.downloadTransfer(itemID: 2, contentID: ContentIdentity("c1.y.10"), totalSize: 10)
		let (app, _) = try store.downloadTransfer(itemID: 3, contentID: ContentIdentity("c1.z.10"), totalSize: 10)
		#expect(try store.claimTransfer(id: download.transferID, owner: "extension.old/1", duration: 600))
		#expect(try store.claimTransfer(id: current.transferID, owner: "extension.new/1", duration: 600))
		#expect(try store.claimTransfer(id: app.transferID, owner: "app-1234/1", duration: 600))
		let scheduler = TransferScheduler(store: store, owner: "extension.old")
		_ = try scheduler.tryAcquire(.content)
		try store.releaseClaims(ownerPrefix: "extension.", except: "extension.new")
		#expect(try store.transfer(id: download.transferID)?.owner == nil)
		#expect(try store.transfer(id: current.transferID)?.owner == "extension.new/1")
		#expect(try store.transfer(id: app.transferID)?.owner == "app-1234/1")
		#expect(try store.activeTransferSlots(.content) == 0)
		#expect(try store.claimTransfer(id: download.transferID, owner: "extension.new/2", duration: 60))
	}
}

@Suite struct InterruptedDownloadTests {
	/// The extension's download is canceled (the system took its time back) or the extension is terminated: what was
	/// verified stays, and the next download (of a new instance, which does not wait out the old claim) resumes from it
	@Test func canceledDownloadKeepsItsVerifiedBlocks() async throws {
		let gateway = ContentGateway(size: 6 * 128 * 1024)
		let reached = Locked(false)
		let stall = Locked(true)
		let client = mockClient { request in
			if request.path == "/v1/content/\(gateway.leaseID)", request.header("Range")?.hasPrefix("bytes=0-") == false,
				stall.value
			{
				reached.value = true
				Thread.sleep(forTimeInterval: 1)
			}
			return gateway.handle(request)
		}
		let store = try makeStore()
		var downloader = RemoteDownloader(
			store: store, client: client, storage: RemoteStorage(baseURL: try makeTemporaryDirectory()),
			credentials: InMemoryCredentialStore(), owner: "extension.old")
		downloader.rangeTarget = Int64(gateway.blockSize * 2)
		downloader.parallelRanges = 1
		let first = downloader
		let download = Task { try await first.download(gateway.request) }
		#expect(await eventually { reached.value })
		download.cancel()
		await #expect(throws: RemoteError.canceled) { try await download.value }

		let transfer = try #require(try store.transfers().first)
		#expect(transfer.state == .interrupted && transfer.owner == nil)
		let verified = try store.verifiedBlocks(transferID: transfer.transferID)
		#expect(verified.verifiedCount == 2 && verified.contains(0) && verified.contains(1))

		// Terminated instead, with the claim still current: a new instance releases it and goes on right away
		#expect(try store.claimTransfer(id: transfer.transferID, owner: "extension.old/1", duration: 600))
		try store.releaseClaims(ownerPrefix: "extension.", except: "extension.new")
		stall.value = false
		var resumed = RemoteDownloader(
			store: store, client: client, storage: downloader.storage, credentials: InMemoryCredentialStore(),
			owner: "extension.new")
		resumed.rangeTarget = Int64(gateway.blockSize * 2)
		let url = try await resumed.download(gateway.request)
		#expect(try Data(contentsOf: url) == gateway.content)
		#expect(gateway.rangeHeaders.filter { $0.hasPrefix("bytes=0-") }.count == 1, "verified blocks are not fetched again")
	}
}
