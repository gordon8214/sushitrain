// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Darwin
@preconcurrency import FileProvider
import Foundation
import os
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// The domain, which the framework does not declare Sendable (it is not changed after it was created)
private struct DomainReference: @unchecked Sendable {
	let domain: NSFileProviderDomain
}

/// Serves the folders on gateways through their remote file API. Their metadata is projected into the index, which
/// items and enumeration work from; contents and changes go to the gateway over HTTPS.
///
/// - Contents: a staged local change (the newest version) or a verified cached copy is cloned for the system; otherwise
///   the file is downloaded into the cache first, every block verified.
/// - Changes: contents are staged immutably, the change is recorded in the shared outbox, and its operation is driven
///   to the gateway's commit inside the call. A change the gateway has not committed is reported as not synchronized;
///   the system's next attempt continues the same operation. An upload still running after `foregroundUploadBudget`
///   (or when the extension is invalidated) hands its rest to the extension's background session, which sends it while
///   neither process runs; whichever process handles the task's end (the app, when the extension does not run) submits
///   the operation, and the system is told that the change is resolved.
/// - Metadata is refreshed in the background when the system enumerates a remote folder (or the extension starts), at
///   most every 30 seconds per folder.
final class RemoteProvider: Sendable {
	let context: ExtensionContext
	let storage: RemoteStorage
	private let domainReference: DomainReference
	private let credentials: KeychainCredentialStore
	let scheduler: TransferScheduler
	/// Who claims outbox operations and transfers in this process. A claim of a process that was terminated expires,
	/// after which the app (or a later instance of the extension) continues the operation.
	let owner = RemoteProvider.ownerPrefix + UUID().uuidString.lowercased()
	/// Hands the rest of a long upload to the extension's background session
	let handoff: BackgroundHandoff?
	private let state = OSAllocatedUnfairLock(initialState: State())

	/// What the owners of this process's claims start with
	static let ownerPrefix = "extension."
	/// How long an upload runs in the foreground of a call before its rest goes to the background session (a rest of at
	/// least one chunk; a shorter one is sent in the call)
	static let foregroundUploadBudget: TimeInterval = 60
	/// How long the extension follows an operation the gateway still applies after its background upload ended
	private static let commitFollowUp: TimeInterval = 20
	/// The extension's background session: one per process, since a second session with its identifier would not work
	private static let sessionTransfers = OSAllocatedUnfairLock<BackgroundTransfers?>(initialState: nil)
	/// The instance that goes on with what the session's tasks finished (the newest one)
	private static let current = OSAllocatedUnfairLock(initialState: CurrentProvider())

	private struct CurrentProvider {
		weak var provider: RemoteProvider?
	}

	/// How often a folder's metadata is refreshed at most
	static let refreshInterval: TimeInterval = 30
	/// How often a gateway's capabilities and folder permissions are fetched at most
	static let serviceCheckInterval: TimeInterval = 300
	/// How long optional requests (capabilities for thumbnails, thumbnails) are not made to a gateway that could not
	/// be reached, did not accept this device or did not answer in time
	static let failureBackoff: TimeInterval = 60
	/// The version of the remote file API this extension speaks (the app's `RemoteGatewayStatus.apiVersion`). Nothing
	/// is sent to a gateway that speaks another one.
	static let apiVersion = 1
	/// Where the errors the system waits on are remembered (in the app group's defaults), so that a later instance of
	/// the extension signals them resolved: the system keeps waiting after the process that reported them ended
	private static let reportedErrorsKey = "remoteReportedErrors"

	private struct State {
		/// One client (and session) per gateway, for as long as its address and pins stay the same
		var clients: [String: RemoteFileClient] = [:]
		var capabilities: [String: (value: Capabilities, fetched: Date)] = [:]
		var lastRefresh: [String: Date] = [:]
		var refreshing: Set<String> = []
		var lastServiceCheck: [String: Date] = [:]
		/// When a gateway last could not be reached, did not accept this device or did not answer in time
		var failedAt: [String: Date] = [:]
		/// Errors the system was given that it waits on until they are signaled resolved (codes, by service ID)
		var reportedErrors: [String: Set<Int>] = [:]
		/// Background work and the system's calls in progress
		var tasks: [UUID: Task<Void, Never>] = [:]
		var isInvalidated = false
	}

	init(context: ExtensionContext, domain: NSFileProviderDomain) {
		self.context = context
		storage = context.storage
		domainReference = DomainReference(domain: domain)
		credentials = KeychainCredentialStore(accessGroup: Self.appGroupIdentifier)
		scheduler = TransferScheduler(store: context.store, owner: owner)
		handoff = Self.backgroundTransfers(context: context, credentials: credentials).map {
			BackgroundHandoff(transfers: $0, foregroundBudget: Self.foregroundUploadBudget)
		}
		try? storage.createDirectories()
		Self.current.withLock { $0.provider = self }
	}

