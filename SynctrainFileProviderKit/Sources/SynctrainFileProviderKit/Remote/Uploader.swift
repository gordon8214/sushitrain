// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Darwin
import Foundation

/// Stages edited files immutably and uploads them with tus 1.0. An upload only stages bytes on the gateway; the
/// operation that references it changes the live file. Uploads resume from the offset the gateway confirms; in the
/// foreground every chunk is streamed from a file and carries its checksum, so an interrupted chunk appends nothing.
public struct RemoteUploader: Sendable {
	public let store: IndexStore
	public let client: RemoteFileClient
	public let storage: RemoteStorage
	public let scheduler: TransferScheduler?
	/// Keeps upload tokens for background tasks (nil: background uploads are not possible)
	public let credentials: (any RemoteCredentialStore)?

	/// Bytes per foreground request
	var chunkSize: Int64 = 8 << 20
	/// Attempts per chunk before a failure is final
	var maximumAttempts = 3
	/// How long a claim is extended before each request
	var claimDuration: TimeInterval = 180

	public init(
		store: IndexStore, client: RemoteFileClient, storage: RemoteStorage, scheduler: TransferScheduler? = nil,
		credentials: (any RemoteCredentialStore)? = nil
	) {
		self.store = store
		self.client = client
		self.storage = storage
		self.scheduler = scheduler
		self.credentials = credentials
	}

	// MARK: - Staging

	/// Stages an immutable copy of the file (an APFS clone where possible, so it takes no space until either copy
	/// changes) below `storage.stagingURL`, and computes its SHA-256 while reading it once with a bounded buffer
	public static func stage(_ source: URL, in storage: RemoteStorage, modifiedNs: Int64? = nil) throws(RemoteError)
		-> StagedFile
	{
		let name = UUID().uuidString.lowercased()
		let destination = storage.stagingURL.appending(path: name, directoryHint: .notDirectory)
		do {
			try FileManager.default.createDirectory(at: storage.stagingURL, withIntermediateDirectories: true)
		}
		catch {
			throw .fileSystem("could not create the staging directory: \(error.localizedDescription)")
		}
		// COPYFILE_CLONE clones when the volume can, and copies otherwise
		let result = copyfile(
			source.path(percentEncoded: false), destination.path(percentEncoded: false), nil,
			copyfile_flags_t(COPYFILE_CLONE))
		guard result == 0 else {
			throw .fileSystem("could not stage \(source.lastPathComponent): \(String(cString: strerror(errno)))")
		}
		do {
			var attributes: [FileAttributeKey: Any] = [.posixPermissions: 0o444]
			#if os(iOS)
				attributes[.protectionKey] = FileProtectionType.completeUntilFirstUserAuthentication
			#endif
			try FileManager.default.setAttributes(attributes, ofItemAtPath: destination.path(percentEncoded: false))
		}
		catch {
			try? FileManager.default.removeItem(at: destination)
			throw .fileSystem("could not protect the staged file: \(error.localizedDescription)")
		}
		let digest: String
		let size: Int64
		do {
			digest = Data(try FileIO.sha256(of: destination)).base64URLEncodedString()
			size = try FileDescriptor(destination, flags: O_RDONLY).size
		}
		catch {
			try? FileManager.default.removeItem(at: destination)
			throw error
		}
		return StagedFile(
			relativePath: "\(RemoteStorage.stagingDirectory)/\(name)", size: size, digest: digest,
			modifiedNs: modifiedNs)
	}

