// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import os

/// The background `URLSession` of one process. Only one process can use a background session at a time, so the app
/// and the extension each own one, with distinct identifiers (for instance `<bundle>.remote.app` and
/// `<bundle>.remote.extension`); the app recreates the extension's session only to finish its events, from
/// `application(_:handleEventsForBackgroundURLSession:completionHandler:)`. Every task has a stable description that
/// names its transfer in the shared registry, which is how finished tasks are reconciled, in whichever process
/// handles them. Tasks carry scoped tokens, never the client credential. Nothing here keeps File Provider completion
/// handlers alive; the only handler held is the system's for finishing background events.
public final class BackgroundTransfers: NSObject, Sendable, URLSessionDownloadDelegate {
	public let identifier: String
	public let store: IndexStore
	public let storage: RemoteStorage
	public let credentials: any RemoteCredentialStore
	private let makeConfiguration: @Sendable () -> URLSessionConfiguration
	private let sessionBox = OSAllocatedUnfairLock<URLSession?>(initialState: nil)
	/// The system's handlers for finishing events, each called once all events were delivered
	private let eventsCompletions = OSAllocatedUnfairLock<[@Sendable () -> Void]>(initialState: [])
	private let onTransferFinished: (@Sendable (TransferRecord) -> Void)?
	/// Tasks being started (created, and not yet recorded or resumed), which reconciling leaves alone
	private let starting = OSAllocatedUnfairLock<Set<Int>>(initialState: [])

	/// Where a finished background download is kept in its transfer's directory, until it is verified
	public static let downloadedFileName = "background.part"
	/// Where the rest of an upload (from the confirmed offset) is copied in its transfer's directory
	public static let uploadRestFileName = "rest"
	static let descriptionPrefix = "synctrain.remote.v1"
	/// How the errors of tasks whose response came from another origin start
	static let protocolViolationPrefix = "protocol violation"

	/// - Parameters:
	///   - identifier: the session's identifier, distinct per process
	///   - appGroupIdentifier: the shared container identifier, so that both processes reach the files
	///   - onTransferFinished: called when a task finished (with its transfer as recorded), to verify or continue it
	public convenience init(
		identifier: String, appGroupIdentifier: String?, store: IndexStore, storage: RemoteStorage,
		credentials: any RemoteCredentialStore, onTransferFinished: (@Sendable (TransferRecord) -> Void)? = nil
	) {
		self.init(
			identifier: identifier, store: store, storage: storage, credentials: credentials,
			configuration: {
				let configuration = URLSessionConfiguration.background(withIdentifier: identifier)
				configuration.sharedContainerIdentifier = appGroupIdentifier
				configuration.sessionSendsLaunchEvents = true
				configuration.isDiscretionary = false
				configuration.httpCookieStorage = nil
				configuration.urlCache = nil
				configuration.httpShouldSetCookies = false
				return configuration
			}, onTransferFinished: onTransferFinished)
	}

	/// With another configuration than a background one (tests run the delegate with a mocked gateway this way)
	init(
		identifier: String, store: IndexStore, storage: RemoteStorage, credentials: any RemoteCredentialStore,
		configuration: @escaping @Sendable () -> URLSessionConfiguration,
		onTransferFinished: (@Sendable (TransferRecord) -> Void)?
	) {
		self.identifier = identifier
		self.store = store
		self.storage = storage
		self.credentials = credentials
		makeConfiguration = configuration
		self.onTransferFinished = onTransferFinished
	}

	/// The background session, created on first use. Creating it reconnects this process to the tasks the session
	/// runs (the system delivers their events to it).
	public var session: URLSession {
		sessionBox.withLock { box in
			if let session = box {
				return session
			}
			let session = URLSession(configuration: makeConfiguration(), delegate: self, delegateQueue: nil)
			box = session
			return session
		}
	}

	/// Whether the session was created in this process
	public var isConnected: Bool {
		sessionBox.withLock { $0 != nil }
	}

