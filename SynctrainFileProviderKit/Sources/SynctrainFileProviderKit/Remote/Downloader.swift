// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Darwin
import Foundation
import os

/// One block of a manifest
struct ManifestBlock: Sendable, Hashable {
	var index: Int
	var offset: Int64
	var size: Int
	var hash: Data

	/// Records in the manifest file: offset (8 bytes), size (4 bytes) and hash (32 bytes)
	static let recordSize = 44
}

/// A transfer's manifest on disk, read a page of records at a time
struct ManifestFile: Sendable {
	let url: URL

	/// Up to `count` blocks starting at the index
	func blocks(from index: Int, count: Int) throws(RemoteError) -> [ManifestBlock] {
		let file = try FileDescriptor(url, flags: O_RDONLY)
		let length = count * ManifestBlock.recordSize
		let buffer = UnsafeMutableRawBufferPointer.allocate(byteCount: max(length, 1), alignment: 8)
		defer { buffer.deallocate() }
		let read = try file.read(into: buffer, count: length, at: Int64(index * ManifestBlock.recordSize))
		var blocks: [ManifestBlock] = []
		blocks.reserveCapacity(read / ManifestBlock.recordSize)
		var position = 0
		while position + ManifestBlock.recordSize <= read {
			var offset: Int64 = 0
			for byte in 0..<8 {
				offset = (offset << 8) | Int64(buffer[position + byte])
			}
			var size = 0
			for byte in 8..<12 {
				size = (size << 8) | Int(buffer[position + byte])
			}
			let hash = Data(buffer[(position + 12)..<(position + ManifestBlock.recordSize)])
			blocks.append(
				ManifestBlock(
					index: index + position / ManifestBlock.recordSize, offset: offset, size: size, hash: hash))
			position += ManifestBlock.recordSize
		}
		return blocks
	}

	static func record(_ block: ManifestBlock) -> [UInt8] {
		var bytes: [UInt8] = []
		bytes.reserveCapacity(ManifestBlock.recordSize)
		for shift in stride(from: 56, through: 0, by: -8) {
			bytes.append(UInt8(truncatingIfNeeded: block.offset >> Int64(shift)))
		}
		for shift in stride(from: 24, through: 0, by: -8) {
			bytes.append(UInt8(truncatingIfNeeded: block.size >> shift))
		}
		bytes += block.hash
		return bytes
	}
}

/// Consecutive unverified blocks fetched with one request
struct BlockRange: Sendable, Hashable {
	var blocks: [ManifestBlock]

	var start: Int64 { blocks.first?.offset ?? 0 }
	var end: Int64 { blocks.last.map { $0.offset + Int64($0.size) } ?? 0 }
	var length: Int64 { end - start }
}

/// Downloads remote content into verified files. A read lease binds the item's revision to one content identity; the
/// manifest (fetched page by page and kept on disk) is checked against that identity; ranges aligned to blocks are
/// downloaded with file-backed tasks, every block is verified with SHA-256 before it is written to the partial file
/// and recorded durably, and the file is published (under its content identity) only once every block passed. An
/// interrupted transfer resumes from its verified blocks, with the original validator.
public struct RemoteDownloader: Sendable {
	public let store: IndexStore
	public let client: RemoteFileClient
	public let storage: RemoteStorage
	public let scheduler: TransferScheduler?
	/// Keeps lease tokens for background tasks (nil: background downloads are not possible)
	public let credentials: (any RemoteCredentialStore)?
	/// The worker the transfers are claimed for
	public let owner: String

	/// Bytes per range request (a single larger block makes its own range)
	var rangeTarget: Int64 = 8 << 20
	/// Ranges of one file downloaded at once
	var parallelRanges = 2
	/// Attempts per range before a failure is final
	var maximumAttempts = 3
	/// Blocks per manifest request (the gateway's default when nil)
	var manifestPageLimit: Int?
	/// How long a transfer claim lasts without renewal
	var claimDuration: TimeInterval = 120
	/// How long to wait before trying again to claim a transfer another worker has
	var claimPollInterval: Duration = .milliseconds(500)

	public static let maximumBlockSize = 16 << 20

	/// What a download throws when its claim on the transfer could not be renewed (another worker took it over)
	static let claimLost = RemoteError.unavailable(retryAfter: 5)

	public init(
		store: IndexStore, client: RemoteFileClient, storage: RemoteStorage, scheduler: TransferScheduler? = nil,
		credentials: (any RemoteCredentialStore)? = nil, owner: String
	) {
		self.store = store
		self.client = client
		self.storage = storage
		self.scheduler = scheduler
		self.credentials = credentials
		self.owner = owner
	}

	/// What to download: a gateway item at an exact revision
	public struct Request: Sendable, Hashable {
		public var itemID: Int64
		public var gatewayID: String
		public var revision: RemoteRevision
		/// The content identity the metadata names for the revision, if known
		public var contentID: ContentIdentity?