	/// The extension's background session (created on first use of its `session`), nil without the app's bundle
	/// identifier, which its identifier derives from
	private static func backgroundTransfers(context: ExtensionContext, credentials: KeychainCredentialStore)
		-> BackgroundTransfers?
	{
		guard let appBundleIdentifier = RemoteSessionIdentifier.appBundleIdentifier() else {
			return nil
		}
		return sessionTransfers.withLock { transfers in
			if let transfers {
				return transfers
			}
			let created = BackgroundTransfers(
				identifier: RemoteSessionIdentifier.fileProviderExtension(appBundleIdentifier: appBundleIdentifier),
				appGroupIdentifier: appGroupIdentifier, store: context.store, storage: context.storage,
				credentials: credentials
			) { record in
				current.withLock { $0.provider }?.backgroundTransferFinished(record)
			}
			transfers = created
			return created
		}
	}

	/// The app group, which also serves as the Keychain access group the app and the extension share
	static var appGroupIdentifier: String? {
		(Bundle.main.object(forInfoDictionaryKey: SharedContainer.appGroupInfoKey) as? String).flatMap {
			$0.isEmpty ? nil : $0
		}
	}

	private var manager: NSFileProviderManager? {
		NSFileProviderManager(for: domainReference.domain)
	}

	private static var sharedDefaults: UserDefaults? {
		appGroupIdentifier.flatMap { UserDefaults(suiteName: $0) }
	}

	/// Refreshes the remote folders in the background, and makes sure that what earlier instances of the extension
	/// reported (changes waiting for their gateway, a location paused for a download) is signaled resolved once a
	/// gateway answers. Only errors this path reported are signaled: signaling one it did not report would lift what
	/// the folders this device synchronizes itself reported (such as a pause while the app does not run).
	func start() {
		guard let bindings = try? context.store.remoteBindings(), !bindings.isEmpty else {
			return
		}
		reconnectBackgroundSession()
		let serviceIDs = Set(bindings.compactMap(\.serviceID))
		var reported: [String: Set<Int>] = [:]
		if let stored = Self.sharedDefaults?.dictionary(forKey: Self.reportedErrorsKey) as? [String: [Int]] {
			for (serviceID, codes) in stored where serviceIDs.contains(serviceID) {
				reported[serviceID] = Set(codes)
			}
		}
		for binding in bindings {
			let serviceID = binding.serviceID
			// Changes that wait for their gateway were reported as such (or the system has not made them yet)
			if (try? context.store.hasUnresolvedMutations(folderID: binding.folderID)) == true {
				reported[serviceID, default: []].formUnion([
					NSFileProviderError.cannotSynchronize.rawValue, NSFileProviderError.notAuthenticated.rawValue,
				])
			}
		}
		let codes = reported
		let snapshot = state.withLock { state in
			state.reportedErrors.merge(codes) { $0.union($1) }
			return state.reportedErrors
		}
		Self.storeReportedErrors(snapshot)
		refreshSoon()
	}

	private static func storeReportedErrors(_ errors: [String: Set<Int>]) {
		let stored = errors.filter { !$0.value.isEmpty }.mapValues { $0.sorted() }
		if stored.isEmpty {
			sharedDefaults?.removeObject(forKey: reportedErrorsKey)
		}
		else {
			sharedDefaults?.set(stored, forKey: reportedErrorsKey)
		}
	}

	/// Cancels the work in progress (including the system's calls, which complete as canceled); the outbox and the
	/// transfer registry let it continue later. The rest of an upload in progress goes to the background session first.
	func invalidate() {
		handoff?.request()
		let tasks = state.withLock { state in
			state.isInvalidated = true
			defer { state.tasks = [:] }
			return Array(state.tasks.values)
		}
		for task in tasks {
			task.cancel()
		}
	}

	/// Runs work in the background, canceled when the extension is invalidated
	func background(_ work: @escaping @Sendable () async -> Void) {
		let id = UUID()
		state.withLock { current in
			guard !current.isInvalidated else {
				return
			}
			current.tasks[id] = Task(priority: .utility) { [self] in
				await work()
				_ = state.withLock { $0.tasks.removeValue(forKey: id) }
			}
		}
	}