	/// For `application(_:handleEventsForBackgroundURLSession:completionHandler:)`: creates the session (which
	/// delivers its events) and calls the handler once they are all delivered. Every handler is called exactly once.
	public func handleEvents(completionHandler: @escaping @Sendable () -> Void) {
		eventsCompletions.withLock { $0.append(completionHandler) }
		_ = session
	}

	/// Lets go of the session once its tasks are done, without canceling them (for a session of another process,
	/// whose events this one finished)
	public func disconnect() {
		let session = sessionBox.withLock { box in
			defer { box = nil }
			return box
		}
		session?.finishTasksAndInvalidate()
	}

	/// The stable description of a transfer's task
	public static func taskDescription(kind: TransferKind, transferID: String) -> String {
		"\(descriptionPrefix)/\(kind.rawValue)/\(transferID)"
	}

	/// The transfer a task description names
	public static func transfer(fromTaskDescription description: String) -> (kind: TransferKind, transferID: String)? {
		let parts = description.split(separator: "/", omittingEmptySubsequences: false)
		guard parts.count == 3, parts[0] == descriptionPrefix, let kind = TransferKind(rawValue: String(parts[1])),
			!parts[2].isEmpty
		else {
			return nil
		}
		return (kind, String(parts[2]))
	}

	// MARK: - Starting tasks

	/// Starts a download task for the transfer (one task for everything that is missing, from the offset to the end).
	/// With `claimedBy`, the task takes that worker's claim on the transfer over, and is not started (`.unavailable`)
	/// unless the worker still holds it.
	public func startDownload(transferID: String, request: URLRequest, offset: Int64, claimedBy worker: String? = nil)
		throws(RemoteError)
	{
		let task = session.downloadTask(with: request)
		try start(task, kind: .download, transferID: transferID, offset: offset, claimedBy: worker)
	}

	/// Starts an upload task sending the file (the rest of an upload, from the offset, as one `PATCH`). With
	/// `claimedBy`, the task takes that worker's claim on the transfer over, and is not started (`.unavailable`) unless
	/// the worker still holds it.
	public func startUpload(
		transferID: String, request: URLRequest, file: URL, offset: Int64, claimedBy worker: String? = nil
	) throws(RemoteError) {
		let task = session.uploadTask(with: request, fromFile: file)
		try start(task, kind: .upload, transferID: transferID, offset: offset, claimedBy: worker)
	}

	private func start(
		_ task: URLSessionTask, kind: TransferKind, transferID: String, offset: Int64, claimedBy worker: String?
	) throws(RemoteError) {
		let description = Self.taskDescription(kind: kind, transferID: transferID)
		let taskIdentifier = task.taskIdentifier
		starting.withLock { _ = $0.insert(taskIdentifier) }
		defer { starting.withLock { _ = $0.remove(taskIdentifier) } }
		let assigned: Bool
		do {
			assigned = try store.assignTask(
				transferID: transferID, sessionIdentifier: identifier, taskIdentifier: task.taskIdentifier,
				taskDescription: description, offset: offset, claimedBy: worker)
		}
		catch {
			task.cancel()
			throw .store(error)
		}
		guard assigned else {
			// The worker's claim was lost: another worker has the transfer now. The task is not named after the
			// transfer, so that its end is not taken for that of the transfer's task.
			task.cancel()
			throw .unavailable(retryAfter: 5)
		}
		task.taskDescription = description
		task.resume()
	}

	/// The transfer a task of this session runs, unless another task runs it now (a task the registry replaced)
	private func transfer(of task: URLSessionTask) -> (kind: TransferKind, transfer: TransferRecord)? {
		guard let description = task.taskDescription,
			let (kind, transferID) = Self.transfer(fromTaskDescription: description),
			let transfer = try? store.transfer(id: transferID)
		else {
			return nil
		}
		if let current = transfer.taskIdentifier,
			current != task.taskIdentifier || transfer.sessionIdentifier != identifier
		{
			return nil
		}
		return (kind, transfer)
	}