		public init(itemID: Int64, gatewayID: String, revision: RemoteRevision, contentID: ContentIdentity?) {
			self.itemID = itemID
			self.gatewayID = gatewayID
			self.revision = revision
			self.contentID = contentID
		}

		/// The current revision of a file whose remote metadata is stored
		public init?(_ state: RemoteItemState) {
			guard let itemID = state.itemID, state.kind == .file, !state.isDeleted else {
				return nil
			}
			self.init(itemID: itemID, gatewayID: state.gatewayID, revision: state.revision, contentID: state.contentID)
		}
	}

	private func stored<T>(_ body: () throws(IndexError) -> T) throws(RemoteError) -> T {
		do {
			return try body()
		}
		catch {
			throw .store(error)
		}
	}

	/// A verified local copy of the content, if there is one: downloaded before, or a staged edit the gateway committed
	/// with this identity
	public func cachedContent(_ contentID: ContentIdentity) throws(RemoteError) -> URL? {
		if let url = storage.cachedContentURL(contentID),
			FileManager.default.fileExists(atPath: url.path(percentEncoded: false))
		{
			storage.markContentUsed(url)
			return url
		}
		if let staged = try stored({ () throws(IndexError) in try store.stagedFile(contentID: contentID) }) {
			let url = storage.url(relativePath: staged)
			if FileManager.default.fileExists(atPath: url.path(percentEncoded: false)) {
				return url
			}
		}
		return nil
	}

	/// Downloads the content of the item's revision and returns the verified file (in `storage.contentURL`, named after
	/// its content identity; clone it, do not move it). `progress` receives the verified and total bytes.
	public func download(_ request: Request, progress: (@Sendable (Int64, Int64) -> Void)? = nil)
		async throws(RemoteError) -> URL
	{
		defer { removeReleasedTokens() }
		if let contentID = request.contentID, let cached = try cachedContent(contentID) {
			return cached
		}
		let tracker = LeaseTracker(client: client, request: request)
		let lease = try await tracker.start()
		// Every call claims the transfer for itself, so that two downloads of one item in this process do not run it
		// together: the second waits, and finds the content published by the first
		let worker = newWorker()
		var vanished = 0
		while true {
			if let cached = try cachedContent(lease.contentID) {
				return cached
			}
			let (transfer, obsolete) = try stored { () throws(IndexError) in
				try store.downloadTransfer(itemID: request.itemID, contentID: lease.contentID, totalSize: lease.size)
			}
			removeObsolete(obsolete)
			guard try await claim(transfer.transferID, worker: worker) else {
				// The transfer is gone (published by the worker that had it, or replaced): look again
				vanished += 1
				if vanished >= 3 {
					throw .unavailable(retryAfter: 5)
				}
				continue
			}
			if let cached = try cachedContent(lease.contentID) {
				// Published while this worker waited for the transfer
				try? store.removeTransfer(id: transfer.transferID)
				try? FileManager.default.removeItem(at: storage.transferURL(transfer.transferID))
				return cached
			}
			do throws(RemoteError) {
				return try await whileClaimed(transfer.transferID, worker: worker) { () async throws(RemoteError) -> URL in
					try await run(transfer.transferID, worker: worker, tracker: tracker, progress: progress)
				}
			}
			catch {
				try? store.releaseTransfer(id: transfer.transferID, owner: worker, error: "\(error)")
				throw error
			}
		}
	}

	/// A claim owner for one call: claims are exclusive, so concurrent calls in one process never share one
	private func newWorker() -> String {
		"\(owner)/\(UUID().uuidString.lowercased())"
	}

	private func removeObsolete(_ paths: [String]) {
		for path in paths {
			try? FileManager.default.removeItem(at: storage.url(relativePath: path))
		}
	}

	/// Removes the lease tokens no transfer needs anymore from the credential store
	private func removeReleasedTokens() {
		if let credentials {
			store.removeReleasedTokens(from: credentials)
		}
	}

	/// Claims the transfer for the worker, waiting a while for another worker that has it. False when the transfer is
	/// gone (published, or replaced by a transfer of other content). A transfer that a background task runs is not
	/// taken over: it is `.unavailable` until the task finished (or was canceled through `BackgroundTransfers`), since
	/// a transfer has one active task.
	private func claim(_ transferID: String, worker: String) async throws(RemoteError) -> Bool {
		// Long enough for the claim of a worker that stopped renewing it to expire
		let deadline = Date().addingTimeInterval(claimDuration + 5)
		while true {
			if try stored({ () throws(IndexError) in
				try store.claimTransfer(id: transferID, owner: worker, duration: claimDuration)
			}) {
				return true
			}
			guard let transfer = try stored({ () throws(IndexError) in try store.transfer(id: transferID) }) else {
				return false
			}
			if transfer.taskDescription != nil {
				throw .unavailable(retryAfter: 5)
			}
			if Date() > deadline {
				throw .overloaded(retryAfter: 5)
			}
			do {
				try await Task.sleep(for: claimPollInterval)
			}
			catch {
				throw .canceled
			}
		}
	}

