// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import OSLog
import SynctrainFileProviderKit

let log = Logger(subsystem: "nl.t-shaped.Sushitrain.FileProvider", category: "Extension")

/// The memory the extension uses, as the system counts it against its limit (debug builds only)
func memoryFootprint() -> String {
	#if DEBUG
		var info = task_vm_info_data_t()
		var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
		let result = withUnsafeMutablePointer(to: &info) { pointer in
			pointer.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
				task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count)
			}
		}
		return result == KERN_SUCCESS ? "(footprint \(info.phys_footprint / 1_048_576) MB)" : ""
	#else
		return ""
	#endif
}

/// Debug builds also append what the extension does to a file in the app group's caches. Unlike the system log, which
/// needs root to collect from a device, `xcrun devicectl device copy from --domain-type appGroupDataContainer` can read
/// it.
func trace(_ message: @autoclosure () -> String) {
	#if DEBUG
		guard let container = SharedContainer.appGroup() else {
			return
		}
		let url = container.baseURL.appending(path: "Library/Caches/FileProviderExtension.log")
		let line = "\(Date().formatted(.iso8601.time(includingFractionalSeconds: true))) \(message())\n"
		if let handle = try? FileHandle(forWritingTo: url) {
			_ = try? handle.seekToEnd()
			try? handle.write(contentsOf: Data(line.utf8))
			try? handle.close()
		}
		else {
			try? Data(line.utf8).write(to: url)
		}
	#endif
}

/// Lets non-Sendable completion handlers from the framework cross into tasks. The framework allows calling them from
/// any thread; each one is called exactly once.
struct Completion<T>: @unchecked Sendable {
	let call: T
}

/// Serves the Synctrain location in the Files app from the shared index that the app maintains. This process must
/// stay small (the system terminates File Provider extensions on iOS that use more than about 20 MB), so it does not
/// run Syncthing: files on this device are cloned directly, other files are fetched through the app while it runs.
/// Folders accessed through a gateway are served by `RemoteProvider` instead, which talks to the gateway itself.
final class FileProviderExtension: NSObject, NSFileProviderReplicatedExtension {
	let domain: NSFileProviderDomain
	let context: ExtensionContext?
	let fetcher: ContentFetcher?
	private let writer: LocalWriter?
	let remote: RemoteProvider?

	required init(domain: NSFileProviderDomain) {
		self.domain = domain
		self.context = ExtensionContext()
		self.fetcher = context.map { ContentFetcher(context: $0) }
		self.writer = context.map { LocalWriter(context: $0) }
		self.remote = context.map { RemoteProvider(context: $0, domain: domain) }
		super.init()
		remote?.start()
		if let context {
			// Left behind when an earlier instance was terminated in the middle of a change or a download
			LocalWriter.removeLeftovers(in: context.container.stagingURL)
			if let temporary = try? NSFileProviderManager(for: domain)?.temporaryDirectoryURL() {
				LocalWriter.removeLeftovers(in: temporary)
			}
		}
		else {
			log.error("The app group container is not available")
		}
	}

	func invalidate() {
		fetcher?.invalidate()
		remote?.invalidate()
	}

	private var unavailableError: Error {
		NSFileProviderError(.serverUnreachable)
	}

	// MARK: - Items