	/// Runs a call of the system, canceled when the extension is invalidated (canceled from the start when it is
	/// already). The work calls the call's completion handler in any case: cancellation completes it as canceled.
	func call(_ work: @escaping @Sendable () async -> Void) -> Task<Void, Never> {
		let id = UUID()
		return state.withLock { current in
			let task = Task { [self] in
				await work()
				_ = state.withLock { $0.tasks.removeValue(forKey: id) }
			}
			if current.isInvalidated {
				task.cancel()
			}
			else {
				current.tasks[id] = task
			}
			return task
		}
	}

	// MARK: - Gateways

	/// The client of the gateway (nil when it is not paired). Clients are shared by all calls, so that parallel requests
	/// share connections; a new one is made when the stored address or pins change.
	func client(serviceID: String) -> RemoteFileClient? {
		guard let endpoint = try? context.store.remoteEndpoint(serviceID: serviceID) else {
			return nil
		}
		return state.withLock { state in
			if let client = state.clients[serviceID], client.endpoint == endpoint {
				return client
			}
			let client = RemoteFileClient(endpoint: endpoint, credentials: credentials)
			state.clients[serviceID] = client
			return client
		}
	}

	/// The binding of the folder of an item the system deletes: the item's folder, or (once its row is removed, while
	/// the deletion is not confirmed) that of its overlay. Nil for items this device no longer has.
	func binding(ofDeleted identifier: NSFileProviderItemIdentifier) throws(IndexError) -> RemoteFolderBinding? {
		guard let id = identifier.itemID else {
			return nil
		}
		if let item = try context.store.item(id: id) {
			return try context.remoteBinding(folderID: item.folderID)
		}
		if let overlay = try context.store.remoteOverlay(itemID: id) {
			return try context.remoteBinding(folderID: overlay.folderID)
		}
		return nil
	}

	func gatewayName(_ binding: RemoteFolderBinding) -> String {
		RemoteFailure.gatewayName(try? context.store.remoteEndpoint(serviceID: binding.serviceID))
	}

	var outbox: Outbox {
		var outbox = Outbox(
			store: context.store, storage: storage, owner: owner, scheduler: scheduler, credentials: credentials,
			clients: { [self] serviceID in client(serviceID: serviceID) })
		outbox.handoff = handoff
		return outbox
	}

	// MARK: - Background session

	/// Reconnects to the extension's background session when it may have tasks (running, or finished while the
	/// extension did not run), so that their events reach this process, and reconciles them. Claims an earlier instance
	/// of the extension left (it was terminated) are released, so that their transfers resume right away from what was
	/// verified.
	private func reconnectBackgroundSession() {
		try? context.store.releaseClaims(ownerPrefix: Self.ownerPrefix, except: owner)
		guard let transfers = handoff?.transfers,
			let records = try? context.store.transfers(sessionIdentifier: transfers.identifier),
			records.contains(where: { $0.taskDescription != nil })
		else {
			return
		}
		background { [self] in
			do throws(RemoteError) {
				let lost = try await transfers.reconcile()
				trace("remote: reconnected to the background session, \(lost.count) tasks lost")
			}
			catch {
				log.info("Could not reconcile the background session: \(error.summary)")
			}
			outbox.removeIdleTransfers()
			outbox.removeReleasedTokens()
		}
	}

	/// A task of the extension's session ended while the extension runs: an upload's operation is submitted (and
	/// followed while the gateway applies it), and the system is told that the change it waits on is resolved
	private func backgroundTransferFinished(_ record: TransferRecord) {
		guard record.kind == .upload, let operationID = record.operationID else {
			return
		}
		trace("remote: background upload \(record.transferID) ended (\(record.state.rawValue))")
		background { [self] in
			let started = Date()
			let outbox = outbox
			while !Task.isCancelled {
				let outcome: Outbox.Outcome
				do throws(RemoteError) {
					outcome = try await outbox.process(operationID: operationID)
				}
				catch {
					trace("remote: after the background upload of \(operationID): \(error.summary)")
					return
				}
				switch outcome {
				case .resolved(let resolved):
					signalWorkingSet()
					if let serviceID = resolved.serviceID {
						reached(serviceID: serviceID)
					}
					return
				case .waiting(let waiting, let notBefore)
				where waiting.state == .pending && Date().timeIntervalSince(started) < Self.commitFollowUp:
					try? await Task.sleep(for: .seconds(max(0.05, (notBefore ?? Date()).timeIntervalSinceNow)))
				default:
					// Handed to the background again, or tried later (by the app too)
					return
				}
			}
		}
	}

