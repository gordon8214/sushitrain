// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation
import Testing

@testable import SynctrainFileProviderKit

@Suite struct DownloaderTests {
	/// A downloader for the gateway, with small ranges (two blocks each) so that files have several
	func makeDownloader(_ gateway: ContentGateway, store: IndexStore? = nil, rangeBlocks: Int = 2) throws
		-> RemoteDownloader
	{
		let client = mockClient { gateway.handle($0) }
		var downloader = RemoteDownloader(
			store: try store ?? makeStore(), client: client, storage: RemoteStorage(baseURL: try makeTemporaryDirectory()),
			scheduler: nil, credentials: InMemoryCredentialStore(), owner: "test")
		downloader.rangeTarget = Int64(gateway.blockSize * rangeBlocks)
		downloader.manifestPageLimit = 3
		return downloader
	}

	@Test func downloadsVerifiedRanges() async throws {
		let gateway = ContentGateway(size: 5 * 128 * 1024 + 1000)
		let downloader = try makeDownloader(gateway)
		let progress = Locked<[Int64]>([])
		let url = try await downloader.download(gateway.request) { verified, _ in
			progress.update { $0.append(verified) }
		}
		#expect(try Data(contentsOf: url) == gateway.content)
		#expect(url.lastPathComponent == gateway.contentID.rawValue)
		#expect(gateway.rangeHeaders.count == 3, "six blocks in ranges of two")
		#expect(progress.value.max() == Int64(gateway.content.count))
		// Every request carries the credential in the header, never in the URL
		for request in gateway.state.value.requests {
			#expect(request.header("Authorization") == "Bearer stc_test")
			#expect(request.url.query?.contains("stc_") != true)
			#expect(request.header(RemoteFileClient.correlationHeader) != nil)
		}
		#expect(try downloader.store.transfers().isEmpty, "the transfer is forgotten once published")
		// Downloaded once: the cached copy is used from then on
		_ = try await downloader.download(gateway.request)
		#expect(gateway.rangeHeaders.count == 3)
	}

	@Test func tamperedBlockIsRefetchedAndThenRejected() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		gateway.state.update { $0.tamperedBlocks = [1] }
		let downloader = try makeDownloader(gateway)
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		let verified = try downloader.store.verifiedBlocks(transferID: transfer.transferID)
		#expect(verified.contains(0) && !verified.contains(1), "only the good block of the range is kept")
		#expect(try downloader.cachedContent(gateway.contentID) == nil, "nothing is published")