	func item(
		for identifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, (any Error)?) -> Void
	) -> Progress {
		if identifier == .rootContainer {
			completionHandler(RootItem(), nil)
			return Progress()
		}
		guard let context else {
			completionHandler(nil, unavailableError)
			return Progress()
		}
		guard let id = identifier.itemID else {
			completionHandler(nil, NSError.fileProviderErrorForNonExistentItem(withIdentifier: identifier))
			return Progress()
		}

		do {
			guard let item = try context.store.item(id: id) else {
				completionHandler(nil, NSError.fileProviderErrorForNonExistentItem(withIdentifier: identifier))
				return Progress()
			}
			completionHandler(try context.makeItem(item), nil)
		}
		catch {
			completionHandler(nil, error.fileProviderError)
		}
		return Progress()
	}

	func enumerator(for containerItemIdentifier: NSFileProviderItemIdentifier, request: NSFileProviderRequest) throws
		-> NSFileProviderEnumerator
	{
		guard let context else {
			throw unavailableError
		}
		switch containerItemIdentifier {
		case .workingSet:
			return WorkingSetEnumerator(context: context)
		case .rootContainer:
			remote?.refreshSoon()
			return ContainerEnumerator(context: context, parentID: nil)
		case .trashContainer:
			// There is no trash: deleting removes items on all devices
			throw CocoaError(.featureUnsupported)
		default:
			let found: IndexItem?
			do {
				found = try containerItemIdentifier.itemID.map { try context.store.item(id: $0) } ?? nil
			}
			catch let error as IndexError {
				// Not knowing is not the same as not existing: the system would delete the item
				throw error.fileProviderError
			}
			guard let item = found, item.kind.isContainer else {
				throw NSError.fileProviderErrorForNonExistentItem(withIdentifier: containerItemIdentifier)
			}
			// Only folders accessed through a gateway are refreshed; this never waits for it
			remote?.refreshSoon(folderIDs: [item.folderID])
			return ContainerEnumerator(context: context, parentID: item.id)
		}
	}

	// MARK: - Contents

	func fetchContents(
		for itemIdentifier: NSFileProviderItemIdentifier, version requestedVersion: NSFileProviderItemVersion?,
		request: NSFileProviderRequest, completionHandler: @escaping (URL?, NSFileProviderItem?, (any Error)?) -> Void
	) -> Progress {
		guard let context, let fetcher else {
			completionHandler(nil, nil, unavailableError)
			return Progress()
		}

		let item: IndexItem
		let destination: URL
		do {
			guard let id = itemIdentifier.itemID, let found = try context.store.item(id: id), found.kind == .file else {
				completionHandler(nil, nil, NSError.fileProviderErrorForNonExistentItem(withIdentifier: itemIdentifier))
				return Progress()
			}
			item = found
			guard let manager = NSFileProviderManager(for: domain) else {
				completionHandler(nil, nil, unavailableError)
				return Progress()
			}
			destination = try manager.temporaryDirectoryURL().appending(path: UUID().uuidString)
		}
		catch let error as IndexError {
			completionHandler(nil, nil, error.fileProviderError)
			return Progress()
		}
		catch {
			completionHandler(nil, nil, error)
			return Progress()
		}

		// Folders accessed through a gateway never use the folder tree or the app
		do {
			if let remote, let binding = try context.remoteBinding(folderID: item.folderID) {
				return remote.fetchContents(
					of: item, binding: binding, to: destination, request: request, completionHandler: completionHandler)
			}
		}
		catch {
			completionHandler(nil, nil, error.fileProviderError)
			return Progress()
		}

		// The version on this device, if it is current: no download, no extra space (the clone shares its blocks)
		if fetcher.cloneLocal(item, to: destination) {
			completionHandler(destination, context.makeItemForContents(item), nil)
			return Progress()
		}

		if item.size == 0 {
			if FileManager.default.createFile(atPath: destination.path(percentEncoded: false), contents: nil) {
				completionHandler(destination, context.makeItemForContents(item), nil)
			}
			else {
				completionHandler(nil, nil, NSError(domain: NSCocoaErrorDomain, code: NSFileWriteUnknownError))
			}
			return Progress()
		}

		let progress = Progress(totalUnitCount: max(item.size, 1))
		let completion = Completion(call: completionHandler)
		let isSystemRequest = request.isSystemRequest
		Task {
			do {
				guard let heartbeat = fetcher.awakeApp(), await fetcher.ping(heartbeat) else {
					throw FetchError.appNotRunning
				}
				var fetched = item
				if let served = try await fetcher.download(item, from: heartbeat, to: destination, progress: progress) {
					// The contents must match the item returned with them; the index may have caught up meanwhile
					guard let current = try? context.store.item(id: item.id), served.isVersion(of: current) else {
						try? FileManager.default.removeItem(at: destination)
						throw FetchError.versionChanged
					}
					fetched = current
				}
				// If the user asked for it while the app was not running, the app no longer needs to list it
				try? context.store.completeWork(.fetch, extra: ItemIdentifiers.string(for: item.id))
				completion.call(destination, context.makeItemForContents(fetched), nil)
			}
			catch let error as FetchError {
				if case .appNotRunning = error, !isSystemRequest {
					// Remember the request, so the app can have it retried once it runs
					try? context.store.enqueue(
						.fetch, folderID: item.folderID, path: item.path, extra: ItemIdentifiers.string(for: item.id))
					SharedNotification.workQueued.post()
				}
				log.info("Could not fetch \(item.id): \(error.localizedDescription)")
				trace("fetch \(item.id) failed (system request: \(isSystemRequest)): \(error.localizedDescription)")
				completion.call(nil, nil, error.fileProviderError(isSystemRequest: isSystemRequest))
			}
			catch {
				log.warning("Could not fetch \(item.id): \(error.localizedDescription)")
				completion.call(nil, nil, progress.isCancelled ? CocoaError(.userCancelled) : Self.reportable(error))
			}
		}
		return progress
	}

	// MARK: - Changes

	/// Fields only the system keeps; Syncthing does not sync them, so changing them needs no work on disk (tags, the
	/// favorite rank and the last used date are stored in the index)
	static let systemFields: NSFileProviderItemFields = [
		.lastUsedDate, .tagData, .favoriteRank, .extendedAttributes, .typeAndCreator, .fileSystemFlags, .creationDate,
	]

	func createItem(
		basedOn itemTemplate: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?,
		options: NSFileProviderCreateItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		guard let context, let writer else {
			completionHandler(nil, [], false, unavailableError)
			return Progress()
		}
		do {
			if let remote, let parentID = itemTemplate.parentItemIdentifier.itemID,
				let parent = try context.store.item(id: parentID),
				let binding = try context.remoteBinding(folderID: parent.folderID)
			{
				return remote.createItem(
					basedOn: itemTemplate, fields: fields, contents: url, options: options, parent: parent,
					binding: binding, completionHandler: completionHandler)
			}
		}
		catch {
			completionHandler(nil, [], false, Self.reportable(error))
			return Progress()
		}
		do {
			let item = try writer.create(template: itemTemplate, fields: fields, contents: url, options: options)
			try context.saveAttributes(of: itemTemplate, fields: fields, itemID: item.id)
			trace("create \(itemTemplate.filename) in \(itemTemplate.parentItemIdentifier.rawValue) -> \(item.id)")
			completionHandler(try context.makeItem(item), [], false, nil)
		}
		catch {
			trace("create \(itemTemplate.filename) failed: \(error)")
			log.info("Could not create \(itemTemplate.filename, privacy: .private): \(error.localizedDescription)")
			completionHandler(nil, [], false, Self.reportable(error))
		}
		return Progress()
	}

	func modifyItem(
		_ item: NSFileProviderItem, baseVersion version: NSFileProviderItemVersion,
		changedFields: NSFileProviderItemFields, contents newContents: URL?,
		options: NSFileProviderModifyItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		if item.itemIdentifier == .rootContainer {
			completionHandler(RootItem(), [], false, nil)
			return Progress()
		}
		guard let context, let writer else {
			completionHandler(nil, [], false, unavailableError)
			return Progress()
		}
		do {
			if let remote, let id = item.itemIdentifier.itemID, let stored = try context.store.item(id: id) {
				if let binding = try context.remoteBinding(folderID: stored.folderID) {
					return remote.modifyItem(
						item, baseVersion: version, changedFields: changedFields, contents: newContents,
						options: options, item: stored, binding: binding, completionHandler: completionHandler)
				}
				// Into a folder accessed through a gateway: Syncthing on this device cannot move an item there
				if changedFields.contains(.parentItemIdentifier), let parentID = item.parentItemIdentifier.itemID,
					let parent = try context.store.item(id: parentID), parent.folderID != stored.folderID,
					try context.remoteBinding(folderID: parent.folderID) != nil
				{
					throw RemoteFailure.acrossEngines
				}
			}
		}
		catch {
			trace("modify \(item.itemIdentifier.rawValue) fields \(changedFields.rawValue) failed: \(error)")
			completionHandler(nil, [], false, Self.reportable(error))
			return Progress()
		}
		do {
			var outcome: LocalWriter.Outcome
			if changedFields.isSubset(of: Self.systemFields) {
				// Opening a file updates its last used date, for instance
				guard let id = item.itemIdentifier.itemID, let stored = try context.store.item(id: id) else {
					throw NSFileProviderError(.noSuchItem)
				}
				outcome = LocalWriter.Outcome(item: stored)
			}
			else {
				outcome = try writer.modify(
					template: item, baseVersion: version, changedFields: changedFields, contents: newContents,
					options: options)
			}

			try context.saveAttributes(of: item, fields: changedFields, itemID: outcome.item.id)
			let current = try context.makeItem(outcome.item)
			trace(
				"modify \(item.itemIdentifier.rawValue) fields \(changedFields.rawValue) -> \(outcome.item.path) fetch \(outcome.shouldFetchContent)"
			)
			completionHandler(current, [], outcome.shouldFetchContent, nil)
			if outcome.addedItems {
				signalWorkingSet()
			}
		}
		catch {
			trace("modify \(item.itemIdentifier.rawValue) fields \(changedFields.rawValue) failed: \(error)")
			log.info("Could not modify \(item.itemIdentifier.rawValue): \(error.localizedDescription)")
			completionHandler(nil, [], false, Self.reportable(error))
		}
		return Progress()
	}

	func deleteItem(
		identifier: NSFileProviderItemIdentifier, baseVersion version: NSFileProviderItemVersion,
		options: NSFileProviderDeleteItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		guard let writer else {
			completionHandler(unavailableError)
			return Progress()
		}
		do {
			if let remote, let binding = try remote.binding(ofDeleted: identifier) {
				return remote.deleteItem(
					identifier: identifier, baseVersion: version, options: options, binding: binding,
					completionHandler: completionHandler)
			}
		}
		catch {
			completionHandler(Self.reportable(error))
			return Progress()
		}
		do {
			let removedDescendants = try writer.delete(identifier: identifier, baseVersion: version, options: options)
			trace("delete \(identifier.rawValue)")
			completionHandler(nil)
			if removedDescendants {
				// The system learns about the items inside from the working set
				signalWorkingSet()
			}
		}
		catch {
			trace("delete \(identifier.rawValue) failed: \(error)")
			log.info("Could not delete \(identifier.rawValue): \(error.localizedDescription)")
			completionHandler(Self.reportable(error))
		}
		return Progress()
	}

	/// Tells the system to fetch changes, for items it does not learn about from the call it made
	private func signalWorkingSet() {
		NSFileProviderManager(for: domain)?.signalEnumerator(for: .workingSet) { error in
			if let error {
				log.warning("Could not signal the working set: \(error.localizedDescription)")
			}
		}
	}

	/// The framework only accepts Cocoa and File Provider errors
	static func reportable(_ error: Error) -> Error {
		if let error = error as? IndexError {
			return error.fileProviderError
		}
		let nsError = error as NSError
		if nsError.domain == NSFileProviderErrorDomain || nsError.domain == NSCocoaErrorDomain {
			return error
		}
		return NSError(
			domain: NSCocoaErrorDomain, code: NSXPCConnectionReplyInvalid, userInfo: [NSUnderlyingErrorKey: nsError])
	}
}