	/// Remembers an error the system waits on until it is signaled resolved
	func reported(_ code: NSFileProviderError.Code, serviceID: String?) {
		guard let serviceID else {
			return
		}
		let snapshot = state.withLock { state -> [String: Set<Int>]? in
			state.reportedErrors[serviceID, default: []].insert(code.rawValue).inserted ? state.reportedErrors : nil
		}
		if let snapshot {
			Self.storeReportedErrors(snapshot)
		}
	}

	/// Whether the gateway recently could not be reached, did not accept this device or did not answer in time:
	/// optional requests (such as for thumbnails) are not made to it for a while
	func failedRecently(serviceID: String) -> Bool {
		state.withLock { state in
			state.failedAt[serviceID].map { Date().timeIntervalSince($0) < Self.failureBackoff } ?? false
		}
	}

	/// Remembers that a request to the gateway failed in a way that concerns the whole gateway
	func noteFailure(_ error: RemoteError, serviceID: String) {
		switch error {
		case .unreachable, .unauthenticated:
			noteUnresponsive(serviceID: serviceID)
		default:
			break
		}
	}

	/// Remembers that the gateway did not answer an optional request in time
	func noteUnresponsive(serviceID: String) {
		state.withLock { $0.failedAt[serviceID] = Date() }
	}

	/// The gateway answered: the errors reported for it are resolved, so the system tries the affected items again
	/// (changes waiting for it, downloads after a paused location, items after a new pairing)
	func reached(serviceID: String) {
		let (codes, snapshot) = state.withLock { state in
			state.failedAt[serviceID] = nil
			let codes = state.reportedErrors.removeValue(forKey: serviceID) ?? []
			return (codes, state.reportedErrors)
		}
		guard !codes.isEmpty else {
			return
		}
		Self.storeReportedErrors(snapshot)
		guard let manager else {
			return
		}
		for code in codes.sorted() {
			guard let code = NSFileProviderError.Code(rawValue: code) else {
				continue
			}
			manager.signalErrorResolved(NSFileProviderError(code)) { error in
				if let error {
					log.warning("Could not signal that \(code.rawValue) is resolved: \(error.localizedDescription)")
				}
			}
		}
		trace("remote: \(serviceID) reached, signaled \(codes.sorted()) resolved")
	}

	/// Tells the system to fetch changes, for items it does not learn about from the call it made
	func signalWorkingSet() {
		SharedNotification.indexChanged.post()
		manager?.signalEnumerator(for: .workingSet) { error in
			if let error {
				log.warning("Could not signal the working set: \(error.localizedDescription)")
			}
		}
	}

	/// The gateway's capabilities (for its optional features), fetched at most every five minutes, and not for a while
	/// after the gateway could not be used. Fetching them also records its authority generation.
	func capabilities(serviceID: String) async -> Capabilities? {
		let cached = state.withLock { $0.capabilities[serviceID] }
		if let cached, Date().timeIntervalSince(cached.fetched) < Self.serviceCheckInterval {
			return cached.value
		}
		guard !failedRecently(serviceID: serviceID), let client = client(serviceID: serviceID) else {
			return cached?.value
		}
		do {
			let capabilities = try await RemoteSync(store: context.store, client: client, scheduler: scheduler)
				.refreshCapabilities()
			state.withLock { $0.capabilities[serviceID] = (capabilities, Date()) }
			if capabilities.apiVersion == Self.apiVersion {
				reached(serviceID: serviceID)
			}
			return capabilities
		}
		catch {
			noteFailure(error, serviceID: serviceID)
			trace("remote: capabilities of \(serviceID) failed: \(error.summary)")
			return cached?.value
		}
	}

	/// The capabilities fetched last, without asking the gateway
	func cachedCapabilities(serviceID: String) -> Capabilities? {
		state.withLock { $0.capabilities[serviceID]?.value }
	}

	/// The gateway's version of the remote file API, when the capabilities fetched last show one this extension does
	/// not speak
	func incompatibleVersion(serviceID: String?) -> Int? {
		guard let serviceID, let version = cachedCapabilities(serviceID: serviceID)?.apiVersion,
			version != Self.apiVersion
		else {
			return nil
		}
		return version
	}

	// MARK: - Metadata

