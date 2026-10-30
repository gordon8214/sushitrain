// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Browses the previous versions a gateway keeps of a file in a gateway folder, and writes them back: restores the file
/// to a version, or saves a copy of one next to it. Both happen on the gateway, through operations that copy the version
/// there, so this device never uploads anything. Used by the Files app's Previous Versions action.
public struct RemoteVersionBrowser: Sendable {
	/// Why versions cannot be shown or written
	public enum Failure: Error, Sendable, Equatable {
		/// The item is not (or no longer) a file in a folder accessed through a gateway, or its folder is being switched
		case notAvailable
		/// The gateway is not paired with this device
		case notPaired
		/// The gateway cannot list versions (it needs an update)
		case unsupported
		/// The gateway speaks a newer version of the remote file API (this device needs an update)
		case outdated
		/// The file has changes on this device that the gateway has not confirmed yet (for a new file: it has not been
		/// uploaded yet)
		case pendingChange
		/// This device may not change the folder
		case readOnly
		/// The gateway accepted the restore or copy, but had not carried it out when this device stopped waiting
		case outcomeUnknown
		/// The gateway refused, failed or could not be reached
		case remote(RemoteError)
		/// The local index failed
		case store(IndexError)
	}

	/// The file as the index has it now
	public struct File: Sendable, Hashable {
		public var item: IndexItem
		public var remote: RemoteItemState
	}

	/// The version of the remote file API this client speaks
	public static let apiVersion = 1
	/// How many numbered names a copy tries when the gateway reports the name taken
	static let maximumCopyNames = 5
	/// The longest name (in bytes) file systems accept
	static let maximumNameBytes = 255

	public let store: IndexStore
	public let client: RemoteFileClient
	public let itemID: Int64
	public let binding: RemoteFolderBinding
	/// How long to wait before asking again for the state of an operation, or after a failed request, by attempt
	public var pollDelay: @Sendable (Int) -> Duration = { attempt in .milliseconds(min(250 << min(attempt, 4), 4_000)) }
	/// How long to wait for the gateway to carry out a restore or a copy
	public var operationDeadline = Duration.seconds(90)

	/// Resolves an item (an `items` row) to its gateway and folder
	/// - Parameter makeClient: the client of the gateway's endpoint
	public init(store: IndexStore, itemID: Int64, makeClient: (RemoteEndpoint) -> RemoteFileClient) throws(Failure) {
		let row: IndexItem?
		let binding: RemoteFolderBinding?
		let endpoint: RemoteEndpoint?
		do {
			row = try store.item(id: itemID)
			binding = try row.flatMap { item throws(IndexError) in try store.remoteBinding(folderID: item.folderID) }
			endpoint = try binding.flatMap { binding throws(IndexError) in
				try store.remoteEndpoint(serviceID: binding.serviceID)
			}
		}
		catch {
			throw .store(error)
		}
		guard let row, row.kind == .file, let binding else {
			throw .notAvailable
		}
		guard let endpoint else {
			throw .notPaired
		}
		self.store = store
		self.itemID = itemID
		self.binding = binding
		client = makeClient(endpoint)
	}

	/// Whether this device may change the folder
	public var canWrite: Bool { binding.permissions.write }

	/// The file's row and its confirmed state on the gateway, as the index has them now
	public func file() throws(Failure) -> File {
		let item: IndexItem?
		let remote: RemoteItemState?
		do {
			item = try store.item(id: itemID)
			remote = try store.remoteItem(itemID: itemID)
		}
		catch {
			throw .store(error)
		}
		guard let item, !item.isDeleted else {
			throw .notAvailable
		}
		guard let remote, !remote.isDeleted, remote.kind == .file else {
			throw item.pending == .create ? .pendingChange : .notAvailable
		}
		return File(item: item, remote: remote)
	}

	/// The versions the gateway keeps of the file, newest first
	public func versions() async throws(Failure) -> RemoteVersionList {
		let remote = try file().remote
		let capabilities: Capabilities
		do {
			capabilities = try await client.capabilities()
		}
		catch {
			throw .remote(error)
		}
		guard capabilities.serviceID == client.endpoint.serviceID else {
			throw .remote(.protocolViolation("the gateway's service ID changed"))
		}
		if capabilities.apiVersion > Self.apiVersion {
			throw .outdated
		}
		guard capabilities.apiVersion == Self.apiVersion, capabilities.supports(.versions) else {
			throw .unsupported
		}
		let list: RemoteVersionList
		do {
			list = try await client.versions(itemID: remote.gatewayID)
		}
		catch {
			throw .remote(error)
		}
		guard list.itemID == remote.gatewayID, list.versions.allSatisfy({ Self.isVersionID($0.id) }) else {
			throw .remote(.protocolViolation("the gateway listed versions of another file, or with malformed IDs"))
		}
		return list
	}