// MARK: - Actions

/// "Keep on This Device" and "Stop Keeping on This Device" for items in on-demand folders. Only the app can change what
/// Syncthing keeps, so the extension queues the change; the app carries it out when it runs (stopping only once
/// another device has the whole item, so nothing is lost).
extension FileProviderExtension: NSFileProviderCustomAction {
	private static let actionPrefix = (Bundle.main.bundleIdentifier ?? "") + "."

	func performAction(
		identifier actionIdentifier: NSFileProviderExtensionActionIdentifier,
		onItemsWithIdentifiers itemIdentifiers: [NSFileProviderItemIdentifier],
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		guard let context else {
			completionHandler(unavailableError)
			return Progress()
		}
		let kind: WorkKind
		switch actionIdentifier.rawValue {
		case Self.actionPrefix + "keep":
			kind = .select
		case Self.actionPrefix + "stopKeeping":
			kind = .deselectSafely
		default:
			completionHandler(CocoaError(.featureUnsupported))
			return Progress()
		}

		do {
			var items: [IndexItem] = []
			var remoteItems: [IndexItem] = []
			for identifier in itemIdentifiers {
				if let id = identifier.itemID, let item = try context.store.item(id: id) {
					if remote != nil, try context.remoteBinding(folderID: item.folderID) != nil {
						remoteItems.append(item)
					}
					else {
						items.append(item)
					}
				}
			}
			if let remote, !remoteItems.isEmpty {
				return perform(kind, legacy: items, remote: remoteItems, with: remote, completionHandler: completionHandler)
			}
			try Self.queue(kind, for: items, in: context)
			trace("action \(actionIdentifier.rawValue) on \(items.map(\.id))")
			SharedNotification.workQueued.post()
			completionHandler(nil)
		}
		catch {
			completionHandler(Self.reportable(error))
		}
		return Progress()
	}
}