	/// Brings the metadata of the remote folders (all of them by default) up to date in the background, at most every
	/// 30 seconds per folder, and tells the system when something it shows changed. It never waits for the gateway,
	/// and nothing is inferred from requests that fail.
	func refreshSoon(folderIDs: [String]? = nil) {
		guard let readable = try? context.store.remoteBindings().filter(\.permissions.read) else {
			return
		}
		let bindings = folderIDs.map { folderIDs in readable.filter { folderIDs.contains($0.folderID) } } ?? readable
		let now = Date()
		let due = state.withLock { state in
			bindings.filter { binding in
				guard !state.isInvalidated, !state.refreshing.contains(binding.folderID),
					state.lastRefresh[binding.folderID].map({ now.timeIntervalSince($0) >= Self.refreshInterval }) ?? true
				else {
					return false
				}
				state.refreshing.insert(binding.folderID)
				state.lastRefresh[binding.folderID] = now
				return true
			}
		}
		let byService = Dictionary(grouping: due.filter { $0.serviceID != nil }, by: { $0.serviceID ?? "" })
		for (serviceID, bindings) in byService {
			let folderIDs = bindings.map(\.folderID)
			background { [self] in
				await refresh(folderIDs: folderIDs, serviceID: serviceID)
				state.withLock { $0.refreshing.subtract(folderIDs) }
			}
		}
		// Folders without a gateway cannot be refreshed
		let orphaned = due.filter { $0.serviceID == nil }.map(\.folderID)
		if !orphaned.isEmpty {
			state.withLock { $0.refreshing.subtract(orphaned) }
		}
	}

	private func refresh(folderIDs: [String], serviceID: String) async {
		guard let client = client(serviceID: serviceID) else {
			return
		}
		let sync = RemoteSync(store: context.store, client: client, scheduler: scheduler)
		var changed = false
		do throws(RemoteError) {
			let checkDue = state.withLock { state in
				let due =
					state.capabilities[serviceID] == nil
					|| state.lastServiceCheck[serviceID].map { Date().timeIntervalSince($0) >= Self.serviceCheckInterval }
						?? true
				if due {
					state.lastServiceCheck[serviceID] = Date()
				}
				return due
			}
			if checkDue {
				// A changed authority generation starts a fresh snapshot; changed permissions change capabilities
				let capabilities = try await sync.refreshCapabilities()
				state.withLock { $0.capabilities[serviceID] = (capabilities, Date()) }
				if capabilities.apiVersion == Self.apiVersion {
					try await sync.refreshFolders()
				}
			}
			if let version = incompatibleVersion(serviceID: serviceID) {
				// Requests in another version of the API could be misunderstood: nothing is asked or sent until one of
				// the two is updated (the app tells the user)
				log.info("Not refreshing \(serviceID): it uses version \(version) of the remote file API")
				trace("remote: refresh of \(folderIDs) skipped: API version \(version)")
				return
			}
			for folderID in folderIDs {
				do throws(RemoteError) {
					changed = try await sync.refresh(folderID: folderID) || changed
				}
				catch .store(let failure) {
					// The folder's binding changed meanwhile; the others are refreshed still
					trace("remote: refresh of \(folderID) skipped: \(failure)")
				}
			}
			reached(serviceID: serviceID)
			trace("remote: refreshed \(folderIDs) (changed: \(changed)) \(memoryFootprint())")
		}
		catch {
			noteFailure(error, serviceID: serviceID)
			log.info("Could not refresh the folders of \(serviceID): \(error.summary)")
			trace("remote: refresh of \(folderIDs) failed: \(error.summary)")
		}
		if changed {
			signalWorkingSet()
		}
	}

	// MARK: - Local copies

	/// A verified local copy of the content: downloaded before, or a staged edit the gateway committed as this version
	/// (as `RemoteDownloader.cachedContent` finds them, without needing a client)
	func cachedCopy(_ contentID: ContentIdentity?) throws(IndexError) -> URL? {
		guard let contentID else {
			return nil
		}
		if let url = storage.cachedContentURL(contentID), Self.exists(url) {
			storage.markContentUsed(url)
			return url
		}
		if let path = try context.store.stagedFile(contentID: contentID) {
			let url = storage.url(relativePath: path)
			if Self.exists(url) {
				return url
			}
		}
		return nil
	}

	/// The contents the item's row shows, if this device holds them: its staged local change, or a cached copy of the
	/// gateway's version
	func localCopy(of item: IndexItem) throws(IndexError) -> URL? {
		if let path = try context.store.remoteOverlay(itemID: item.id)?.stagedPath {
			let url = storage.url(relativePath: path)
			if Self.exists(url) {
				return url
			}
		}
		return try cachedCopy(item.blocksHash.map { ContentIdentity($0) })
	}

	static func exists(_ url: URL) -> Bool {
		FileManager.default.fileExists(atPath: url.path(percentEncoded: false))
	}