	/// Removes staged files that nothing needs anymore
	public static func removeReleasedStagedFiles(_ paths: [String], store: IndexStore, storage: RemoteStorage) {
		for path in paths {
			let url = storage.url(relativePath: path)
			// Staged files are read-only. A partial download is a directory, which this would keep from being entered.
			if (try? url.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory != true {
				try? FileManager.default.setAttributes(
					[.posixPermissions: 0o644], ofItemAtPath: url.path(percentEncoded: false))
			}
			try? FileManager.default.removeItem(at: url)
			try? store.forgetStagedFile(path: path)
		}
	}

	// MARK: - Foreground

	private func stored<T>(_ body: () throws(IndexError) -> T?) throws(RemoteError) -> T {
		let result: T?
		do {
			result = try body()
		}
		catch {
			throw .store(error)
		}
		guard let result else {
			throw Self.claimLost
		}
		return result
	}

	/// What updates with a claim that is no longer current throw
	public static let claimLost = RemoteError.store(.notAllowed("the claim on the operation was lost"))

	private func withUploadSlot<T: Sendable>(_ body: () async throws(RemoteError) -> T) async throws(RemoteError) -> T {
		guard let scheduler else {
			return try await body()
		}
		return try await scheduler.withSlot(.upload, body)
	}

	/// What a foreground upload ended with
	public enum UploadResult: Sendable {
		/// The gateway holds every byte (state `uploaded`)
		case uploaded(OutboxClaim)
		/// The rest goes on in a task of the background session (`transferID`); the claim is still held, to be released
		case inBackground(OutboxClaim, transferID: String)
	}

	/// Uploads the staged file of a claimed operation, resuming from the offset the gateway confirms, and returns the
	/// claim once the gateway holds every byte (state `uploaded`). An upload that expired starts again from the staged
	/// file, as long as the operation was never sent.
	public func upload(_ claim: OutboxClaim, remoteFolderID: String) async throws(RemoteError) -> OutboxClaim {
		switch try await upload(claim, remoteFolderID: remoteFolderID, handoff: nil) {
		case .uploaded(let uploaded):
			return uploaded
		case .inBackground:
			throw .protocolViolation("an upload without a handoff went to the background")
		}
	}

	/// Uploads the staged file of a claimed operation like `upload(_:remoteFolderID:)`, and hands the rest to the
	/// handoff's background session when it says so (`.inBackground`)
	public func upload(_ claim: OutboxClaim, remoteFolderID: String, handoff: BackgroundHandoff?)
		async throws(RemoteError) -> UploadResult
	{
		var claim = claim
		guard let stagedPath = claim.record.stagedPath, let length = claim.record.uploadLength,
			let digest = claim.record.uploadDigest
		else {
			throw .store(.notAllowed("the operation has no staged file"))
		}
		let file = storage.url(relativePath: stagedPath)
		let start = Date()
		var restarted = false
		while true {
			if claim.record.uploadID == nil {
				claim = try stored { () throws(IndexError) in try store.renewClaim(claim, duration: claimDuration) }
				let upload = try await withUploadSlot { () async throws(RemoteError) -> UploadInfo in
					try await client.createUpload(folderID: remoteFolderID, length: length, digest: digest)
				}
				var reference: String?
				if let credentials {
					let name = "upload:\(upload.upload)"
					if (try? credentials.setToken(upload.token, reference: name)) != nil {
						reference = name
					}
				}
				let created = claim
				do throws(RemoteError) {
					claim = try stored { () throws(IndexError) in
						try store.recordUploadCreated(created, upload: upload, tokenReference: reference)
					}
				}
				catch {
					// Nothing refers to the token: it is not kept
					if let reference {
						try? credentials?.removeToken(reference: reference)
					}
					throw error
				}
			}
			guard let uploadID = claim.record.uploadID else {
				continue
			}
			do {
				return try await send(
					claim, uploadID: uploadID, file: file, length: length, handoff: handoff, uploadingSince: start)
			}
			catch .notFound where !restarted && claim.record.state.isUnsent {
				// Expired after seven days without activity: start again from the staged file
				restarted = true
				let current = claim
				claim = try stored { () throws(IndexError) in try store.restartUpload(current) }
			}
		}
	}

	private func send(
		_ claim: OutboxClaim, uploadID: String, file: URL, length: Int64, handoff: BackgroundHandoff?,
		uploadingSince start: Date
	) async throws(RemoteError) -> UploadResult {
		var claim = claim
		var offset = try await withUploadSlot { () async throws(RemoteError) -> Int64 in
			try await client.uploadStatus(uploadID: uploadID).offset
		}
		if offset != claim.record.uploadOffset || offset >= length {
			let current = claim
			let confirmed = offset
			claim = try stored { () throws(IndexError) in try store.recordUploadOffset(current, offset: confirmed) }
		}
		let chunkURL = storage.transfersURL.appending(path: "upload-\(claim.operationID).chunk")
		defer { try? FileManager.default.removeItem(at: chunkURL) }
		do {
			try FileManager.default.createDirectory(at: storage.transfersURL, withIntermediateDirectories: true)
		}
		catch {
			throw .fileSystem("could not create the transfers directory: \(error.localizedDescription)")
		}
		var attempts = 0
		/// Once a handoff failed (no space for the rest, a task runs it already), the rest is sent here
		var mayHandOver = handoff != nil
		while offset < length {
			if let handoff, mayHandOver, handoff.shouldHandOver(remaining: length - offset, uploadingSince: start) {
				do throws(RemoteError) {
					switch try await handOver(claim, knownOffset: offset, length: length, to: handoff) {
					case .started(let handedOver, let transferID):
						return .inBackground(handedOver, transferID: transferID)
					case .foreground(let current, let confirmed):
						// Too little is left for a task: it is sent here
						claim = current
						offset = confirmed
						mayHandOver = false
						continue
					}
				}
				catch RemoteUploader.claimLost {
					throw RemoteUploader.claimLost
				}
				catch {
					mayHandOver = false
					if Task.isCancelled {
						throw .canceled
					}
				}
			}
			let current = claim
			claim = try stored { () throws(IndexError) in try store.renewClaim(current, duration: claimDuration) }
			let count = min(chunkSize, length - offset)
			// The chunk is copied from the staged file with a bounded buffer and uploaded from its file
			let checksum = try FileIO.copy(from: file, offset: offset, length: count, to: chunkURL)
			// A handoff interrupts the chunk while the rest from it is worth a task (it is then handed over right away)
			let interruptible = mayHandOver && length - offset >= (handoff?.minimumRemainder ?? .max) ? handoff : nil
			do throws(RemoteError) {
				let chunkStart = offset
				let client = client
				let confirmed = try await withUploadSlot { () async throws(RemoteError) -> Int64 in
					guard let interruptible else {
						return try await client.appendUpload(
							uploadID: uploadID, offset: chunkStart, file: chunkURL, checksum: checksum)
					}
					return try await interruptible.interruptible { () async throws(RemoteError) -> Int64 in
						try await client.appendUpload(
							uploadID: uploadID, offset: chunkStart, file: chunkURL, checksum: checksum)
					}
				}
				// With a checksum, a chunk is appended whole or not at all; anything else (no progress in particular)
				// would make this loop forever
				guard confirmed == chunkStart + count else {
					throw .protocolViolation(
						"the gateway confirmed offset \(confirmed) after \(count) bytes at \(chunkStart)")
				}
				offset = confirmed
				attempts = 0
			}
			catch .canceled where interruptible?.isRequested == true {
				// Interrupted for the handoff, which asks the gateway where the upload is
				continue
			}
			catch .server(let status, _) where status == 409 || status == 460 {
				// Another offset (the other process sent bytes too) or a damaged chunk: ask where the upload is
				attempts += 1
				if attempts >= maximumAttempts {
					throw .server(status: status, code: nil)
				}
				offset = try await withUploadSlot { () async throws(RemoteError) -> Int64 in
					try await client.uploadStatus(uploadID: uploadID).offset
				}
			}
			let recorded = claim
			let confirmed = offset
			claim = try stored { () throws(IndexError) in try store.recordUploadOffset(recorded, offset: confirmed) }
		}
		return .uploaded(claim)
	}

	// MARK: - Background

	/// What handing the rest of an upload over did
	private enum Handover {
		/// A task of the background session sends the rest
		case started(OutboxClaim, transferID: String)
		/// The rest is too short for a task (or there is none): the foreground goes on from the confirmed offset
		case foreground(OutboxClaim, offset: Int64)
	}

	/// Hands the rest of the claimed operation's upload to the handoff's session, from the offset the gateway confirms
	/// (or, when it cannot be asked now, the last one it confirmed, `knownOffset`: a task sent from a lower offset is
	/// refused, and the foreground goes on later)
	private func handOver(_ claim: OutboxClaim, knownOffset: Int64, length: Int64, to handoff: BackgroundHandoff)
		async throws(RemoteError) -> Handover
	{
		var claim = claim
		let (uploadID, token, reference) = try backgroundToken(claim)
		var offset = knownOffset
		if !Task.isCancelled {
			do {
				offset = try await client.uploadStatus(uploadID: uploadID, token: token).offset
			}
			catch .notFound {
				throw .notFound
			}
			catch .unauthenticated {
				throw .unauthenticated
			}
			catch {
				// A background task waits until the gateway can be reached
				offset = knownOffset
			}
		}
		if offset != claim.record.uploadOffset {
			let current = claim
			let confirmed = offset
			claim = try stored { () throws(IndexError) in try store.recordUploadOffset(current, offset: confirmed) }
		}
		guard length - offset >= handoff.minimumRemainder else {
			return .foreground(claim, offset: offset)
		}
		let transferID = try startTask(
			claim, uploadID: uploadID, token: token, reference: reference, offset: offset, length: length,
			using: handoff.transfers)
		handoff.didHandOver(operationID: claim.operationID, transferID: transferID)
		return .started(claim, transferID: transferID)
	}

	/// The upload and its scoped token, which background tasks carry instead of the credential
	private func backgroundToken(_ claim: OutboxClaim) throws(RemoteError) -> (String, String, String) {
		guard let credentials, let uploadID = claim.record.uploadID,
			let reference = claim.record.uploadTokenReference,
			let token = try? credentials.token(reference: reference)
		else {
			throw .store(.notAllowed("the upload has not been created, or its token is not kept"))
		}
		return (uploadID, token, reference)
	}

	/// Sends the rest of the claimed operation's upload (from the offset the gateway confirms) as one `PATCH` of the
	/// background session, without a checksum, so that the gateway keeps the bytes of an interrupted request. The task
	/// carries the upload's scoped token. Returns the transfer ID; the claim stays held (release it, so that the
	/// operation waits for the task). Once the task finished, process the operation again: it asks the gateway for the
	/// offset. `.unavailable` while another task sends the operation's upload.
	public func startBackgroundUpload(_ claim: OutboxClaim, using transfers: BackgroundTransfers)
		async throws(RemoteError) -> String
	{
		let (uploadID, token, reference) = try backgroundToken(claim)
		guard let length = claim.record.uploadLength else {
			throw .store(.notAllowed("the operation has no staged file"))
		}
		let offset = try await client.uploadStatus(uploadID: uploadID, token: token).offset
		return try startTask(
			claim, uploadID: uploadID, token: token, reference: reference, offset: offset, length: length,
			using: transfers)
	}

	/// Registers the transfer for the claimed operation, copies the rest into its directory (when it does not start at
	/// the beginning), and starts the task, which takes the transfer's claim over
	private func startTask(
		_ claim: OutboxClaim, uploadID: String, token: String, reference: String, offset: Int64, length: Int64,
		using transfers: BackgroundTransfers
	) throws(RemoteError) -> String {
		guard let stagedPath = claim.record.stagedPath else {
			throw .store(.notAllowed("the operation has no staged file"))
		}
		guard offset < length else {
			throw .store(.notAllowed("the gateway has every byte of the upload"))
		}
		// Nothing is started for an operation this process no longer holds
		let current = claim
		_ = try stored { () throws(IndexError) in try store.renewClaim(current, duration: claimDuration) }
		let worker = "\(claim.owner)/\(UUID().uuidString.lowercased())"
		let registered: (transfer: TransferRecord?, obsolete: [String])
		do {
			registered = try store.claimUploadTransfer(
				operationID: claim.operationID, itemID: claim.record.itemID, uploadID: uploadID,
				tokenReference: reference, totalSize: length, owner: worker, duration: claimDuration)
		}
		catch {
			throw .store(error)
		}
		for path in registered.obsolete {
			try? FileManager.default.removeItem(at: storage.url(relativePath: path))
		}
		guard let transfer = registered.transfer else {
			// A task sends this upload already: an operation has one at a time
			throw .unavailable(retryAfter: nil)
		}
		let directory = storage.transferURL(transfer.transferID)
		do throws(RemoteError) {
			var file = storage.url(relativePath: stagedPath)
			if offset > 0 {
				// The rest goes in its own file, which needs that much space
				let rest = length - offset
				if let available = FileIO.availableCapacity(at: storage.transfersURL), available < rest {
					throw .fileSystem("not enough storage for \(rest) bytes")
				}
				do {
					try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
				}
				catch {
					throw .fileSystem("could not create the transfer directory: \(error.localizedDescription)")
				}
				let restURL = directory.appending(path: BackgroundTransfers.uploadRestFileName)
				try FileIO.copy(from: file, offset: offset, length: rest, to: restURL)
				file = restURL
			}
			let request = try client.appendRequest(uploadID: uploadID, offset: offset, checksum: nil, token: token)
			try transfers.startUpload(
				transferID: transfer.transferID, request: request, file: file, offset: offset, claimedBy: worker)
			return transfer.transferID
		}
		catch {
			// No task runs it: neither the transfer nor the copy of the rest is needed
			try? store.removeTransfer(id: transfer.transferID)
			try? FileManager.default.removeItem(at: directory)
			throw error
		}
	}
}
