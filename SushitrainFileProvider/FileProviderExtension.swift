// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import OSLog
import SynctrainFileProviderKit

let log = Logger(subsystem: "nl.t-shaped.Sushitrain.FileProvider", category: "Extension")

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
final class FileProviderExtension: NSObject, NSFileProviderReplicatedExtension {
	let domain: NSFileProviderDomain
	private let context: ExtensionContext?
	private let fetcher: ContentFetcher?
	private let writer: LocalWriter?

	required init(domain: NSFileProviderDomain) {
		self.domain = domain
		self.context = ExtensionContext()
		self.fetcher = context.map { ContentFetcher(context: $0) }
		self.writer = context.map { LocalWriter(context: $0) }
		super.init()
		if context == nil {
			log.error("The app group container is not available")
		}
	}

	func invalidate() {
		fetcher?.invalidate()
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
			return ContainerEnumerator(context: context, parentID: nil)
		case .trashContainer:
			throw NSError.fileProviderErrorForNonExistentItem(withIdentifier: containerItemIdentifier)
		default:
			guard let id = containerItemIdentifier.itemID, let item = try? context.store.item(id: id),
				item.kind.isContainer
			else {
				throw NSError.fileProviderErrorForNonExistentItem(withIdentifier: containerItemIdentifier)
			}
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

		// The version on this device, if it is current: no download, no extra space (the clone shares its blocks)
		if fetcher.cloneLocal(item, to: destination) {
			completionHandler(destination, try? context.makeItem(item), nil)
			return Progress()
		}

		if item.size == 0 {
			if FileManager.default.createFile(atPath: destination.path(percentEncoded: false), contents: nil) {
				completionHandler(destination, try? context.makeItem(item), nil)
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
				try await fetcher.download(item, from: heartbeat, to: destination, progress: progress)
				// If the user asked for it while the app was not running, the app no longer needs to list it
				try? context.store.completeWork(.fetch, extra: ItemIdentifiers.string(for: item.id))
				completion.call(destination, try? context.makeItem(item), nil)
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
				completion.call(nil, nil, progress.isCancelled ? CocoaError(.userCancelled) : error)
			}
		}
		return progress
	}

	// MARK: - Changes

	/// Fields only the system keeps; Syncthing does not sync them, so changing them needs no work
	private static let systemFields: NSFileProviderItemFields = [
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
			let item = try writer.create(template: itemTemplate, fields: fields, contents: url, options: options)
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

			let current = try context.makeItem(outcome.item)
			if let current = current as? FileProviderItem {
				current.lastUsedDate = item.lastUsedDate ?? nil
				current.tagData = item.tagData ?? nil
				current.favoriteRank = item.favoriteRank ?? nil
			}
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
			try writer.delete(identifier: identifier, baseVersion: version, options: options)
			trace("delete \(identifier.rawValue)")
			completionHandler(nil)
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
	private static func reportable(_ error: Error) -> Error {
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
