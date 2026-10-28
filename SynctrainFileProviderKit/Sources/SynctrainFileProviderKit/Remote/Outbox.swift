// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Sends the outbox's operations to their gateways: uploads staged bytes, submits each operation with its client
/// operation ID, polls pending ones and records the results. The app and the extension both run it; claims with
/// generations make sure only one of them works on an operation at a time, and nothing ever sends a second operation ID
/// for the same edit. After an interruption or an uncertain answer, the original operation is recovered: polled by
/// its server ID, or submitted again with the same ID and stored payload, which the gateway answers with the same
/// operation. With a `handoff`, the rest of a long upload goes to a background task of the process's session; until the
/// task ended, the operation waits for it (neither process sends chunks of it, or starts another task for it).
public struct Outbox: Sendable {
	public let store: IndexStore
	public let storage: RemoteStorage
	/// The process (or worker) claims are made for
	public let owner: String
	public let scheduler: TransferScheduler?
	public let credentials: (any RemoteCredentialStore)?
	/// The client of a gateway, by service ID (nil when it is not paired)
	public let clients: @Sendable (String) -> RemoteFileClient?
	/// How long a claim lasts; extended before every request
	public var claimDuration: TimeInterval = 180
	/// The longest wait before an operation is tried again
	public var maximumBackoff: TimeInterval = 300
	/// How long to wait before asking about a pending operation again
	public var pollInterval: TimeInterval = 1
	/// When the rest of an upload goes to the process's background session (nil: uploads run in the foreground only)
	public var handoff: BackgroundHandoff?
	/// How long an operation whose upload a background task sends waits before it is looked at again (the task's end
	/// lets it go on right away, in whichever process handles it)
	public var backgroundRecheckInterval: TimeInterval = 300

	public init(
		store: IndexStore, storage: RemoteStorage, owner: String, scheduler: TransferScheduler? = nil,
		credentials: (any RemoteCredentialStore)? = nil, clients: @escaping @Sendable (String) -> RemoteFileClient?
	) {
		self.store = store
		self.storage = storage
		self.owner = owner
		self.scheduler = scheduler
		self.credentials = credentials
		self.clients = clients
	}

	/// What happened to an operation
	public enum Outcome: Sendable {
		/// Final: committed, kept as a conflict copy, failed or outcome-unknown
		case resolved(MutationRecord)
		/// Not final yet (pending on the gateway, or an attempt failed); try again no earlier than `notBefore`
		case waiting(MutationRecord, notBefore: Date?)
		/// Another process took the operation over
		case lost
	}

	private func stored<T>(_ body: () throws(IndexError) -> T) throws(RemoteError) -> T {
		do {
			return try body()
		}
		catch {
			throw .store(error)
		}
	}

	private func backoff(_ attempts: Int, retryAfter: TimeInterval? = nil) -> Date {
		let delay = retryAfter ?? min(maximumBackoff, pow(2, Double(min(attempts, 16))))
		return Date().addingTimeInterval(max(1, delay))
	}

	/// Works on operations that can make progress until none is left (or the limit is reached). An error that affects
	/// the whole gateway (such as `.unauthenticated`) stops the run and is thrown after its operation was put back.
	@discardableResult
	public func run(limit: Int = 100) async throws(RemoteError) -> [Outcome] {
		var outcomes: [Outcome] = []
		// Nothing more is started once the process is about to lose its execution time
		while outcomes.count < limit, handoff?.isRequested != true {
			guard
				let claim = try stored({ () throws(IndexError) in
					try store.claimMutation(owner: owner, duration: claimDuration)
				})
			else {
				break
			}
			outcomes.append(try await process(claim))
		}
		return outcomes
	}

	/// Claims and works on the operation, if it can make progress now
	public func process(operationID: String) async throws(RemoteError) -> Outcome {
		guard
			let claim = try stored({ () throws(IndexError) in
				try store.claimMutation(owner: owner, duration: claimDuration, operationID: operationID)
			})
		else {
			return .lost
		}
		return try await process(claim)
	}