	/// After a launch: transfers whose task this session no longer has are interrupted (to be resumed from their
	/// verified blocks or confirmed offset), tasks the registry does not know are canceled, and so are uploads that no
	/// operation needs anymore. Returns the transfers that were interrupted.
	@discardableResult
	public func reconcile() async throws(RemoteError) -> [TransferRecord] {
		// Tasks this process starts meanwhile are not in the list: their transfers were assigned after this
		let since = Date()
		let tasks = await session.allTasks
		let records: [TransferRecord]
		do {
			records = try store.transfers(sessionIdentifier: identifier)
		}
		catch {
			throw .store(error)
		}
		let live = Set(tasks.compactMap(\.taskDescription))
		let beingStarted = starting.withLock { $0 }
		var lost: [TransferRecord] = []
		for record in records where record.state == .active && record.updatedAt < since {
			if let description = record.taskDescription, live.contains(description) {
				continue
			}
			if let taskIdentifier = record.taskIdentifier, beingStarted.contains(taskIdentifier) {
				continue
			}
			try? store.finishTask(transferID: record.transferID, state: .interrupted, error: "the task was lost")
			if let operationID = record.operationID {
				try? store.expediteMutation(operationID: operationID)
			}
			lost.append(record)
		}
		let known = Set(records.compactMap(\.taskDescription))
		for task in tasks where !(task.taskDescription.map(known.contains) ?? false) {
			if beingStarted.contains(task.taskIdentifier) || starting.withLock({ $0.contains(task.taskIdentifier) }) {
				continue
			}
			task.cancel()
		}
		await cancelObsoleteUploads(among: tasks)
		return lost
	}

	/// Cancels this session's uploads that their operation no longer needs: it is gone or resolved, or a newer edit
	/// replaced the upload. Their transfers are removed once the tasks ended (`removeIdleUploadTransfers`).
	public func cancelObsoleteUploads() async {
		await cancelObsoleteUploads(among: session.allTasks)
	}

	private func cancelObsoleteUploads(among tasks: [URLSessionTask]) async {
		guard let records = try? store.transfers(sessionIdentifier: identifier) else {
			return
		}
		var obsolete: Set<String> = []
		for record in records where record.kind == .upload {
			guard let description = record.taskDescription, let operationID = record.operationID else {
				continue
			}
			do {
				let operation = try store.mutation(operationID: operationID)
				if operation.map({ $0.state.isTerminal || $0.uploadID != record.leaseID }) ?? true {
					obsolete.insert(description)
				}
			}
			catch {
				// Not readable now: the task is kept
				continue
			}
		}
		for task in tasks where task.taskDescription.map(obsolete.contains) ?? false {
			task.cancel()
		}
	}

	// MARK: - Delegate

	public func urlSession(
		_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL
	) {
		// The file is removed when this returns: keep it (or reject it) now
		guard let (_, transfer) = transfer(of: downloadTask), transfer.taskIdentifier == downloadTask.taskIdentifier
		else {
			return
		}
		let transferID = transfer.transferID
		let response = downloadTask.response as? HTTPURLResponse
		if let problem = Self.problem(with: response, of: downloadTask, transfer: transfer) {
			reject(transfer, problem)
			return
		}
		let destination = storage.transferURL(transferID).appending(path: Self.downloadedFileName)
		do {
			try? FileManager.default.removeItem(at: destination)
			try FileManager.default.moveItem(at: location, to: destination)
			try store.finishTask(transferID: transferID, state: .downloaded, error: nil)
		}
		catch {
			try? store.finishTask(transferID: transferID, state: .interrupted, error: error.localizedDescription)
		}
	}

	/// Why a background download's response cannot be used, if it cannot
	static func problem(with response: HTTPURLResponse?, of task: URLSessionTask, transfer: TransferRecord) -> String? {
		guard let response else {
			return "no response"
		}
		guard sameOrigin(response.url, task.originalRequest?.url) else {
			return "\(protocolViolationPrefix): the response came from another origin"
		}
		guard response.statusCode == 206 else {
			return "status \(response.statusCode)"
		}
		let expected = transfer.entityTag ?? transfer.contentID?.entityTag
		guard let entityTag = response.value(forHTTPHeaderField: "ETag"), entityTag == expected else {
			return "the content's validator changed"
		}
		let start = response.value(forHTTPHeaderField: "Content-Range").flatMap(RemoteFileClient.parseContentRange)?
			.start
		guard start == transfer.taskOffset else {
			return "the gateway answered with another range"
		}
		return nil
	}

