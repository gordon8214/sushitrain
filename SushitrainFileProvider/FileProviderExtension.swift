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

/// Serves the Synctrain location in the Files app from the shared index: the folders of the paired gateways, whose
/// metadata, contents and changes `RemoteProvider` exchanges with them. This process must stay small (the system
/// terminates File Provider extensions on iOS that use more than about 20 MB).
final class FileProviderExtension: NSObject, NSFileProviderReplicatedExtension {
	let domain: NSFileProviderDomain
	let context: ExtensionContext?
	let remote: RemoteProvider?

	required init(domain: NSFileProviderDomain) {
		self.domain = domain
		self.context = ExtensionContext()
		self.remote = context.map { RemoteProvider(context: $0, domain: domain) }
		super.init()
		remote?.start()
		if context != nil {
			// Left behind when an earlier instance was terminated in the middle of a download
			if let temporary = try? NSFileProviderManager(for: domain)?.temporaryDirectoryURL() {
				Self.removeLeftovers(in: temporary)
			}
		}
		else {
			log.error("The app group container is not available")
		}
	}

	/// Removes files that were not touched for a while
	private static func removeLeftovers(in directory: URL, olderThan age: TimeInterval = 3600) {
		let fileManager = FileManager.default
		guard
			let entries = try? fileManager.contentsOfDirectory(
				at: directory, includingPropertiesForKeys: [.contentModificationDateKey])
		else {
			return
		}
		for entry in entries {
			let modified = (try? entry.resourceValues(forKeys: [.contentModificationDateKey]))?.contentModificationDate
			if let modified, Date().timeIntervalSince(modified) > age {
				try? fileManager.removeItem(at: entry)
			}
		}
	}

	func invalidate() {
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
			// There is no trash: deleting removes items on the gateway
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
			// This never waits for the refresh
			remote?.refreshSoon(folderIDs: [item.folderID])
			return ContainerEnumerator(context: context, parentID: item.id)
		}
	}

	// MARK: - Contents

	func fetchContents(
		for itemIdentifier: NSFileProviderItemIdentifier, version requestedVersion: NSFileProviderItemVersion?,
		request: NSFileProviderRequest, completionHandler: @escaping (URL?, NSFileProviderItem?, (any Error)?) -> Void
	) -> Progress {
		guard let context, let remote, let manager = NSFileProviderManager(for: domain) else {
			completionHandler(nil, nil, unavailableError)
			return Progress()
		}
		do {
			guard let id = itemIdentifier.itemID, let item = try context.store.item(id: id), item.kind == .file,
				let binding = try context.remoteBinding(folderID: item.folderID)
			else {
				completionHandler(nil, nil, NSError.fileProviderErrorForNonExistentItem(withIdentifier: itemIdentifier))
				return Progress()
			}
			let destination = try manager.temporaryDirectoryURL().appending(path: UUID().uuidString)
			return remote.fetchContents(
				of: item, binding: binding, to: destination, request: request, completionHandler: completionHandler)
		}
		catch let error as IndexError {
			completionHandler(nil, nil, error.fileProviderError)
		}
		catch {
			completionHandler(nil, nil, error)
		}
		return Progress()
	}

	// MARK: - Changes

	func createItem(
		basedOn itemTemplate: NSFileProviderItem, fields: NSFileProviderItemFields, contents url: URL?,
		options: NSFileProviderCreateItemOptions = [], request: NSFileProviderRequest,
		completionHandler: @escaping (NSFileProviderItem?, NSFileProviderItemFields, Bool, (any Error)?) -> Void
	) -> Progress {
		guard let context, let remote else {
			completionHandler(nil, [], false, unavailableError)
			return Progress()
		}
		do {
			guard let parentID = itemTemplate.parentItemIdentifier.itemID else {
				let root = try remote.existingFolderRoot(for: itemTemplate, options: options)
				completionHandler(try context.makeItem(root), [], false, nil)
				return Progress()
			}
			guard let parent = try context.store.item(id: parentID),
				let binding = try context.remoteBinding(folderID: parent.folderID)
			else {
				throw NSFileProviderError(.noSuchItem)
			}
			return remote.createItem(
				basedOn: itemTemplate, fields: fields, contents: url, options: options, parent: parent,
				binding: binding, completionHandler: completionHandler)
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
		guard let context, let remote else {
			completionHandler(nil, [], false, unavailableError)
			return Progress()
		}
		do {
			guard let id = item.itemIdentifier.itemID, let stored = try context.store.item(id: id),
				let binding = try context.remoteBinding(folderID: stored.folderID)
			else {
				throw NSFileProviderError(.noSuchItem)
			}
			return remote.modifyItem(
				item, baseVersion: version, changedFields: changedFields, contents: newContents,
				options: options, item: stored, binding: binding, completionHandler: completionHandler)
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
		guard let remote else {
			completionHandler(unavailableError)
			return Progress()
		}
		do {
			guard let binding = try remote.binding(ofDeleted: identifier) else {
				// Already gone: reporting anything else would make the system try again forever
				completionHandler(nil)
				return Progress()
			}
			return remote.deleteItem(
				identifier: identifier, baseVersion: version, options: options, binding: binding,
				completionHandler: completionHandler)
		}
		catch {
			trace("delete \(identifier.rawValue) failed: \(error)")
			log.info("Could not delete \(identifier.rawValue): \(error.localizedDescription)")
			completionHandler(Self.reportable(error))
		}
		return Progress()
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

/// "Keep on This Device" and "Stop Keeping on This Device": what this device keeps of a gateway's files is up to this
/// device alone
extension FileProviderExtension: NSFileProviderCustomAction {
	private static let actionPrefix = (Bundle.main.bundleIdentifier ?? "") + "."

	func performAction(
		identifier actionIdentifier: NSFileProviderExtensionActionIdentifier,
		onItemsWithIdentifiers itemIdentifiers: [NSFileProviderItemIdentifier],
		completionHandler: @escaping ((any Error)?) -> Void
	) -> Progress {
		guard let context, let remote else {
			completionHandler(unavailableError)
			return Progress()
		}
		let keep: Bool
		switch actionIdentifier.rawValue {
		case Self.actionPrefix + "keep":
			keep = true
		case Self.actionPrefix + "stopKeeping":
			keep = false
		default:
			completionHandler(CocoaError(.featureUnsupported))
			return Progress()
		}

		let items: [IndexItem]
		do {
			var found: [IndexItem] = []
			for identifier in itemIdentifiers {
				if let id = identifier.itemID, let item = try context.store.item(id: id),
					try context.remoteBinding(folderID: item.folderID) != nil
				{
					found.append(item)
				}
			}
			items = found
		}
		catch {
			completionHandler(Self.reportable(error))
			return Progress()
		}
		trace("action \(actionIdentifier.rawValue) on \(items.map(\.id))")
		let completion = Completion(call: completionHandler)
		let task = Task {
			do {
				try await remote.perform(keep: keep, on: items)
				completion.call(nil)
			}
			catch {
				completion.call(Self.reportable(error))
			}
		}
		let progress = Progress(totalUnitCount: Int64(items.count))
		progress.cancellationHandler = { task.cancel() }
		return progress
	}
}
