// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import SynctrainFileProviderKit

/// Shared state of the extension: where the index and the folder contents are
struct ExtensionContext: Sendable {
	let container: SharedContainer
	let store: IndexStore

	static let maximumPageSize = 500

	init?() {
		guard let container = SharedContainer.appGroup() else {
			return nil
		}
		self.container = container
		self.store = IndexStore(container: container)
	}

	func localURL(for item: IndexItem) -> URL {
		container.fileURL(folderID: item.folderID, path: item.path)
	}

	/// Whether the version of the file in the index is on this device
	func isOnDevice(_ item: IndexItem) -> Bool {
		switch item.kind {
		case .folderRoot, .directory:
			return FileFingerprint(path: localURL(for: item).path(percentEncoded: false))?.isDirectory == true
		case .symlink:
			return false
		case .file:
			// Changes made through the File Provider are recorded with what they wrote, so this holds for them as well
			guard let fingerprint = FileFingerprint(path: localURL(for: item).path(percentEncoded: false)) else {
				return false
			}
			return fingerprint.matches(size: item.size, modifiedNanoseconds: item.modifiedNanoseconds)
		}
	}

	func makeItems(_ items: [IndexItem]) throws(IndexError) -> [NSFileProviderItem] {
		let folders = Dictionary(uniqueKeysWithValues: try store.folders().map { ($0.folderID, $0) })
		let attributes = try store.attributes(of: items.map(\.id))
		return items.map { makeItem($0, folder: folders[$0.folderID], attributes: attributes[$0.id]) }
	}

	func makeItem(_ item: IndexItem) throws(IndexError) -> NSFileProviderItem {
		makeItem(
			item, folder: try store.folder(id: item.folderID), attributes: try store.attributes(of: [item.id])[item.id])
	}

	/// The item to return with its contents, which must not be missing (without the folder's details if they cannot be
	/// read)
	func makeItemForContents(_ item: IndexItem) -> NSFileProviderItem {
		(try? makeItem(item)) ?? makeItem(item, folder: nil, attributes: nil)
	}

	private func makeItem(_ item: IndexItem, folder: IndexFolder?, attributes: ItemAttributes?) -> NSFileProviderItem {
		FileProviderItem(
			item: item, folder: folder, isOnDevice: isOnDevice(item),
			fallbackDate: item.modificationDate == nil ? localModificationDate(item) : nil,
			attributes: attributes ?? ItemAttributes())
	}

	/// Stores the attributes only the system keeps (tags, favorite rank, last used date) that are among the fields
	func saveAttributes(of template: NSFileProviderItem, fields: NSFileProviderItemFields, itemID: Int64)
		throws(IndexError)
	{
		guard !fields.isDisjoint(with: [.tagData, .favoriteRank, .lastUsedDate]) else {
			return
		}
		var attributes = try store.attributes(of: [itemID])[itemID] ?? ItemAttributes()
		if fields.contains(.tagData) {
			attributes.tagData = template.tagData ?? nil
		}
		if fields.contains(.favoriteRank) {
			attributes.favoriteRank = (template.favoriteRank ?? nil).map { Int64(bitPattern: $0.uint64Value) }
		}
		if fields.contains(.lastUsedDate) {
			attributes.lastUsedDate = template.lastUsedDate ?? nil
		}
		try store.setAttributes(attributes, of: itemID)
	}

	/// The modification time of the item on this device
	private func localModificationDate(_ item: IndexItem) -> Date? {
		guard let fingerprint = FileFingerprint(path: localURL(for: item).path(percentEncoded: false)) else {
			return nil
		}
		return Date(timeIntervalSince1970: Double(fingerprint.modifiedNanoseconds) / 1_000_000_000)
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

/// Lists the children of a container. The root container lists the Syncthing folders.
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
	/// ID of 0 selects the Syncthing folders, which are the children of the root container).
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