	/// Extends the worker's claim (before each request made with it); `claimLost` when another worker has it now
	private func renewClaim(_ transferID: String, worker: String) throws(RemoteError) {
		guard
			try stored({ () throws(IndexError) in
				try store.renewTransferClaim(id: transferID, owner: worker, duration: claimDuration)
			})
		else {
			throw Self.claimLost
		}
	}

	/// Runs the body while renewing the worker's claim on the transfer. When the claim cannot be renewed (it expired
	/// while the process was suspended, and another worker took the transfer over), the body is canceled and
	/// `claimLost` is thrown.
	private func whileClaimed<T: Sendable>(
		_ transferID: String, worker: String, _ body: @escaping @Sendable () async throws(RemoteError) -> T
	) async throws(RemoteError) -> T {
		let store = store
		let duration = claimDuration
		do {
			return try await withThrowingTaskGroup(of: T?.self) { group in
				group.addTask {
					try await body()
				}
				group.addTask {
					while true {
						do {
							try await Task.sleep(for: .seconds(duration / 3))
						}
						catch {
							return nil
						}
						// A failed update is tried again; only a claim that is gone stops the download
						if let renewed = try? store.renewTransferClaim(id: transferID, owner: worker, duration: duration),
							!renewed
						{
							throw Self.claimLost
						}
					}
				}
				while let result = try await group.next() {
					if let value = result {
						group.cancelAll()
						return value
					}
				}
				throw RemoteError.canceled
			}
		}
		catch {
			throw RemoteFileClient.map(error, delegate: nil)
		}
	}

	private func run(
		_ transferID: String, worker: String, tracker: LeaseTracker, progress: (@Sendable (Int64, Int64) -> Void)?
	) async throws(RemoteError) -> URL {
		var lease = await tracker.current
		let directory = storage.transferURL(transferID)
		let partialURL = directory.appending(path: "content.partial")
		let manifest = ManifestFile(url: directory.appending(path: "manifest"))
		do {
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
			if !FileManager.default.fileExists(atPath: partialURL.path(percentEncoded: false)) {
				// The partial file is sparse: it takes space as blocks arrive, so a resumed transfer needs less
				if let available = FileIO.availableCapacity(at: directory), available < lease.size {
					throw RemoteError.fileSystem("not enough storage for \(lease.size) bytes")
				}
				guard FileManager.default.createFile(atPath: partialURL.path(percentEncoded: false), contents: nil)
				else {
					throw RemoteError.fileSystem("could not create the partial file")
				}
			}
		}
		catch let error as RemoteError {
			throw error
		}
		catch {
			throw .fileSystem("could not prepare the transfer: \(error.localizedDescription)")
		}
		try FileDescriptor(partialURL, flags: O_WRONLY).truncate(to: lease.size)
		// The foreground sends the credential: the lease's token is kept only for background tasks
		try recordLease(lease, transferID: transferID, tokenReference: nil)

		guard var transfer = try stored({ () throws(IndexError) in try store.transfer(id: transferID) }) else {
			throw .store(.notFound)
		}
		if !transfer.isManifestVerified {
			lease = try await fetchManifest(lease, into: manifest, tracker: tracker)
			let count = lease.blockCount
			try stored { () throws(IndexError) in try store.setManifestVerified(id: transferID, blockCount: count) }
			transfer.isManifestVerified = true
		}
		var verified = try stored { () throws(IndexError) in try store.verifiedBlocks(transferID: transferID) }

		if lease.size == 0 {
			try verifyEmpty(manifest, blockCount: lease.blockCount)
			return try publish(transferID, partialURL: partialURL, contentID: lease.contentID)
		}

		let queue = RangeQueue(
			manifest: manifest, blockCount: lease.blockCount, verified: verified, target: rangeTarget)
		let total = lease.size
		let verifiedBytes = VerifiedBytes(initial: try verifiedByteCount(manifest, verified))
		progress?(verifiedBytes.value, total)

		do {
			try await withThrowingTaskGroup(of: Void.self) { group in
				for _ in 0..<max(1, parallelRanges) {
					group.addTask {
						while let range = try await queue.next() {
							try await fetch(
								range, transferID: transferID, worker: worker, directory: directory,
								partialURL: partialURL, tracker: tracker
							) { bytes in
								progress?(verifiedBytes.add(bytes), total)
							}
						}
					}
				}
				try await group.waitForAll()
			}
		}
		catch {
			throw RemoteFileClient.map(error, delegate: nil)
		}

		verified = try stored { () throws(IndexError) in try store.verifiedBlocks(transferID: transferID) }
		guard verified.isComplete else {
			throw .protocolViolation("blocks are missing after the transfer")
		}
		return try publish(transferID, partialURL: partialURL, contentID: lease.contentID)
	}