	/// Advances a claimed operation as far as it goes now
	public func process(_ claim: OutboxClaim) async throws(RemoteError) -> Outcome {
		var claim = claim
		defer { removeReleasedTokens() }
		guard let serviceID = claim.record.serviceID, let client = clients(serviceID) else {
			_ = try? store.recordRetry(claim, error: "the gateway is not paired", notBefore: backoff(claim.record.attempts))
			throw .unauthenticated
		}
		// The staged file is uploaded again at most once per call, when the gateway no longer has the upload
		var reuploaded = false
		do {
			while true {
				switch claim.record.state {
				case .staged, .uploading:
					if needsUpload(claim.record) {
						switch try await checkBackgroundUpload(claim) {
						case .running:
							return try waitForBackgroundTask(claim)
						case .lost:
							return .lost
						case .proceed(let current):
							claim = current
						}
						switch try await upload(claim, client: client) {
						case .uploaded(let uploaded):
							claim = uploaded
						case .inBackground(let handedOver, _):
							return try waitForBackgroundTask(handedOver)
						}
					}
					else {
						return try outcome(of: try await submit(claim, client: client, mayUploadAgain: false))
					}
				case .uploaded, .submitted:
					if !reuploaded, try await uploadExpired(claim, client: client) {
						// Never sent, and the gateway forgot the upload: upload the staged file again
						reuploaded = true
						let current = claim
						guard let restarted = try stored({ () throws(IndexError) in try store.restartUpload(current) })
						else {
							return .lost
						}
						claim = restarted
						continue
					}
					switch try await submit(claim, client: client, mayUploadAgain: !reuploaded) {
					case .finished(let outcome):
						return outcome
					case .uploadAgain(let restarted):
						reuploaded = true
						claim = restarted
					}
				case .pending:
					return try await poll(claim, client: client)
				default:
					try? store.releaseClaim(claim)
					return .resolved(claim.record)
				}
			}
		}
		catch RemoteUploader.claimLost {
			return .lost
		}
		catch .store(.notAllowed(let reason)) {
			// Nothing to retry: the operation cannot be carried out as recorded
			guard
				let failed = try stored({ () throws(IndexError) in
					try store.recordRejection(claim, error: reason, currentItem: nil)
				})
			else {
				return .lost
			}
			return .resolved(failed.record)
		}
		catch {
			return try retry(claim, after: error)
		}
	}

	private func needsUpload(_ record: MutationRecord) -> Bool {
		(record.kind == .replace || (record.kind == .create && record.itemKind != .directory))
			&& record.state != .uploaded
	}

	/// Whether the upload of an operation that was never sent is gone: past the expiry recorded when it was created
	/// (activity extends it on the gateway, so the gateway is asked), and unknown to the gateway
	private func uploadExpired(_ claim: OutboxClaim, client: RemoteFileClient) async throws(RemoteError) -> Bool {
		let record = claim.record
		guard record.state == .uploaded, record.payload == nil, let uploadID = record.uploadID,
			let expires = record.uploadExpires, expires <= Date().addingTimeInterval(60)
		else {
			return false
		}
		do {
			_ = try await withMetadataSlot { () async throws(RemoteError) -> UploadStatus in
				try await client.uploadStatus(uploadID: uploadID)
			}
			return false
		}
		catch .notFound {
			return true
		}
	}

	/// The result of a submission: final for now, or the staged file must be uploaded again first
	private enum Submission {
		case finished(Outcome)
		case uploadAgain(OutboxClaim)
	}

	private func outcome(of submission: Submission) throws(RemoteError) -> Outcome {
		switch submission {
		case .finished(let outcome):
			return outcome
		case .uploadAgain:
			throw .protocolViolation("an operation without an upload was asked to upload again")
		}
	}