	/// Clones the file to the destination for the system (an APFS clone shares the blocks; elsewhere the data is
	/// copied). The copy is writable, and does not carry the system's attributes of another item along: staged files
	/// are read-only, and were cloned from the system's copy of the item they came from.
	static func handOut(_ source: URL, to destination: URL) throws {
		let target = destination.path(percentEncoded: false)
		guard copyfile(source.path(percentEncoded: false), target, nil, copyfile_flags_t(COPYFILE_CLONE)) == 0 else {
			throw posixError()
		}
		guard chmod(target, 0o644) == 0 else {
			let error = posixError()
			unlink(target)
			throw error
		}
		removeFileProviderAttributes(destination)
	}

	/// The system's attributes of an item, which must not travel with the contents to another one
	private static func removeFileProviderAttributes(_ url: URL) {
		let path = url.path(percentEncoded: false)
		let size = listxattr(path, nil, 0, XATTR_NOFOLLOW)
		guard size > 0 else {
			return
		}
		var buffer = [CChar](repeating: 0, count: size)
		guard listxattr(path, &buffer, size, XATTR_NOFOLLOW) == size else {
			return
		}
		for name in buffer.split(separator: 0).map({ String(decoding: $0.map { UInt8(bitPattern: $0) }, as: UTF8.self) })
		where name.hasPrefix("com.apple.fileprovider") {
			removexattr(path, name, XATTR_NOFOLLOW)
		}
	}