		// Once the gateway serves good bytes, the download resumes from the verified blocks
		gateway.state.update { $0.tamperedBlocks = [] }
		let before = gateway.rangeHeaders.count
		let url = try await downloader.download(gateway.request)
		#expect(try Data(contentsOf: url) == gateway.content)
		#expect(gateway.rangeHeaders[before...] == ["bytes=\(128 * 1024)-\(2 * 128 * 1024 - 1)"], "block 1 only")
	}

	@Test func truncatedBodyKeepsWhatWasVerified() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		// The first range stops 100 bytes into its second block
		gateway.state.update { $0.truncateNext = [128 * 1024 + 100] }
		var downloader = try makeDownloader(gateway)
		downloader.parallelRanges = 1
		let url = try await downloader.download(gateway.request)
		#expect(try Data(contentsOf: url) == gateway.content)
		let ranges = gateway.rangeHeaders
		#expect(ranges.first == "bytes=0-\(2 * 128 * 1024 - 1)")
		#expect(ranges.contains("bytes=\(128 * 1024)-\(2 * 128 * 1024 - 1)"), "only the missing block again")
	}

	@Test func wholeContentForARangeIsAProtocolViolation() async throws {
		let gateway = ContentGateway(size: 8 * 128 * 1024)
		gateway.state.update { $0.ignoreRanges = true }
		let downloader = try makeDownloader(gateway)
		await #expect {
			try await downloader.download(gateway.request)
		} throws: { error in
			guard case RemoteError.protocolViolation = error else {
				return false
			}
			return true
		}
		let transfer = try #require(try downloader.store.transfers().first)
		#expect(try downloader.store.verifiedBlocks(transferID: transfer.transferID).verifiedCount == 0)
		#expect(gateway.rangeHeaders.count <= downloader.parallelRanges, "not repeated for every range")
	}

	@Test func changedValidatorStartsOver() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		gateway.state.update { $0.changeValidatorAfter = 1 }
		var downloader = try makeDownloader(gateway)
		downloader.parallelRanges = 1
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		#expect(try downloader.store.verifiedBlocks(transferID: transfer.transferID).verifiedCount == 0)
		#expect(transfer.entityTag == nil, "the progress of the old representation is gone")
		// The resumed request carried the original validator
		let ifRanges = gateway.state.value.requests.compactMap { $0.header("If-Range") }
		#expect(ifRanges == [gateway.contentID.entityTag])
	}

	@Test func manifestMustMatchTheContentIdentity() async throws {
		let gateway = ContentGateway(size: 3 * 128 * 1024)
		gateway.state.update { $0.forgedManifest = true }
		let downloader = try makeDownloader(gateway)
		await #expect {
			try await downloader.download(gateway.request)
		} throws: { error in
			error as? RemoteError == .protocolViolation("the manifest does not match the content identity")
		}
		#expect(gateway.rangeHeaders.isEmpty, "no content is requested")
	}

	@Test func waitsForPreparation() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		gateway.state.update { $0.preparation = [0, 128 * 1024, 3 * 128 * 1024] }
		let downloader = try makeDownloader(gateway)
		let url = try await downloader.download(gateway.request)
		#expect(try Data(contentsOf: url) == gateway.content)
		#expect(gateway.state.value.leaseRequests >= 3)
	}

	@Test func emptyFile() async throws {
		let gateway = ContentGateway(size: 0)
		let downloader = try makeDownloader(gateway)
		let url = try await downloader.download(gateway.request)
		#expect(try Data(contentsOf: url).isEmpty)
		#expect(gateway.rangeHeaders.isEmpty)
	}

	/// What a background task downloaded (everything from the first missing block) is verified once it finished
	@Test func backgroundDownloadIsVerifiedAfterCompletion() async throws {
		let gateway = ContentGateway(size: 6 * 128 * 1024)
		gateway.state.update { $0.tamperedBlocks = [4] }
		var downloader = try makeDownloader(gateway)
		downloader.parallelRanges = 1
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		#expect(try downloader.store.verifiedBlocks(transferID: transfer.transferID).verifiedCount == 4)

		// The background session's task downloaded bytes=524288- and its delegate kept the file
		let offset = Int64(4 * 128 * 1024)
		try downloader.store.assignTask(
			transferID: transfer.transferID, sessionIdentifier: "test.remote.app", taskIdentifier: 7,
			taskDescription: BackgroundTransfers.taskDescription(kind: .download, transferID: transfer.transferID),
			offset: offset)
		let directory = downloader.storage.transferURL(transfer.transferID)
		try gateway.content[Int(offset)...].write(to: directory.appending(path: BackgroundTransfers.downloadedFileName))
		try downloader.store.finishTask(transferID: transfer.transferID, state: .downloaded, error: nil)

		let url = try #require(try downloader.finishBackgroundDownload(transferID: transfer.transferID))
		#expect(try Data(contentsOf: url) == gateway.content)
		#expect(try downloader.store.transfer(id: transfer.transferID) == nil)
	}

	@Test func damagedBackgroundDownloadKeepsOnlyVerifiedBlocks() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		gateway.state.update { $0.tamperedBlocks = [2] }
		var downloader = try makeDownloader(gateway)
		downloader.parallelRanges = 1
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		let offset = Int64(2 * 128 * 1024)
		try downloader.store.assignTask(
			transferID: transfer.transferID, sessionIdentifier: "s", taskIdentifier: 1,
			taskDescription: BackgroundTransfers.taskDescription(kind: .download, transferID: transfer.transferID),
			offset: offset)
		var damaged = gateway.content[Int(offset)...]
		damaged[damaged.startIndex + 128 * 1024 + 5] ^= 1
		try Data(damaged).write(
			to: downloader.storage.transferURL(transfer.transferID).appending(path: BackgroundTransfers.downloadedFileName))
		try downloader.store.finishTask(transferID: transfer.transferID, state: .downloaded, error: nil)
		#expect(try downloader.finishBackgroundDownload(transferID: transfer.transferID) == nil)
		let verified = try downloader.store.verifiedBlocks(transferID: transfer.transferID)
		#expect(verified.contains(2) && !verified.contains(3))

		// The foreground fetches the rest
		gateway.state.update { $0.tamperedBlocks = [] }
		let url = try await downloader.download(gateway.request)
		#expect(try Data(contentsOf: url) == gateway.content)
	}

	/// Two downloads of one item in one process (the system asks twice): one runs the transfer, the other waits for it
	/// and returns the published file, so no block is fetched or written twice and the claims never mix
	@Test func concurrentDownloadsOfOneItemInOneProcess() async throws {
		let gateway = ContentGateway(size: 5 * 128 * 1024 + 1000)
		var downloader = try makeDownloader(gateway)
		downloader.claimPollInterval = .milliseconds(10)
		let worker = downloader
		async let first = worker.download(gateway.request)
		async let second = worker.download(gateway.request)
		let (a, b) = try await (first, second)
		#expect(a == b)
		#expect(try Data(contentsOf: a) == gateway.content)
		#expect(gateway.rangeHeaders.count == 3, "every range is fetched once")
		#expect(Set(gateway.rangeHeaders).count == 3)
		#expect(try downloader.store.transfers().isEmpty)
	}

	/// A block is hashed before it is copied: the partial file holds no byte of a block that failed verification
	@Test func unverifiedBytesNeverReachThePartialFile() async throws {
		let gateway = ContentGateway(size: 2 * 128 * 1024)
		gateway.state.update { $0.tamperedBlocks = [1] }
		let downloader = try makeDownloader(gateway)
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		let partial = try Data(
			contentsOf: downloader.storage.transferURL(transfer.transferID).appending(path: "content.partial"))
		#expect(partial.count == gateway.content.count)
		#expect(partial.prefix(128 * 1024) == gateway.content.prefix(128 * 1024), "the verified block is written")
		#expect(partial.dropFirst(128 * 1024).allSatisfy { $0 == 0 }, "the tampered block is never written")
	}

	/// A download whose claim another worker took over (its own expired while it waited) stops, and leaves the
	/// transfer to that worker
	@Test func lostClaimStopsTheDownload() async throws {
		let gateway = ContentGateway(size: 4 * 128 * 1024)
		// The gateway prepares the content for a while, so the download waits with its claim
		gateway.state.update { $0.preparation = Array(repeating: 0, count: 60) }
		var downloader = try makeDownloader(gateway)
		downloader.claimDuration = 0.3
		let worker = downloader
		let download = Task { try await worker.download(gateway.request) }
		var claimed: TransferRecord?
		for _ in 0..<300 where claimed == nil {
			try await Task.sleep(for: .milliseconds(10))
			claimed = try downloader.store.transfers().first { $0.owner != nil }
		}
		let transferID = try #require(claimed?.transferID)
		#expect(claimed?.owner?.hasPrefix("test/") == true, "claimed for this call")
		#expect(
			try downloader.store.claimTransfer(
				id: transferID, owner: "thief", duration: 60, now: Date().addingTimeInterval(1)))
		await #expect(throws: RemoteDownloader.claimLost) { try await download.value }
		#expect(try downloader.store.transfer(id: transferID)?.owner == "thief", "the other worker keeps it")
		#expect(try downloader.cachedContent(gateway.contentID) == nil)
	}

	@Test func claimsAreExclusive() throws {
		let store = try makeStore()
		let (transfer, _) = try store.downloadTransfer(itemID: 1, contentID: ContentIdentity("c1.x.10"), totalSize: 10)
		let id = transfer.transferID
		#expect(try store.claimTransfer(id: id, owner: "a", duration: 60))
		#expect(try !store.claimTransfer(id: id, owner: "a", duration: 60), "not even for the same owner")
		#expect(try !store.claimTransfer(id: id, owner: "b", duration: 60))
		#expect(try store.renewTransferClaim(id: id, owner: "a", duration: 60))
		#expect(try !store.renewTransferClaim(id: id, owner: "b", duration: 60))
		// A background task takes the claim over only from the worker that holds it
		#expect(
			try !store.assignTask(
				transferID: id, sessionIdentifier: "s", taskIdentifier: 1, taskDescription: "d", offset: 0,
				claimedBy: "b"))
		#expect(
			try store.assignTask(
				transferID: id, sessionIdentifier: "s", taskIdentifier: 1, taskDescription: "d", offset: 0,
				claimedBy: "a"))
		#expect(try !store.renewTransferClaim(id: id, owner: "a", duration: 60), "the task has it now")
		#expect(try !store.claimTransfer(id: id, owner: "c", duration: 60, now: Date().addingTimeInterval(600)))
		// Once the task is detached, a task's end no longer touches the transfer another worker claimed
		try store.finishTask(transferID: id, state: .downloaded, error: nil)
		#expect(try store.claimTransfer(id: id, owner: "c", duration: 60))
		try store.finishTask(transferID: id, state: .interrupted, error: "late")
		#expect(try store.transfer(id: id)?.owner == "c")
	}

	@Test func finishingABackgroundDownloadNeedsTheClaim() async throws {
		let gateway = ContentGateway(size: 2 * 128 * 1024)
		gateway.state.update { $0.tamperedBlocks = [1] }
		var downloader = try makeDownloader(gateway)
		downloader.parallelRanges = 1
		await #expect(throws: RemoteError.self) { try await downloader.download(gateway.request) }
		let transfer = try #require(try downloader.store.transfers().first)
		let backgroundURL = downloader.storage.transferURL(transfer.transferID)
			.appending(path: BackgroundTransfers.downloadedFileName)
		try downloader.store.assignTask(
			transferID: transfer.transferID, sessionIdentifier: "s", taskIdentifier: 1,
			taskDescription: BackgroundTransfers.taskDescription(kind: .download, transferID: transfer.transferID),
			offset: Int64(128 * 1024))
		try gateway.content[(128 * 1024)...].write(to: backgroundURL)
		// The task still runs it: nothing is verified, and its file is left alone
		#expect(try downloader.finishBackgroundDownload(transferID: transfer.transferID) == nil)
		#expect(FileManager.default.fileExists(atPath: backgroundURL.path(percentEncoded: false)))

		try downloader.store.finishTask(transferID: transfer.transferID, state: .downloaded, error: nil)
		// Another worker has the transfer: the same
		#expect(try downloader.store.claimTransfer(id: transfer.transferID, owner: "other", duration: 60))
		#expect(try downloader.finishBackgroundDownload(transferID: transfer.transferID) == nil)
		#expect(try downloader.store.transfer(id: transfer.transferID)?.owner == "other")
		try downloader.store.releaseTransfer(id: transfer.transferID, owner: "other", state: .downloaded)

		let url = try #require(try downloader.finishBackgroundDownload(transferID: transfer.transferID))
		#expect(try Data(contentsOf: url) == gateway.content)
	}

	/// The foreground sends the credential, so it keeps no lease token in the Keychain
	@Test func foregroundDownloadsKeepNoLeaseToken() async throws {
		let gateway = ContentGateway(size: 128 * 1024)
		let credentials = InMemoryCredentialStore()
		let client = mockClient { gateway.handle($0) }
		let downloader = RemoteDownloader(
			store: try makeStore(), client: client, storage: RemoteStorage(baseURL: try makeTemporaryDirectory()),
			credentials: credentials, owner: "test")
		_ = try await downloader.download(gateway.request)
		#expect(credentials.tokenReferences.isEmpty)
		#expect(try downloader.store.releasedTokenReferences().isEmpty)
	}

	@Test func manifestPagesMustMakeProgress() async throws {
		let gateway = ContentGateway(size: 2 * 128 * 1024)
		let client = mockClient { request in
			guard request.path.hasSuffix("/manifest") else {
				return gateway.handle(request)
			}
			let from = Int(request.query("from") ?? "0") ?? 0
			return .json(ManifestPage(contentID: gateway.contentID, blockSize: gateway.blockSize, blocks: [], next: from))
		}
		let downloader = RemoteDownloader(
			store: try makeStore(), client: client, storage: RemoteStorage(baseURL: try makeTemporaryDirectory()),
			owner: "test")
		await #expect(throws: RemoteError.protocolViolation("a manifest page without blocks continues the manifest")) {
			try await downloader.download(gateway.request)
		}
	}

	@Test func manifestVerifierChecksCoverage() throws {
		let gateway = ContentGateway(size: 2 * 128 * 1024)
		var verifier = ManifestVerifier(contentID: gateway.contentID, size: Int64(gateway.content.count), blockCount: 2)
		let blocks = gateway.blocks.map {
			ManifestPage.Block(offset: Int64($0.offset), size: $0.size, hash: $0.hash.base64URLEncodedString())
		}
		// A gap between blocks
		var gapped = blocks
		gapped[1].offset += 1
		#expect(throws: RemoteError.self) {
			var copy = verifier
			_ = try copy.add(
				ManifestPage(contentID: gateway.contentID, blockSize: gateway.blockSize, blocks: gapped), from: 0)
		}
		_ = try verifier.add(
			ManifestPage(contentID: gateway.contentID, blockSize: gateway.blockSize, blocks: blocks), from: 0)
		try verifier.finish(identityHash: try #require(gateway.contentID.blocksHash))
	}
}