	/// Brings the folder's metadata up to date, for instance after the gateway reported that the file changed
	public func refresh() async throws(Failure) {
		do {
			try await RemoteSync(store: store, client: client).refresh(folderID: binding.folderID)
		}
		catch {
			throw .remote(error)
		}
	}

	/// Whether a version ID has the documented form (`ver_` and the Unix time the version was archived). IDs name
	/// directories of downloads, so others are refused.
	static func isVersionID(_ id: String) -> Bool {
		id.wholeMatch(of: /ver_-?[0-9]{1,19}/) != nil
	}

	/// The versions worth showing: each whose modification time (to the second) or size differs from the next newer
	/// one's, the newest compared with the current file. Others hold the same content almost certainly: Syncthing
	/// archives a version when only a file's metadata changes, such as its extended attributes.
	public static func distinct(_ versions: [RemoteVersion], currentModifiedNs: Int64, currentSize: Int64)
		-> [RemoteVersion]
	{
		var newer = (seconds: currentModifiedNs / 1_000_000_000, size: currentSize)
		var shown: [RemoteVersion] = []
		for version in versions {
			let this = (seconds: version.modifiedNs / 1_000_000_000, size: version.size)
			if this != newer {
				shown.append(version)
			}
			newer = this
		}
		return shown
	}

	/// Downloads a version into the directory (in a subdirectory of the file and the version, under the file's name, so
	/// that previews show the name and recognize the type), or returns the earlier download
	public func download(_ version: RemoteVersion, into directory: URL) async throws(Failure) -> URL {
		let file = try file()
		guard Self.isVersionID(version.id), FileNames.isValid(file.remote.gatewayID) else {
			throw .remote(.protocolViolation("malformed version or item ID"))
		}
		let folder = directory.appending(path: file.remote.gatewayID, directoryHint: .isDirectory)
			.appending(path: version.id, directoryHint: .isDirectory)
		let destination = folder.appending(path: file.item.name, directoryHint: .notDirectory)
		let attributes = try? FileManager.default.attributesOfItem(atPath: destination.path(percentEncoded: false))
		if let size = (attributes?[.size] as? NSNumber)?.int64Value, size == version.size {
			return destination
		}
		do {
			try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
		}
		catch {
			throw .remote(.fileSystem("could not make a directory for a version: \(error.localizedDescription)"))
		}
		do {
			try await client.downloadVersion(itemID: file.remote.gatewayID, versionID: version.id, to: destination)
		}
		catch {
			throw .remote(error)
		}
		return destination
	}

	/// Restores the file to the version. The gateway archives the current content as a version. A file that changed on
	/// the gateway since this device last saw it is not overwritten (`.remote(.revisionMismatch)`).
	@discardableResult
	public func restore(_ version: RemoteVersion) async throws(Failure) -> RemoteOperation {
		let file = try file()
		try checkWritable()
		guard file.item.pending == .none else {
			throw .pendingChange
		}
		return try await perform(
			.restore(
				id: Self.newOperationID(), folderID: binding.remoteFolderID, itemID: file.remote.gatewayID,
				baseRevision: file.remote.revision, version: version.id))
	}

	/// Saves a copy of the version next to the file on the gateway, under a name no item there has
	@discardableResult
	public func saveCopy(of version: RemoteVersion) async throws(Failure) -> RemoteOperation {
		let file = try file()
		try checkWritable()
		guard let parentID = file.remote.parentGatewayID else {
			throw .notAvailable
		}
		// Named and placed as on the gateway, which a rename on this device may not have reached yet
		let parentRow: Int64?
		do {
			parentRow = try store.remoteItem(gatewayID: parentID)?.itemID
		}
		catch {
			throw .store(error)
		}
		var taken = try siblingNames(of: parentRow ?? file.item.parentID)
		for _ in 0..<Self.maximumCopyNames {
			let name = Self.copyName(of: file.remote.name, version: version, taken: taken)
			do {
				return try await perform(
					.copyVersion(
						id: Self.newOperationID(), folderID: binding.remoteFolderID, parentID: parentID, name: name,
						sourceItemID: file.remote.gatewayID, version: version.id))
			}
			catch .remote(.nameConflict) {
				// A name this device doesn't know of yet
				taken.insert(FileNames.collisionKey(name))
			}
		}
		throw .remote(.nameConflict(current: nil))
	}

	/// The name of a copy of a version: "Name (2026-10-20 15.56).ext", numbered when an item has it, and shortened to
	/// fit the longest name file systems accept
	/// - Parameter taken: the collision keys (`FileNames.collisionKey`) of the names in the directory
	public static func copyName(
		of name: String, version: RemoteVersion, taken: Set<String>, timeZone: TimeZone = .current
	) -> String {
		let formatter = DateFormatter()
		formatter.locale = Locale(identifier: "en_US_POSIX")
		formatter.timeZone = timeZone
		formatter.dateFormat = "yyyy-MM-dd HH.mm"
		let stamp = formatter.string(from: version.modificationDate)
		let ext = (name as NSString).pathExtension
		var stem = ext.isEmpty ? name : (name as NSString).deletingPathExtension
		let suffix = ext.isEmpty ? "" : "." + ext
		// Room for a number too
		while "\(stem) (\(stamp)) \(maximumCopyNames + 1)\(suffix)".utf8.count > maximumNameBytes, !stem.isEmpty {
			stem.removeLast()
		}
		var candidate = "\(stem) (\(stamp))\(suffix)"
		var number = 2
		while taken.contains(FileNames.collisionKey(candidate)) {
			candidate = "\(stem) (\(stamp)) \(number)\(suffix)"
			number += 1
		}
		return candidate
	}