	/// Records the lease the transfer reads from, and the reference of its token when one is kept (a token the
	/// transfer referred to before is released)
	private func recordLease(_ lease: ReadLease, transferID: String, tokenReference: String?) throws(RemoteError) {
		let leaseID = lease.lease
		try stored { () throws(IndexError) in
			try store.setTransferLease(id: transferID, leaseID: leaseID, tokenReference: tokenReference)
		}
	}

	/// Keeps the lease's token for a background task; nil when it could not be kept
	private func keepToken(_ lease: ReadLease, in credentials: any RemoteCredentialStore) -> String? {
		let reference = "lease:\(lease.lease)"
		do {
			try credentials.setToken(lease.token, reference: reference)
			return reference
		}
		catch {
			return nil
		}
	}

	private func verifiedByteCount(_ manifest: ManifestFile, _ verified: BlockBitmap) throws(RemoteError) -> Int64 {
		guard verified.verifiedCount > 0 else {
			return 0
		}
		var total: Int64 = 0
		var index = 0
		while index < verified.count {
			let blocks = try manifest.blocks(from: index, count: 1024)
			if blocks.isEmpty {
				break
			}
			for block in blocks where verified.contains(block.index) {
				total += Int64(block.size)
			}
			index += blocks.count
		}
		return total
	}

	// MARK: - Manifest

	/// Fetches the manifest page by page into the file, checking that the blocks cover the content exactly and that
	/// the SHA-256 of their hashes is the content identity's BlocksHash
	private func fetchManifest(_ lease: ReadLease, into manifest: ManifestFile, tracker: LeaseTracker)
		async throws(RemoteError) -> ReadLease
	{
		guard let identityHash = lease.contentID.blocksHash, lease.contentID.size == lease.size else {
			throw .protocolViolation("the lease's content identity \(lease.contentID) does not describe its content")
		}
		var lease = lease
		var verifier = ManifestVerifier(contentID: lease.contentID, size: lease.size, blockCount: lease.blockCount)
		guard FileManager.default.createFile(atPath: manifest.url.path(percentEncoded: false), contents: nil) else {
			throw .fileSystem("could not create the manifest file")
		}
		let file = try FileDescriptor(manifest.url, flags: O_WRONLY)
		var from = 0
		while true {
			let page: ManifestPage
			do {
				page = try await client.manifestPage(leaseID: lease.lease, from: from, limit: manifestPageLimit)
			}
			catch .leaseExpired {
				lease = try await tracker.renew()
				continue
			}
			let records = try verifier.add(page, from: from)
			if !records.isEmpty {
				try file.write(records, at: Int64(from * ManifestBlock.recordSize))
			}
			from += page.blocks.count
			guard let next = page.next else {
				break
			}
			// A page that continues the manifest must make progress, or a misbehaving gateway keeps this looping
			guard !page.blocks.isEmpty else {
				throw .protocolViolation("a manifest page without blocks continues the manifest")
			}
			guard next == from else {
				throw .protocolViolation("the manifest's next page does not follow its last block")
			}
		}
		try verifier.finish(identityHash: identityHash)
		try file.sync()
		return lease
	}

	private func verifyEmpty(_ manifest: ManifestFile, blockCount: Int) throws(RemoteError) {
		let empty = Data(SHA256.hash(data: Data()))
		for block in try manifest.blocks(from: 0, count: blockCount) where block.size != 0 || block.hash != empty {
			throw .protocolViolation("the empty file's block does not match")
		}
	}

	// MARK: - Ranges

