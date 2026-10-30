// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import SynctrainFileProviderKit

/// Shared state of the extension: where the index and the contents of files are
struct ExtensionContext: Sendable {
	let container: SharedContainer
	let store: IndexStore
	/// Staged edits and verified cached copies
	let storage: RemoteStorage

	static let maximumPageSize = 500

	init?() {
		guard let container = SharedContainer.appGroup() else {
			return nil
		}
		self.container = container
		self.store = IndexStore(container: container)
		self.storage = RemoteStorage(container: container)
	}

	/// The binding of the folder; nil for a folder this device no longer has
	func remoteBinding(folderID: String) throws(IndexError) -> RemoteFolderBinding? {
		try store.remoteBinding(folderID: folderID)
	}

	/// The bindings of the folders, by folder ID
	private func remoteBindings() throws(IndexError) -> [String: RemoteFolderBinding] {
		var bindings: [String: RemoteFolderBinding] = [:]
		for binding in try store.remoteBindings() {
			bindings[binding.folderID] = binding
		}
		return bindings
	}

	/// Whether this device holds the contents the row shows: its staged local change, or a verified copy of the gateway's
	/// version
	func isOnDevice(_ item: IndexItem, content: RemoteLocalContent?) -> Bool {
		guard item.kind == .file else {
			return false
		}
		func exists(_ path: String?) -> Bool {
			path.map { FileManager.default.fileExists(atPath: storage.url(relativePath: $0).path(percentEncoded: false)) }
				?? false
		}
		if exists(content?.overlayStagedPath) {
			return true
		}
		guard let shown = item.blocksHash.map({ ContentIdentity($0) }) else {
			return false
		}
		if let cached = storage.cachedContentURL(shown),
			FileManager.default.fileExists(atPath: cached.path(percentEncoded: false))
		{
			return true
		}
		return content?.contentID == shown && exists(content?.committedStagedPath)
	}

	func makeItems(_ items: [IndexItem]) throws(IndexError) -> [NSFileProviderItem] {
		let bindings = try remoteBindings()
		let known = items.filter { bindings[$0.folderID] != nil }
		let content = known.isEmpty ? [:] : try store.remoteLocalContent(of: known.map(\.id))
		// An item the system created is listed once the gateway committed it: until then the system shows its own pending
		// item, and listing ours too would look like a second item with the same name. What Synctrain made itself (the
		// conflict copy of an edit) is listed right away: the system has no item for it, and learns about it only from
		// enumerations.
		let shown = known.filter { !($0.pending == .create && content[$0.id]?.overlayOrigin == .system) }
		let attributes = try store.attributes(of: shown.map(\.id))
		return shown.compactMap { item in
			bindings[item.folderID].map {
				makeItem(item, attributes: attributes[item.id], remote: $0, content: content[item.id])
			}
		}
	}

	func makeItem(_ item: IndexItem) throws(IndexError) -> NSFileProviderItem {
		guard let remote = try remoteBinding(folderID: item.folderID) else {
			throw .notFound
		}
		return makeItem(
			item, attributes: try store.attributes(of: [item.id])[item.id], remote: remote,
			content: try store.remoteLocalContent(of: [item.id])[item.id])
	}

	/// The item to return with its contents, which must not be missing (without its details if they cannot be read)
	func makeItemForContents(_ item: IndexItem, remote: RemoteFolderBinding) -> NSFileProviderItem {
		(try? makeItem(item)) ?? makeItem(item, attributes: nil, remote: remote, content: nil)
	}

	private func makeItem(
		_ item: IndexItem, attributes: ItemAttributes?, remote: RemoteFolderBinding, content: RemoteLocalContent?
	) -> NSFileProviderItem {
		FileProviderItem(
			item: item, isOnDevice: isOnDevice(item, content: content), attributes: attributes ?? ItemAttributes(),
			remote: remote)
	}

	static func pageSize(_ suggested: Int) -> Int {
		suggested > 0 ? min(suggested, maximumPageSize) : 200
	}
}

extension IndexError {
	/// The error to report to the File Provider framework
	var fileProviderError: Error {
		switch self {
		case .anchorExpired:
			return NSFileProviderError(.syncAnchorExpired)
		case .notFound:
			return NSFileProviderError(.noSuchItem)
		case .nameCollision:
			return NSFileProviderError(.filenameCollision)
		case .notAllowed(let reason):
			return NSError(
				domain: NSFileProviderErrorDomain, code: NSFileProviderError.cannotSynchronize.rawValue,
				userInfo: [NSLocalizedDescriptionKey: reason])
		case .sqlite:
			// Usually the app holding the index for a moment. Any error other than a few File Provider errors makes the
			// system try again; reporting the server as unreachable would pause the location until the app says so.
			return NSError(
				domain: NSCocoaErrorDomain, code: NSXPCConnectionReplyInvalid, userInfo: [NSUnderlyingErrorKey: self as NSError])
		case .unavailable, .unsupportedSchema:
			return NSFileProviderError(.serverUnreachable)
		}
	}
}