	private func upload(_ claim: OutboxClaim, client: RemoteFileClient) async throws(RemoteError)
		-> RemoteUploader.UploadResult
	{
		guard
			let remoteFolderID = try stored({ () throws(IndexError) in
				try store.remoteBinding(folderID: claim.record.folderID)?.remoteFolderID
			})
		else {
			throw .store(.notAllowed("the folder is not bound to a gateway"))
		}
		let uploader = RemoteUploader(
			store: store, client: client, storage: storage, scheduler: scheduler, credentials: credentials)
		return try await uploader.upload(claim, remoteFolderID: remoteFolderID, handoff: handoff)
	}

	// MARK: - Background tasks

	/// What the background transfers of an operation leave for the foreground
	private enum BackgroundCheck {
		/// A task sends the operation's upload now: the operation waits for it
		case running
		/// Nothing runs it: go on (with the claim as it is now)
		case proceed(OutboxClaim)
		case lost
	}

	/// Whether a background task sends the claimed operation's upload now. Transfers of the operation that ended are
	/// removed (with their copies of the rest, and tokens); one whose task was answered from another origin restarts
	/// the upload, so that its token is not used again. This process cancels its own tasks of uploads the operation no
	/// longer uses.
	private func checkBackgroundUpload(_ claim: OutboxClaim) async throws(RemoteError) -> BackgroundCheck {
		var claim = claim
		let now = Date()
		let transfers = try stored { () throws(IndexError) in try store.uploadTransfers(operationID: claim.operationID) }
		guard !transfers.isEmpty else {
			return .proceed(claim)
		}
		var obsoleteTasks = false
		for transfer in transfers where IndexStore.isBusy(transfer, now: now) {
			if transfer.leaseID == claim.record.uploadID, claim.record.uploadID != nil {
				return .running
			}
			obsoleteTasks = obsoleteTasks || transfer.sessionIdentifier == handoff?.transfers.identifier
		}
		if obsoleteTasks, let handoff {
			await handoff.transfers.cancelObsoleteUploads()
		}
		let violated = transfers.contains {
			$0.state == .failed && $0.leaseID == claim.record.uploadID
				&& $0.lastError?.hasPrefix(BackgroundTransfers.protocolViolationPrefix) == true
		}
		if violated, claim.record.state.isUnsent {
			let current = claim
			guard let restarted = try stored({ () throws(IndexError) in try store.restartUpload(current) }) else {
				return .lost
			}
			claim = restarted
		}
		removeIdleTransfers()
		return .proceed(claim)
	}

	/// Releases the claim of an operation whose upload a background task sends: it is looked at again after
	/// `backgroundRecheckInterval`, or as soon as the task ended
	private func waitForBackgroundTask(_ claim: OutboxClaim) throws(RemoteError) -> Outcome {
		let notBefore = Date().addingTimeInterval(backgroundRecheckInterval)
		guard
			let released = try stored({ () throws(IndexError) in try store.recordPending(claim, notBefore: notBefore) })
		else {
			return .lost
		}
		return .waiting(released.record, notBefore: notBefore)
	}

	/// Removes the upload transfers no background task runs anymore, with their copies of the rest of the upload; the
	/// tokens they held are released (and removed by `removeReleasedTokens`)
	public func removeIdleTransfers() {
		guard let paths = try? store.removeIdleUploadTransfers() else {
			return
		}
		for path in paths {
			try? FileManager.default.removeItem(at: storage.url(relativePath: path))
		}
	}

