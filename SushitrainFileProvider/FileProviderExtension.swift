// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import OSLog
import SynctrainFileProviderKit

let log = Logger(subsystem: "nl.t-shaped.Sushitrain.FileProvider", category: "Extension")

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

	required init(domain: NSFileProviderDomain) {
		self.domain = domain
		self.context = ExtensionContext()
		self.fetcher = context.map { ContentFetcher(context: $0) }
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
				completion.call(nil, nil, error.fileProviderError)
			}
			catch {
				log.warning("Could not fetch \(item.id): \(error.localizedDescription)")
				completion.call(nil, nil, progress.isCancelled ? CocoaError(.userCancelled) : error)
			}
		}
		return progress
	}

	// MARK: - Changes (not supported yet)

	private var readOnlyError: Error {
		NSError(domain: NSCocoaErrorDomain, code: NSFeatureUnsupportedError)
	}

	func createItem(
		basedOn itemTemplate: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?,
		options: NSFileProviderCreateItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		completionHandler(nil, [], false, readOnlyError)
		return Progress()
	}

	func modifyItem(
		_ item: NSFileProviderItem, baseVersion version: NSFileProviderItemVersion,
		changedFields: NSFileProviderItemFields, contents newContents: URL?,
		options: NSFileProviderModifyItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		// Opening a file updates its last used date; accept that and other metadata only the system keeps, so items
		// do not show an upload error. Other changes are not supported yet.
		guard changedFields.isSubset(of: [.lastUsedDate, .tagData, .favoriteRank]) else {
			completionHandler(nil, [], false, readOnlyError)
			return Progress()
		}
		if item.itemIdentifier == .rootContainer {
			completionHandler(RootItem(), [], false, nil)
			return Progress()
		}
		guard let context else {
			completionHandler(nil, [], false, unavailableError)
			return Progress()
		}
		do {
			guard let id = item.itemIdentifier.itemID, let stored = try context.store.item(id: id) else {
				completionHandler(
					nil, [], false, NSError.fileProviderErrorForNonExistentItem(withIdentifier: item.itemIdentifier))
				return Progress()
			}
			let current = try context.makeItem(stored)
			if let current = current as? FileProviderItem {
				current.lastUsedDate = item.lastUsedDate ?? nil
				current.tagData = item.tagData ?? nil
				current.favoriteRank = item.favoriteRank ?? nil
			}
			completionHandler(current, [], false, nil)
		}
		catch {
			completionHandler(nil, [], false, error.fileProviderError)
		}
		return Progress()
	}

	func deleteItem(
		identifier: NSFileProviderItemIdentifier, baseVersion version: NSFileProviderItemVersion,
		options: NSFileProviderDeleteItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		completionHandler(readOnlyError)
		return Progress()
	}
}