	/// Downloads a range (again, while attempts remain), verifies its blocks and writes them to the partial file
	private func fetch(
		_ range: BlockRange, transferID: String, worker: String, directory: URL, partialURL: URL,
		tracker: LeaseTracker, verifiedBytes: @Sendable (Int64) -> Void
	) async throws(RemoteError) {
		var pending = range.blocks
		var attempts = 0
		let rangeURL = directory.appending(path: "range-\(range.blocks[0].index)")
		defer { try? FileManager.default.removeItem(at: rangeURL) }
		while !pending.isEmpty {
			attempts += 1
			let current = BlockRange(blocks: pending)
			let lease = try await tracker.ensureReady(through: current.end)
			// Nothing is requested (or written) for a transfer another worker has taken over
			try renewClaim(transferID, worker: worker)
			guard let transfer = try stored({ () throws(IndexError) in try store.transfer(id: transferID) }) else {
				throw .store(.notFound)
			}
			let response: ContentResponse
			do {
				response = try await withContentSlot(transferID) { () async throws(RemoteError) -> ContentResponse in
					try await client.downloadContent(
						leaseID: lease.lease, range: current.start...(current.end - 1), ifRange: transfer.entityTag,
						to: rangeURL)
				}
			}
			catch .leaseExpired {
				_ = try await tracker.renew()
				continue
			}
			try checkValidator(response, transfer: transfer, lease: lease)

			let result = try Self.verifyAndWrite(
				current.blocks, from: rangeURL, startingAt: current.start, into: partialURL)
			if !result.verified.isEmpty {
				let indexes = result.verified.map(\.index)
				_ = try stored { () throws(IndexError) in
					try store.recordVerifiedBlocks(transferID: transferID, blocks: indexes)
				}
				verifiedBytes(result.verified.reduce(0) { $0 + Int64($1.size) })
			}
			let verifiedSet = Set(result.verified.map(\.index))
			pending = pending.filter { !verifiedSet.contains($0.index) }
			if pending.isEmpty {
				return
			}
			if attempts >= maximumAttempts {
				if result.failedBlock != nil {
					throw .protocolViolation("block \(pending[0].index) failed verification \(attempts) times")
				}
				throw .unavailable(retryAfter: nil)
			}
		}
	}

	private func withContentSlot<T: Sendable>(_ transferID: String, _ body: () async throws(RemoteError) -> T)
		async throws(RemoteError) -> T
	{
		guard let scheduler else {
			return try await body()
		}
		return try await scheduler.withSlot(.content, transferID: transferID, body)
	}

	/// The response must carry the transfer's strong validator (the first response's, or the content identity's)
	private func checkValidator(_ response: ContentResponse, transfer: TransferRecord, lease: ReadLease)
		throws(RemoteError)
	{
		guard let entityTag = response.entityTag, !entityTag.hasPrefix("W/") else {
			throw .protocolViolation("the content has no strong validator")
		}
		let expected = transfer.entityTag ?? lease.contentID.entityTag
		guard entityTag == expected else {
			// Another representation: what was verified cannot be combined with it
			try? store.resetTransferProgress(id: transfer.transferID)
			throw .protocolViolation("the content's validator changed")
		}
		if transfer.entityTag == nil {
			try stored { () throws(IndexError) in
				try store.setTransferEntityTag(id: transfer.transferID, entityTag: entityTag)
			}
		}
		if let complete = response.completeLength, complete != lease.size {
			throw .protocolViolation("the content's length is \(complete), not \(lease.size)")
		}
	}

	/// Verifies downloaded blocks against their hashes and writes the ones that pass into the partial file, streaming
	/// with a bounded buffer. Each block is hashed from the downloaded file first and copied only once it passed, so
	/// the partial file never holds a byte that was not verified. `fileStart` is the content offset of the downloaded
	/// file's first byte. A short file verifies the blocks it holds completely. Stops at the first block that fails.
	static func verifyAndWrite(_ blocks: [ManifestBlock], from url: URL, startingAt fileStart: Int64, into partialURL: URL)
		throws(RemoteError) -> (verified: [ManifestBlock], failedBlock: Int?)
	{
		let input = try FileDescriptor(url, flags: O_RDONLY)
		let output = try FileDescriptor(partialURL, flags: O_WRONLY)
		let available = input.size
		let buffer = UnsafeMutableRawBufferPointer.allocate(byteCount: FileIO.bufferSize, alignment: 16)
		defer { buffer.deallocate() }
		var verified: [ManifestBlock] = []
		var failed: Int?
		for block in blocks {
			let start = block.offset - fileStart
			let size = Int64(block.size)
			guard start >= 0 else {
				continue
			}
			guard start + size <= available else {
				break
			}
			var hasher = SHA256()
			var done: Int64 = 0
			while done < size {
				let count = try input.read(
					into: buffer, count: Int(min(Int64(FileIO.bufferSize), size - done)), at: start + done)
				if count == 0 {
					break
				}
				hasher.update(bufferPointer: UnsafeRawBufferPointer(rebasing: buffer[0..<count]))
				done += Int64(count)
			}
			guard done == size, Data(hasher.finalize()) == block.hash else {
				failed = block.index
				break
			}
			done = 0
			while done < size {
				let count = try input.read(
					into: buffer, count: Int(min(Int64(FileIO.bufferSize), size - done)), at: start + done)
				guard count > 0 else {
					throw .fileSystem("the downloaded range changed while it was written")
				}
				try output.write(UnsafeRawBufferPointer(rebasing: buffer[0..<count]), at: block.offset + done)
				done += Int64(count)
			}
			verified.append(block)
		}
		if !verified.isEmpty {
			try output.sync()
		}
		return (verified, failed)
	}