	// MARK: - Operations

	/// Submits the operation and waits until the gateway has carried it out, then brings the folder's metadata up to
	/// date, so that the Files app shows the result as soon as it asks. Requests that fail on the way (the network, a
	/// busy gateway) are repeated: the same request again gets the gateway's answer to the first.
	func perform(_ request: OperationRequest) async throws(Failure) -> RemoteOperation {
		let body: Data
		do {
			body = try request.canonicalJSON()
		}
		catch {
			throw .remote(.protocolViolation("the operation cannot be encoded"))
		}
		let deadline = ContinuousClock.now + operationDeadline
		var operation: RemoteOperation?
		var attempt = 0
		var retryAfter: TimeInterval?
		while (operation?.state ?? .pending) == .pending {
			if attempt > 0 {
				try await pause(attempt: attempt, retryAfter: retryAfter, until: deadline)
			}
			attempt += 1
			do {
				if let pending = operation {
					operation = try await client.operation(id: pending.operation)
				}
				else {
					operation = try await client.submitOperation(body)
				}
				retryAfter = nil
			}
			catch  where error.isTransient {
				retryAfter = error.retryAfter
				continue
			}
			catch {
				throw .remote(error)
			}
			if operation?.id != request.id {
				throw .remote(.protocolViolation("the gateway answered for another operation"))
			}
		}
		guard let operation else {
			throw .outcomeUnknown
		}
		switch operation.state {
		case .committed:
			try? await refresh()
			return operation
		case .conflict:
			// A rejected conflict is reported as failed; this is defensive
			throw .remote(.revisionMismatch(current: operation.item))
		default:
			throw .remote(Self.error(of: operation))
		}
	}

	/// Waits before the next request about an operation, unless that would pass the deadline
	private func pause(attempt: Int, retryAfter: TimeInterval?, until deadline: ContinuousClock.Instant)
		async throws(Failure)
	{
		var delay = pollDelay(attempt)
		if let retryAfter {
			delay = max(delay, .seconds(retryAfter))
		}
		guard ContinuousClock.now + delay < deadline else {
			throw .outcomeUnknown
		}
		do {
			try await Task.sleep(for: delay)
		}
		catch {
			throw .remote(.canceled)
		}
	}

	/// The error a failed operation reports
	static func error(of operation: RemoteOperation) -> RemoteError {
		guard let detail = operation.error else {
			return .server(status: 500, code: nil)
		}
		let retryAfter = detail.retryAfter.map(TimeInterval.init)
		switch detail.code {
		case RemoteErrorCode.versionNotFound: return .versionNotFound
		case RemoteErrorCode.notFound: return .notFound
		case RemoteErrorCode.nameConflict: return .nameConflict(current: detail.item)
		case RemoteErrorCode.revisionMismatch: return .revisionMismatch(current: detail.item)
		case RemoteErrorCode.forbidden, RemoteErrorCode.folderReadOnly, RemoteErrorCode.reservedName,
			RemoteErrorCode.ignoredPath:
			return .forbidden(code: detail.code)
		case RemoteErrorCode.insufficientStorage: return .insufficientStorage
		case RemoteErrorCode.contentUnavailable: return .unavailable(retryAfter: retryAfter)
		case RemoteErrorCode.overloaded: return .overloaded(retryAfter: retryAfter)
		case RemoteErrorCode.operationMismatch: return .operationMismatch
		case RemoteErrorCode.invalidRequest, RemoteErrorCode.uploadMismatch: return .server(status: 400, code: detail.code)
		default: return .server(status: 500, code: detail.code)
		}
	}

	/// Refuses writes in folders this device may only read
	private func checkWritable() throws(Failure) {
		guard canWrite else {
			throw .readOnly
		}
	}

	private func siblingNames(of parentID: Int64?) throws(Failure) -> Set<String> {
		var names: Set<String> = []
		var after: Int64 = 0
		do {
			while true {
				let page = try store.children(of: parentID, after: after, limit: 500)
				names.formUnion(page.map { FileNames.collisionKey($0.name) })
				guard let last = page.last, page.count == 500 else {
					return names
				}
				after = last.id
			}
		}
		catch {
			throw .store(error)
		}
	}

	private static func newOperationID() -> String {
		UUID().uuidString.lowercased()
	}
}