	static func sameOrigin(_ a: URL?, _ b: URL?) -> Bool {
		guard let a, let b else {
			return false
		}
		return a.scheme?.lowercased() == b.scheme?.lowercased()
			&& a.host(percentEncoded: false)?.lowercased() == b.host(percentEncoded: false)?.lowercased()
			&& (a.port ?? 443) == (b.port ?? 443)
	}

	private func reject(_ transfer: TransferRecord, _ problem: String) {
		// A response from another origin: the token may have been sent there, so it is not used again. A lease's token
		// goes with the lease; an upload's belongs to its operation, which restarts the upload with a new one when it
		// finds this failure (see `Outbox`).
		if problem.hasPrefix(Self.protocolViolationPrefix), transfer.kind == .download, transfer.tokenReference != nil {
			try? store.setTransferLease(id: transfer.transferID, leaseID: "", tokenReference: nil)
			store.removeReleasedTokens(from: credentials)
		}
		try? store.finishTask(transferID: transfer.transferID, state: .failed, error: problem)
	}

	public func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?) {
		guard let (kind, transfer) = transfer(of: task) else {
			return
		}
		let transferID = transfer.transferID
		if transfer.taskIdentifier != nil {
			let response = task.response as? HTTPURLResponse
			if let response, !Self.sameOrigin(response.url, task.originalRequest?.url) {
				reject(transfer, "\(Self.protocolViolationPrefix): the response came from another origin")
			}
			else if kind == .upload, error == nil, let response, response.statusCode == 204 {
				try? store.finishTask(transferID: transferID, state: .completed, error: nil)
			}
			else {
				let message = error?.localizedDescription ?? response.map { "status \($0.statusCode)" } ?? "failed"
				try? store.finishTask(transferID: transferID, state: .interrupted, error: message)
			}
		}
		else if transfer.state == .active {
			// Detached from this task, and claimed by a worker since (after the task was taken for lost): its end is not
			// the transfer's
			return
		}
		// Otherwise `didFinishDownloadingTo` finished it already
		if kind == .upload, let operationID = transfer.operationID {
			// Whichever process looks next goes on with the operation right away (it asks the gateway for the offset)
			try? store.expediteMutation(operationID: operationID)
		}
		if let finished = try? store.transfer(id: transferID) {
			onTransferFinished?(finished)
		}
	}

	public func urlSession(
		_ session: URLSession, task: URLSessionTask, didReceive challenge: URLAuthenticationChallenge,
		completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
	) {
		let space = challenge.protectionSpace
		guard space.authenticationMethod == NSURLAuthenticationMethodServerTrust else {
			// Tasks carry their token in the Authorization header only, never in answer to a challenge
			completionHandler(.rejectProtectionSpace, nil)
			return
		}
		guard let trust = space.serverTrust, let endpoint = try? store.remoteEndpoint(host: space.host, port: space.port),
			RemoteTrust.evaluate(trust, host: endpoint.host, pins: endpoint.acceptedSPKIHashes)
		else {
			completionHandler(.cancelAuthenticationChallenge, nil)
			return
		}
		completionHandler(.useCredential, URLCredential(trust: trust))
	}

	public func urlSession(
		_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
		newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void
	) {
		// The gateway never redirects (background sessions may follow redirects without asking; where they ask, none
		// is followed, and the response checks reject what such a task received)
		completionHandler(nil)
	}

	public func urlSessionDidFinishEvents(forBackgroundURLSession session: URLSession) {
		let handlers = eventsCompletions.withLock { handlers in
			defer { handlers = [] }
			return handlers
		}
		for handler in handlers {
			DispatchQueue.main.async {
				handler()
			}
		}
	}
}