	/// Moves the complete partial file to its place in the content directory and forgets the transfer
	private func publish(_ transferID: String, partialURL: URL, contentID: ContentIdentity) throws(RemoteError) -> URL {
		guard let destination = storage.cachedContentURL(contentID) else {
			throw .protocolViolation("the content identity \(contentID) cannot name a file")
		}
		do {
			try FileManager.default.createDirectory(at: storage.contentURL, withIntermediateDirectories: true)
			if rename(partialURL.path(percentEncoded: false), destination.path(percentEncoded: false)) != 0 {
				throw RemoteError.fileSystem("could not publish the file: \(String(cString: strerror(errno)))")
			}
		}
		catch let error as RemoteError {
			throw error
		}
		catch {
			throw .fileSystem("could not publish the file: \(error.localizedDescription)")
		}
		// Releases the lease token the transfer kept, if any (removed by `removeReleasedTokens`)
		try? store.removeTransfer(id: transferID)
		try? FileManager.default.removeItem(at: storage.transferURL(transferID))
		return destination
	}

	// MARK: - Background

	/// Starts the download of what is missing of the item's content as one task of the background session (which the
	/// system may run while this process is suspended). The task carries the lease's scoped token, never the client
	/// credential. Returns the transfer ID; `finishBackgroundDownload` verifies the bytes once the task completed.
	public func startBackgroundDownload(_ request: Request, using transfers: BackgroundTransfers)
		async throws(RemoteError) -> String
	{
		guard let credentials else {
			throw .store(.notAllowed("background downloads need a credential store for the lease token"))
		}
		defer { removeReleasedTokens() }
		let tracker = LeaseTracker(client: client, request: request)
		let lease = try await tracker.start()
		let (created, obsolete) = try stored { () throws(IndexError) in
			try store.downloadTransfer(itemID: request.itemID, contentID: lease.contentID, totalSize: lease.size)
		}
		removeObsolete(obsolete)
		let transferID = created.transferID
		let worker = newWorker()
		guard try await claim(transferID, worker: worker) else {
			// Published (or replaced) while this call waited for it
			throw .unavailable(retryAfter: 5)
		}
		do throws(RemoteError) {
			let prepared = try await whileClaimed(transferID, worker: worker) {
				() async throws(RemoteError) -> (lease: ReadLease, start: Int64) in
				try await prepareBackgroundDownload(transferID, lease: lease, tracker: tracker)
			}
			guard prepared.start < prepared.lease.size else {
				// Every block is verified already: `finishBackgroundDownload` publishes the file
				try stored { () throws(IndexError) in
					try store.releaseTransfer(id: transferID, owner: worker, state: .downloaded)
				}
				return transferID
			}
			let reference = keepToken(prepared.lease, in: credentials)
			try recordLease(prepared.lease, transferID: transferID, tokenReference: reference)
			let current = try stored { () throws(IndexError) in try store.transfer(id: transferID) }
			let urlRequest = try client.contentRequest(
				leaseID: prepared.lease.lease, range: nil, openEnded: prepared.start,
				ifRange: current?.entityTag ?? prepared.lease.contentID.entityTag, token: prepared.lease.token)
			// The task takes the claim over in the update that records it, so no other worker starts in between
			try transfers.startDownload(
				transferID: transferID, request: urlRequest, offset: prepared.start, claimedBy: worker)
			return transferID
		}
		catch {
			try? store.releaseTransfer(id: transferID, owner: worker, error: "\(error)")
			throw error
		}
	}

	/// Prepares the partial file and the manifest of a claimed transfer for a background task; returns the lease and
	/// where the first block that is not verified starts (the size when every block is)
	private func prepareBackgroundDownload(_ transferID: String, lease: ReadLease, tracker: LeaseTracker)
		async throws(RemoteError) -> (lease: ReadLease, start: Int64)
	{
		var lease = lease
		let directory = storage.transferURL(transferID)
		let partialURL = directory.appending(path: "content.partial")
		let manifest = ManifestFile(url: directory.appending(path: "manifest"))
		do {
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
			if !FileManager.default.fileExists(atPath: partialURL.path(percentEncoded: false)) {
				FileManager.default.createFile(atPath: partialURL.path(percentEncoded: false), contents: nil)
			}
		}
		catch {
			throw .fileSystem("could not prepare the transfer: \(error.localizedDescription)")
		}
		try FileDescriptor(partialURL, flags: O_WRONLY).truncate(to: lease.size)
		guard let transfer = try stored({ () throws(IndexError) in try store.transfer(id: transferID) }) else {
			throw .store(.notFound)
		}
		if !transfer.isManifestVerified {
			lease = try await fetchManifest(lease, into: manifest, tracker: tracker)
			let count = lease.blockCount
			try stored { () throws(IndexError) in
				try store.setManifestVerified(id: transferID, blockCount: count)
			}
		}
		let verified = try stored { () throws(IndexError) in try store.verifiedBlocks(transferID: transferID) }
		var index = 0
		while index < lease.blockCount {
			let blocks = try manifest.blocks(from: index, count: 1024)
			if blocks.isEmpty {
				break
			}
			if let missing = blocks.first(where: { !verified.contains($0.index) }) {
				return (lease, missing.offset)
			}
			index += blocks.count
		}
		return (lease, lease.size)
	}