	/// Submits the operation (or replays it, after an uncertain answer) and records the answer. With
	/// `mayUploadAgain`, a first submission refused because the gateway no longer has the upload asks for the staged
	/// file to be uploaded again (`.uploadAgain`).
	private func submit(_ claim: OutboxClaim, client: RemoteFileClient, mayUploadAgain: Bool)
		async throws(RemoteError) -> Submission
	{
		guard let renewed = try stored({ () throws(IndexError) in try store.renewClaim(claim, duration: claimDuration) })
		else {
			return .finished(.lost)
		}
		// A request built now was never sent: the gateway's answer to it is all it knows of the operation
		let firstSubmission = renewed.record.payload == nil
		let generation = try stored { () throws(IndexError) in
			try store.remoteAuthorityGeneration(serviceID: client.endpoint.serviceID)
		}
		let preparation = try stored { () throws(IndexError) in
			try store.prepareSubmission(renewed, authorityGeneration: generation)
		}
		let submitted: OutboxClaim
		let request: OperationRequest
		let encoded: Data
		switch preparation {
		case .lost:
			return .finished(.lost)
		case .failed:
			return .finished(.resolved(try current(claim)))
		case .ready(let ready, let built, let body):
			submitted = ready
			request = built
			encoded = body
		}
		let operation: RemoteOperation
		do {
			operation = try await withMetadataSlot { () async throws(RemoteError) -> RemoteOperation in
				try await client.submitOperation(encoded)
			}
		}
		catch .notFound where firstSubmission && request.upload != nil {
			return try await recoverUnknownUpload(
				submitted, request: request, client: client, mayUploadAgain: mayUploadAgain)
		}
		catch {
			if let rejected = try reject(submitted, error) {
				return .finished(rejected)
			}
			return .finished(try retry(submitted, after: error))
		}
		return .finished(try record(operation, for: submitted))
	}

	/// The gateway answered the first submission of a request with an upload `404` (an unknown upload, item or
	/// destination), and stored nothing: an upload that is gone (for instance expired) is made again from the staged
	/// file; otherwise the item or destination is gone, which rejects the operation. When the upload cannot be checked
	/// now, the request is forgotten, so that the next attempt checks again before it sends anything.
	private func recoverUnknownUpload(
		_ claim: OutboxClaim, request: OperationRequest, client: RemoteFileClient, mayUploadAgain: Bool
	) async throws(RemoteError) -> Submission {
		guard let upload = request.upload else {
			return .finished(try reject(claim, .notFound) ?? .lost)
		}
		let gone: Bool
		do {
			_ = try await withMetadataSlot { () async throws(RemoteError) -> UploadStatus in
				try await client.uploadStatus(uploadID: upload)
			}
			gone = false
		}
		catch .notFound {
			gone = true
		}
		catch {
			guard
				let withdrawn = try stored({ () throws(IndexError) in
					try store.withdrawSubmission(claim, restartUpload: false)
				})
			else {
				return .finished(.lost)
			}
			return .finished(try retry(withdrawn, after: error))
		}
		guard gone, mayUploadAgain else {
			return .finished(try reject(claim, .notFound) ?? .lost)
		}
		guard
			let restarted = try stored({ () throws(IndexError) in
				try store.withdrawSubmission(claim, restartUpload: true)
			})
		else {
			return .finished(.lost)
		}
		return .uploadAgain(restarted)
	}

	/// Asks the gateway for a pending operation's state
	private func poll(_ claim: OutboxClaim, client: RemoteFileClient) async throws(RemoteError) -> Outcome {
		guard let serverID = claim.record.serverOperationID else {
			// Not known: the stored request is submitted again, which returns the same operation
			return try outcome(of: try await submit(claim, client: client, mayUploadAgain: false))
		}
		guard let renewed = try stored({ () throws(IndexError) in try store.renewClaim(claim, duration: claimDuration) })
		else {
			return .lost
		}
		let operation: RemoteOperation
		do {
			operation = try await withMetadataSlot { () async throws(RemoteError) -> RemoteOperation in
				try await client.operation(id: serverID)
			}
		}
		catch .notFound {
			// The gateway no longer knows it (it lost its state): the outcome needs review
			guard
				let unknown = try stored({ () throws(IndexError) in
					try store.recordOutcomeUnknown(renewed, reason: "the gateway no longer knows the operation")
				})
			else {
				return .lost
			}
			return .resolved(unknown.record)
		}
		catch {
			return try retry(renewed, after: error)
		}
		return try record(operation, for: renewed)
	}