/// Lists the children of a container. The root container lists the folders.
final class ContainerEnumerator: NSObject, NSFileProviderEnumerator {
	private let context: ExtensionContext
	private let parentID: Int64?

	init(context: ExtensionContext, parentID: Int64?) {
		self.context = context
		self.parentID = parentID
	}

	func invalidate() {}

	func enumerateItems(for observer: NSFileProviderEnumerationObserver, startingAt page: NSFileProviderPage) {
		let after = ItemIdentifiers.itemID(fromPage: page.rawValue) ?? 0
		let limit = ExtensionContext.pageSize(observer.suggestedPageSize ?? 0)
		do {
			let children = try context.store.children(of: parentID, after: after, limit: limit)
			observer.didEnumerate(try context.makeItems(children))
			trace("listed \(children.count) items in \(parentID.map(String.init) ?? "root") \(memoryFootprint())")
			if children.count == limit, let last = children.last {
				observer.finishEnumerating(upTo: NSFileProviderPage(ItemIdentifiers.page(after: last.id)))
			}
			else {
				observer.finishEnumerating(upTo: nil)
			}
		}
		catch {
			log.error("Could not enumerate container \(String(describing: self.parentID)): \(String(describing: error))")
			observer.finishEnumeratingWithError(error.fileProviderError)
		}
	}

	func enumerateChanges(for observer: NSFileProviderChangeObserver, from anchor: NSFileProviderSyncAnchor) {
		WorkingSetEnumerator.enumerateChanges(
			context: context, observer: observer, from: anchor, onlyChildrenOf: parentID ?? 0)
	}

	func currentSyncAnchor(completionHandler: @escaping (NSFileProviderSyncAnchor?) -> Void) {
		completionHandler((try? context.store.currentAnchor()).map { NSFileProviderSyncAnchor($0.data) })
	}
}

/// Enumerates all items, and all changes since an anchor. The system keeps its replica up to date through this.
final class WorkingSetEnumerator: NSObject, NSFileProviderEnumerator {
	private let context: ExtensionContext

	init(context: ExtensionContext) {
		self.context = context
	}

	func invalidate() {}

	func enumerateItems(for observer: NSFileProviderEnumerationObserver, startingAt page: NSFileProviderPage) {
		let after = ItemIdentifiers.itemID(fromPage: page.rawValue) ?? 0
		let limit = ExtensionContext.pageSize(observer.suggestedPageSize ?? 0)
		do {
			let items = try context.store.liveItems(after: after, limit: limit)
			observer.didEnumerate(try context.makeItems(items))
			trace("listed \(items.count) items of the working set \(memoryFootprint())")
			if items.count == limit, let last = items.last {
				observer.finishEnumerating(upTo: NSFileProviderPage(ItemIdentifiers.page(after: last.id)))
			}
			else {
				observer.finishEnumerating(upTo: nil)
			}
		}
		catch {
			log.error("Could not enumerate the working set: \(String(describing: error))")
			observer.finishEnumeratingWithError(error.fileProviderError)
		}
	}

	func enumerateChanges(for observer: NSFileProviderChangeObserver, from anchor: NSFileProviderSyncAnchor) {
		Self.enumerateChanges(context: context, observer: observer, from: anchor, onlyChildrenOf: nil)
	}

	/// Reports changes since the anchor. With a container, only changes to its direct children are reported (a parent
	/// ID of 0 selects the folders, which are the children of the root container).
	static func enumerateChanges(
		context: ExtensionContext, observer: NSFileProviderChangeObserver, from anchorData: NSFileProviderSyncAnchor,
		onlyChildrenOf parentID: Int64?
	) {
		guard let anchor = SyncAnchor(data: anchorData.rawValue) else {
			observer.finishEnumeratingWithError(NSFileProviderError(.syncAnchorExpired))
			return
		}
		do {
			let limit = ExtensionContext.pageSize(observer.suggestedBatchSize ?? 0)
			let changes = try context.store.changes(since: anchor, limit: limit)
			var updated = changes.updated
			var deleted = changes.deleted
			if let parentID {
				let isChild: (IndexItem) -> Bool = { ($0.parentID ?? 0) == parentID }
				updated = updated.filter(isChild)
				deleted = deleted.filter(isChild)
			}
			if !updated.isEmpty {
				observer.didUpdate(try context.makeItems(updated))
			}
			if !deleted.isEmpty {
				observer.didDeleteItems(withIdentifiers: deleted.map { NSFileProviderItemIdentifier(itemID: $0.id) })
			}
			observer.finishEnumeratingChanges(
				upTo: NSFileProviderSyncAnchor(changes.anchor.data), moreComing: changes.hasMore)
		}
		catch {
			log.error("Could not enumerate changes: \(String(describing: error))")
			observer.finishEnumeratingWithError(error.fileProviderError)
		}
	}

	func currentSyncAnchor(completionHandler: @escaping (NSFileProviderSyncAnchor?) -> Void) {
		completionHandler((try? context.store.currentAnchor()).map { NSFileProviderSyncAnchor($0.data) })
	}
}