	/// Verifies the bytes a background task downloaded (from its offset to the end), keeps the blocks that pass, and
	/// publishes the file when every block passed. Returns the file, or nil when blocks are still missing (download
	/// them with `download`), or when another worker has the transfer (or a task still runs it).
	public func finishBackgroundDownload(transferID: String) throws(RemoteError) -> URL? {
		defer { removeReleasedTokens() }
		guard let transfer = try stored({ () throws(IndexError) in try store.transfer(id: transferID) }),
			let contentID = transfer.contentID, transfer.isManifestVerified
		else {
			return nil
		}
		// One worker verifies the bytes, and only once the task is detached from the transfer
		let worker = newWorker()
		guard
			try stored({ () throws(IndexError) in
				try store.claimTransfer(id: transferID, owner: worker, duration: claimDuration)
			})
		else {
			return nil
		}
		let directory = storage.transferURL(transferID)
		let partialURL = directory.appending(path: "content.partial")
		let backgroundURL = directory.appending(path: BackgroundTransfers.downloadedFileName)
		defer { try? FileManager.default.removeItem(at: backgroundURL) }
		do throws(RemoteError) {
			let verified = try verifyBackgroundBytes(transfer, from: backgroundURL, into: partialURL)
			guard verified.isComplete else {
				try? store.releaseTransfer(id: transferID, owner: worker, state: .interrupted)
				return nil
			}
			return try publish(transferID, partialURL: partialURL, contentID: contentID)
		}
		catch {
			try? store.releaseTransfer(id: transferID, owner: worker, error: "\(error)")
			throw error
		}
	}

	/// Verifies the blocks of the background task's file (the transfer as it was before it was claimed) and records
	/// the ones that pass; returns the verified blocks
	private func verifyBackgroundBytes(_ transfer: TransferRecord, from backgroundURL: URL, into partialURL: URL)
		throws(RemoteError) -> BlockBitmap
	{
		let transferID = transfer.transferID
		var verified = try stored { () throws(IndexError) in try store.verifiedBlocks(transferID: transferID) }
		guard transfer.state == .downloaded, let offset = transfer.taskOffset,
			FileManager.default.fileExists(atPath: backgroundURL.path(percentEncoded: false))
		else {
			return verified
		}
		let manifest = ManifestFile(url: storage.transferURL(transferID).appending(path: "manifest"))
		var index = 0
		var blocks: [ManifestBlock] = []
		while index < transfer.blockCount {
			let page = try manifest.blocks(from: index, count: 1024)
			if page.isEmpty {
				break
			}
			blocks += page.filter { $0.offset >= offset && !verified.contains($0.index) }
			index += page.count
			// Verify a bounded number of blocks at a time
			if blocks.count >= 1024 || index >= transfer.blockCount {
				let result = try Self.verifyAndWrite(blocks, from: backgroundURL, startingAt: offset, into: partialURL)
				if !result.verified.isEmpty {
					let indexes = result.verified.map { $0.index }
					verified = try stored { () throws(IndexError) in
						try store.recordVerifiedBlocks(transferID: transferID, blocks: indexes)
					}
				}
				blocks = []
				if result.failedBlock != nil {
					break
				}
			}
		}
		return verified
	}
}

/// Checks a manifest page by page: blocks are contiguous from offset 0, sized within limits, and the SHA-256 of their
/// hashes equals the content identity's BlocksHash
struct ManifestVerifier {
	let contentID: ContentIdentity
	let size: Int64
	let blockCount: Int
	private var hasher = SHA256()
	private var nextOffset: Int64 = 0
	private var count = 0

	init(contentID: ContentIdentity, size: Int64, blockCount: Int) {
		self.contentID = contentID
		self.size = size
		self.blockCount = blockCount
	}

	/// Checks a page and returns its records for the manifest file
	mutating func add(_ page: ManifestPage, from: Int) throws(RemoteError) -> [UInt8] {
		guard page.contentID == contentID else {
			throw .protocolViolation("a manifest page is for another content identity")
		}
		guard from == count else {
			throw .protocolViolation("manifest pages out of order")
		}
		var records: [UInt8] = []
		records.reserveCapacity(page.blocks.count * ManifestBlock.recordSize)
		for block in page.blocks {
			guard let hash = Data(base64URLEncoded: block.hash), hash.count == 32 else {
				throw .protocolViolation("a block hash is not a SHA-256")
			}
			guard block.offset == nextOffset, block.size >= 0, block.size <= RemoteDownloader.maximumBlockSize,
				block.size > 0 || size == 0
			else {
				throw .protocolViolation("block \(count) does not continue the content")
			}
			hasher.update(data: hash)
			records += ManifestFile.record(
				ManifestBlock(index: count, offset: block.offset, size: block.size, hash: hash))
			nextOffset += Int64(block.size)
			count += 1
			guard count <= blockCount, nextOffset <= size else {
				throw .protocolViolation("the manifest describes more than the content")
			}
		}
		return records
	}