extension FileProviderExtension {
	/// Queues the change of what Syncthing keeps for the items, for the app
	fileprivate static func queue(_ kind: WorkKind, for items: [IndexItem], in context: ExtensionContext)
		throws(IndexError)
	{
		try context.store.recordLocalChanges { recorder throws(IndexError) in
			for item in items where item.kind != .folderRoot {
				// The latest choice wins, except that new items are still selected: that is how Syncthing gets
				// them at all (the app selects before it deselects, and only deselects what another device has)
				if kind == .select || (item.pending != .create && item.pending != .rename) {
					try recorder.dequeue(
						kind == .select ? .deselectSafely : .select, folderID: item.folderID, path: item.path)
				}
				try recorder.enqueue(kind, folderID: item.folderID, path: item.path)
			}
		}
	}

	/// Carries out an action on a selection with items of folders accessed through a gateway (for which it changes only
	/// what this device keeps), and queues it for the others as usual
	fileprivate func perform(
		_ kind: WorkKind, legacy items: [IndexItem], remote remoteItems: [IndexItem], with remote: RemoteProvider,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		do {
			if let context, !items.isEmpty {
				try Self.queue(kind, for: items, in: context)
				SharedNotification.workQueued.post()
			}
		}
		catch {
			completionHandler(Self.reportable(error))
			return Progress()
		}
		let completion = Completion(call: completionHandler)
		let task = Task {
			do {
				try await remote.perform(keep: kind == .select, on: remoteItems)
				completion.call(nil)
			}
			catch {
				completion.call(Self.reportable(error))
			}
		}
		let progress = Progress(totalUnitCount: Int64(remoteItems.count))
		progress.cancellationHandler = { task.cancel() }
		return progress
	}
}