	/// The current POSIX error as an error the framework accepts (it only takes Cocoa and File Provider errors)
	private static func posixError(_ code: Int32 = errno) -> Error {
		switch code {
		case ENOSPC, EDQUOT:
			return CocoaError(.fileWriteOutOfSpace)
		case EACCES, EPERM:
			return CocoaError(.fileWriteNoPermission)
		case ENOENT:
			return CocoaError(.fileNoSuchFile)
		case ENAMETOOLONG, EINVAL, ENOTDIR, ELOOP, EISDIR:
			// Trying again would not help
			return RemoteFailure.cannotSynchronize(
				POSIXError(POSIXErrorCode(rawValue: code) ?? .EINVAL).localizedDescription)
		default:
			return NSError(
				domain: NSCocoaErrorDomain, code: NSXPCConnectionReplyInvalid,
				userInfo: [NSUnderlyingErrorKey: POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)])
		}
	}

	/// When the system reimports, it asks about the folders themselves; nothing else can be added at the top
	func existingFolderRoot(for template: NSFileProviderItem, options: NSFileProviderCreateItemOptions) throws
		-> IndexItem
	{
		let key = FileNames.collisionKey(template.filename)
		if options.contains(.mayAlreadyExist),
			let root = try context.store.children(of: nil, limit: ExtensionContext.maximumPageSize).first(where: {
				FileNames.collisionKey($0.name) == key
			})
		{
			return root
		}
		throw RemoteFailure.cannotSynchronize(String(localized: "Items can only be added inside a folder."))
	}

	/// Whether the file has the same bytes as a staged file (the SHA-256 of the whole file is its `digest`)
	static func hasContents(_ url: URL, of staged: StagedFile) -> Bool {
		guard let handle = try? FileHandle(forReadingFrom: url) else {
			return false
		}
		defer { try? handle.close() }
		guard (try? handle.seekToEnd()) == UInt64(staged.size), (try? handle.seek(toOffset: 0)) != nil else {
			return false
		}
		var hasher = SHA256()
		while true {
			let chunk: Data?
			do {
				chunk = try autoreleasepool { try handle.read(upToCount: 256 << 10) }
			}
			catch {
				return false
			}
			guard let chunk, !chunk.isEmpty else {
				break
			}
			hasher.update(data: chunk)
		}
		let digest = Data(hasher.finalize()).base64EncodedString().replacingOccurrences(of: "+", with: "-")
			.replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
		return digest == staged.digest
	}

	/// Removes staged files that nothing needs anymore (an unsent edit replaced by a newer one, a discarded change)
	func removeStagedFiles(_ paths: [String]) {
		if !paths.isEmpty {
			RemoteUploader.removeReleasedStagedFiles(paths, store: context.store, storage: storage)
		}
	}

	/// Removes a file that was staged but not recorded (an attempt that failed, or the repetition of a change that is
	/// recorded already)
	func discardStaged(_ staged: StagedFile?) {
		if let staged {
			removeStagedFiles([staged.relativePath])
		}
	}

	// MARK: - Contents

	/// The version of the file the item's row shows. Retryable errors when it is not known yet: a missing item is never
	/// assumed to be gone.
	private func contentSource(of item: IndexItem) throws -> RemoteContentSource {
		guard let source = try context.store.contentSource(of: item, storage: storage) else {
			refreshSoon(folderIDs: [item.folderID])
			throw RemoteFailure.versionChanged
		}
		return source
	}

	func fetchContents(
		of item: IndexItem, binding: RemoteFolderBinding, to destination: URL, request: NSFileProviderRequest,
		completionHandler: @escaping (URL?, NSFileProviderItem?, (any Error)?) -> Void
	) -> Progress {
		let progress = Progress(totalUnitCount: max(item.size, 1))
		let completion = Completion(call: completionHandler)
		let isSystemRequest = request.isSystemRequest
		let task = call { [self] in
			// The version the download asks for: a newer one the gateway answers with replaces only that
			let requested: RemoteRevision?
			if case .remote(let state)? = try? contentSource(of: item) {
				requested = state.revision
			}
			else {
				requested = nil
			}
			do {
				try Task.checkCancellation()
				let returned = try await fetch(item, binding: binding, to: destination, progress: progress)
				progress.completedUnitCount = progress.totalUnitCount
				trace("remote: fetched \(item.id) \(memoryFootprint())")
				completion.call(destination, returned, nil)
			}
			catch let failure as RemoteError {
				try? FileManager.default.removeItem(at: destination)
				let error = fetchFailure(
					failure, item: item, requested: requested, binding: binding, isSystemRequest: isSystemRequest,
					progress: progress)
				log.info("Could not fetch \(item.id): \(failure.summary)")
				trace("remote: fetch \(item.id) failed (system request: \(isSystemRequest)): \(failure.summary)")
				completion.call(nil, nil, error)
			}
			catch {
				try? FileManager.default.removeItem(at: destination)
				trace("remote: fetch \(item.id) failed: \(error)")
				let canceled = progress.isCancelled || error is CancellationError
				completion.call(nil, nil, canceled ? CocoaError(.userCancelled) : FileProviderExtension.reportable(error))
			}
		}
		progress.cancellationHandler = { task.cancel() }
		return progress
	}

	private func fetch(_ item: IndexItem, binding: RemoteFolderBinding, to destination: URL, progress: Progress)
		async throws -> NSFileProviderItem
	{
		let source = try contentSource(of: item)
		switch source {
		case .staged(let url):
			try Self.handOut(url, to: destination)
		case .remote(let state):
			if state.size == 0 {
				guard FileManager.default.createFile(atPath: destination.path(percentEncoded: false), contents: nil)
				else {
					throw CocoaError(.fileWriteUnknown)
				}
			}
			else if let cached = try cachedCopy(state.contentID) {
				try Self.handOut(cached, to: destination)
			}
			else {
				try Self.handOut(try await download(state, binding: binding, progress: progress), to: destination)
			}
		}
		// The item returned must describe these contents exactly; the index may have moved on meanwhile
		guard let current = try context.store.item(id: item.id), current.kind == .file,
			current.contentRevision == item.contentRevision, try contentSource(of: current).version == source.version
		else {
			throw RemoteFailure.versionChanged
		}
		return context.makeItemForContents(current, remote: binding)
	}

	/// Downloads the gateway's version into the cache (verified block by block, resuming from what an earlier attempt
	/// verified) and returns the cached copy
	func download(_ state: RemoteItemState, binding: RemoteFolderBinding, progress: Progress)
		async throws(RemoteError) -> URL
	{
		guard let client = client(serviceID: binding.serviceID), let request = RemoteDownloader.Request(state) else {
			throw .unauthenticated
		}
		let downloader = RemoteDownloader(
			store: context.store, client: client, storage: storage, scheduler: scheduler, credentials: credentials,
			owner: owner)
		let url = try await downloader.download(request) { verified, total in
			progress.totalUnitCount = max(total, 1)
			progress.completedUnitCount = verified
		}
		reached(serviceID: binding.serviceID)
		return url
	}

	private func fetchFailure(
		_ failure: RemoteError, item: IndexItem, requested: RemoteRevision?, binding: RemoteFolderBinding,
		isSystemRequest: Bool, progress: Progress
	) -> Error {
		if progress.isCancelled {
			return CocoaError(.userCancelled)
		}
		noteFailure(failure, serviceID: binding.serviceID)
		switch failure {
		case .notFound:
			// The gateway says the item is gone; the index learns it from the next refresh
			refreshSoon(folderIDs: [item.folderID])
			return NSError.fileProviderErrorForNonExistentItem(
				withIdentifier: NSFileProviderItemIdentifier(itemID: item.id))
		case .revisionMismatch(let current):
			// A newer version: store it, so that the system learns about it and asks again
			if let current,
				(try? context.store.applyRemoteItems(
					[current], serviceID: binding.serviceID, knownRevisions: requested.map { [current.id: $0] } ?? [:]))?
					.changed ?? 0 > 0
			{
				signalWorkingSet()
			}
			refreshSoon(folderIDs: [item.folderID])
			return RemoteFailure.versionChanged
		case .unauthenticated:
			reported(.notAuthenticated, serviceID: binding.serviceID)
		case .unreachable where !isSystemRequest:
			reported(.serverUnreachable, serviceID: binding.serviceID)
		default:
			break
		}
		return RemoteFailure.download(failure, gateway: gatewayName(binding), isSystemRequest: isSystemRequest)
	}

	// MARK: - Thumbnails

	/// The gateway's thumbnail of the file's current revision, when the gateway renders that type (nil when it has
	/// none). Never for a local change the gateway has not seen. Throws right away while the gateway could not be used
	/// recently, rather than waiting for it again.
	func gatewayThumbnail(of item: IndexItem, binding: RemoteFolderBinding, type: UTType, maximumPixels: CGFloat)
		async throws -> Data?
	{
		let serviceID = binding.serviceID
		guard item.pending == .none, let feature = Capabilities.thumbnailFeature(for: type),
			let state = try context.store.remoteItem(itemID: item.id), state.kind == .file, !state.isDeleted,
			state.contentID?.rawValue == item.blocksHash
		else {
			return nil
		}
		guard !failedRecently(serviceID: serviceID) else {
			throw RemoteError.unreachable(.cannotConnectToHost)
		}
		guard let capabilities = await capabilities(serviceID: serviceID), capabilities.apiVersion == Self.apiVersion,
			capabilities.supports(feature), let client = client(serviceID: serviceID)
		else {
			return nil
		}
		let destination = FileManager.default.temporaryDirectory.appending(path: "thumbnail-\(UUID().uuidString)")
		defer { try? FileManager.default.removeItem(at: destination) }
		let size = min(max(Int(maximumPixels.rounded()), 16), 1024)
		do throws(RemoteError) {
			try await scheduler.withSlot(.thumbnail) { () async throws(RemoteError) in
				try await client.thumbnail(itemID: state.gatewayID, revision: state.revision, size: size, to: destination)
			}
		}
		catch .notFound {
			return nil
		}
		catch .revisionMismatch {
			refreshSoon(folderIDs: [item.folderID])
			throw RemoteError.revisionMismatch(current: nil)
		}
		catch {
			noteFailure(error, serviceID: serviceID)
			throw error
		}
		reached(serviceID: serviceID)
		// Bounded by the gateway; anything larger is not a thumbnail
		let attributes = try FileManager.default.attributesOfItem(atPath: destination.path(percentEncoded: false))
		guard let length = (attributes[.size] as? NSNumber)?.intValue, length > 0, length <= 4 << 20 else {
			return nil
		}
		return try Data(contentsOf: destination)
	}

	// MARK: - Actions

	/// "Keep on This Device" asks the system to download the files; "Stop Keeping" removes the system's copy and the
	/// cached one. Neither changes anything on the gateway, and an item with a local change the gateway has not
	/// confirmed is never evicted.
	func perform(keep: Bool, on items: [IndexItem]) async throws {
		guard let manager else {
			throw NSFileProviderError(.serverUnreachable)
		}
		for item in items where item.kind == .file {
			let identifier = NSFileProviderItemIdentifier(itemID: item.id)
			if keep {
				try await manager.requestDownloadForItem(withIdentifier: identifier)
				continue
			}
			guard item.pending == .none, try context.store.remoteOverlay(itemID: item.id) == nil else {
				throw NSFileProviderError(.unsyncedEdits)
			}
			try await manager.evictItem(identifier: identifier)
			// The cached copies of the version go as well. That includes a staged edit the gateway committed as it (no
			// unresolved change refers to it: the item has no overlay); the gateway has those bytes.
			if let contentID = item.blocksHash.map({ ContentIdentity($0) }) {
				if let url = storage.cachedContentURL(contentID) {
					try? FileManager.default.removeItem(at: url)
				}
				if let path = try context.store.stagedFile(contentID: contentID) {
					removeStagedFiles([path])
				}
			}
			// The system keeps the item as it was last returned, which says the file is on this device (and offers to
			// stop keeping it rather than to keep it). Its row does not change, so the working set would not report it:
			// asking the system to modify a field only it keeps makes it take the item again (`modifyItem` returns it).
			do {
				try await manager.requestModification(of: [.lastUsedDate], forItemWithIdentifier: identifier, options: [])
			}
			catch {
				log.info("Could not have the system update \(item.id) after evicting it: \(error.localizedDescription)")
			}
		}
		trace("remote: \(keep ? "keep" : "stop keeping") \(items.map(\.id))")
	}
}