	/// Checks that the blocks cover the content exactly and match the content identity
	func finish(identityHash: Data) throws(RemoteError) {
		guard count == blockCount, nextOffset == size else {
			throw .protocolViolation("the manifest does not cover the content")
		}
		guard Data(hasher.finalize()) == identityHash else {
			throw .protocolViolation("the manifest does not match the content identity")
		}
	}
}

/// Hands out the ranges of a transfer's unverified blocks, reading the manifest a page at a time
actor RangeQueue {
	private let manifest: ManifestFile
	private let blockCount: Int
	private let verified: BlockBitmap
	private let target: Int64
	private var nextIndex = 0
	private var buffered: [ManifestBlock] = []

	init(manifest: ManifestFile, blockCount: Int, verified: BlockBitmap, target: Int64) {
		self.manifest = manifest
		self.blockCount = blockCount
		self.verified = verified
		self.target = target
	}

	private func nextBlock() throws(RemoteError) -> ManifestBlock? {
		if buffered.isEmpty {
			guard nextIndex < blockCount else {
				return nil
			}
			buffered = try manifest.blocks(from: nextIndex, count: 256)
			nextIndex += buffered.count
			if buffered.isEmpty {
				return nil
			}
			buffered.reverse()
		}
		return buffered.popLast()
	}

	private func pushBack(_ block: ManifestBlock) {
		buffered.append(block)
	}

	/// The next range of consecutive unverified blocks, about `target` bytes (one block when a block is larger)
	func next() throws(RemoteError) -> BlockRange? {
		var blocks: [ManifestBlock] = []
		var length: Int64 = 0
		while let block = try nextBlock() {
			if verified.contains(block.index) {
				if blocks.isEmpty {
					continue
				}
				break
			}
			if !blocks.isEmpty && length + Int64(block.size) > target {
				pushBack(block)
				break
			}
			blocks.append(block)
			length += Int64(block.size)
		}
		return blocks.isEmpty ? nil : BlockRange(blocks: blocks)
	}
}

/// The lease of a transfer: renewed when it expires, and asked again while the gateway prepares the content
actor LeaseTracker {
	private let client: RemoteFileClient
	private let request: RemoteDownloader.Request
	private var lease: ReadLease?

	init(client: RemoteFileClient, request: RemoteDownloader.Request) {
		self.client = client
		self.request = request
	}

	var current: ReadLease {
		get async {
			lease!
		}
	}

	private func check(_ new: ReadLease) throws(RemoteError) -> ReadLease {
		guard new.contentID.isValid, new.contentID.size == new.size else {
			throw .protocolViolation("the lease's content identity \(new.contentID) does not describe its content")
		}
		if let expected = request.contentID, expected != new.contentID {
			throw .protocolViolation("the lease is for another content identity than the item's revision")
		}
		if let lease, lease.contentID != new.contentID {
			throw .protocolViolation("the renewed lease is for other content")
		}
		if new.preparation.state == .failed {
			throw .unavailable(retryAfter: new.preparation.retryAfter.map(TimeInterval.init))
		}
		lease = new
		return new
	}

	func start() async throws(RemoteError) -> ReadLease {
		try check(try await client.createReadLease(itemID: request.gatewayID, revision: request.revision))
	}

	/// A new lease of the same content (after `lease_expired`)
	func renew() async throws(RemoteError) -> ReadLease {
		try check(try await client.createReadLease(itemID: request.gatewayID, revision: request.revision))
	}

	/// Waits until the gateway has prepared the content up to the offset, asking for progress as it advises
	func ensureReady(through end: Int64) async throws(RemoteError) -> ReadLease {
		guard var current = lease else {
			return try await start()
		}
		while current.preparation.state != .ready && current.preparation.readyBytes < end {
			let delay = Double(current.preparation.retryAfter ?? 1)
			do {
				try await Task.sleep(for: .seconds(min(max(delay, 0.05), 30)))
			}
			catch {
				throw .canceled
			}
			current = try await renew()
		}
		return current
	}
}

/// Verified bytes of a transfer, for progress
final class VerifiedBytes: Sendable {
	private let storage: OSAllocatedUnfairLock<Int64>

	init(initial: Int64) {
		storage = OSAllocatedUnfairLock(initialState: initial)
	}

	var value: Int64 { storage.withLock { $0 } }

	func add(_ bytes: Int64) -> Int64 {
		storage.withLock {
			$0 += bytes
			return $0
		}
	}
}