	private func record(_ operation: RemoteOperation, for claim: OutboxClaim) throws(RemoteError) -> Outcome {
		guard operation.id == claim.operationID else {
			throw .protocolViolation("the gateway answered for another operation")
		}
		guard let updated = try stored({ () throws(IndexError) in try store.recordOperation(claim, operation) }) else {
			return .lost
		}
		if updated.record.state.isTerminal {
			try? store.releaseClaim(updated)
			removeReleasedFiles()
			removeIdleTransfers()
			return .resolved(updated.record)
		}
		// Pending on the gateway: ask again shortly (a poll is not a failed attempt, so it does not grow the backoff)
		let notBefore = Date().addingTimeInterval(pollInterval)
		_ = try? store.recordPending(updated, notBefore: notBefore)
		return .waiting(try current(updated), notBefore: notBefore)
	}

	/// Records a rejection the gateway found while handling the request (`403`, `404`, `409`, `412`, `422`); nil for
	/// errors worth trying again
	private func reject(_ claim: OutboxClaim, _ error: RemoteError) throws(RemoteError) -> Outcome? {
		let message: String
		var item: RemoteItem?
		switch error {
		case .forbidden(let code):
			message = code
		case .notFound:
			message = RemoteErrorCode.notFound
		case .nameConflict(let current):
			message = RemoteErrorCode.nameConflict
			item = current
		case .revisionMismatch(let current):
			message = RemoteErrorCode.revisionMismatch
			item = current
		case .operationMismatch:
			message = RemoteErrorCode.operationMismatch
		case .server(let status, let code) where status == 409 || status == 400:
			message = code ?? "status \(status)"
		default:
			return nil
		}
		guard
			let rejected = try stored({ () throws(IndexError) in
				try store.recordRejection(claim, error: message, currentItem: item)
			})
		else {
			return .lost
		}
		removeReleasedFiles()
		return .resolved(rejected.record)
	}

	/// Puts the operation back to be tried later. Errors that concern the whole gateway are thrown.
	private func retry(_ claim: OutboxClaim, after error: RemoteError) throws(RemoteError) -> Outcome {
		let notBefore = backoff(claim.record.attempts + 1, retryAfter: error.retryAfter)
		guard
			let updated = try stored({ () throws(IndexError) in
				try store.recordRetry(claim, error: "\(error)", notBefore: notBefore)
			})
		else {
			return .lost
		}
		switch error {
		case .unauthenticated, .canceled, .unreachable(.serverCertificateUntrusted):
			throw error
		default:
			return .waiting(updated.record, notBefore: notBefore)
		}
	}

	private func current(_ claim: OutboxClaim) throws(RemoteError) -> MutationRecord {
		try stored { () throws(IndexError) in try store.mutation(operationID: claim.operationID) } ?? claim.record
	}

	private func withMetadataSlot<T: Sendable>(_ body: () async throws(RemoteError) -> T) async throws(RemoteError)
		-> T
	{
		guard let scheduler else {
			return try await body()
		}
		return try await scheduler.withSlot(.metadata, body)
	}

	/// Removes staged files no unresolved operation needs anymore
	public func removeReleasedFiles() {
		guard let paths = try? store.releasableStagedFiles() else {
			return
		}
		RemoteUploader.removeReleasedStagedFiles(paths, store: store, storage: storage)
	}

	/// Removes the upload (and lease) tokens no operation or transfer refers to anymore from the credential store:
	/// those of operations that were resolved, replaced or restarted here, or by the other process
	public func removeReleasedTokens() {
		if let credentials {
			store.removeReleasedTokens(from: credentials)
		}
	}
}
