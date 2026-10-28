// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import SynctrainFileProviderKit
import UniformTypeIdentifiers

extension NSFileProviderItemIdentifier {
	init(itemID: Int64) {
		self.init(ItemIdentifiers.string(for: itemID))
	}

	var itemID: Int64? {
		ItemIdentifiers.itemID(from: rawValue)
	}
}

/// The root of the Synctrain location, containing one directory per Syncthing folder
final class RootItem: NSObject, NSFileProviderItem {
	var itemIdentifier: NSFileProviderItemIdentifier { .rootContainer }
	var parentItemIdentifier: NSFileProviderItemIdentifier { .rootContainer }
	var filename: String { "Synctrain" }
	var contentType: UTType { .folder }
	var capabilities: NSFileProviderItemCapabilities { [.allowsReading, .allowsContentEnumerating] }
	var itemVersion: NSFileProviderItemVersion {
		NSFileProviderItemVersion(contentVersion: Data([0]), metadataVersion: Data([0]))
	}
}

/// An item from the shared index
final class FileProviderItem: NSObject, NSFileProviderItem, NSFileProviderItemDecorating {
	let item: IndexItem
	let folder: IndexFolder?
	/// Whether the current version is on this device (so it can be opened while Synctrain is not running)
	let isOnDevice: Bool
	/// Used when the index has no modification time (folder roots and implied directories)
	let fallbackDate: Date?

	/// What only the system keeps about the item (Syncthing does not synchronize it), stored for it in the index
	let attributes: ItemAttributes
	/// The folder's binding, for items of a folder accessed through a gateway (nil for folders that Syncthing on this
	/// device synchronizes)
	let remote: RemoteFolderBinding?

	var lastUsedDate: Date? { attributes.lastUsedDate }
	var tagData: Data? { attributes.tagData }
	var favoriteRank: NSNumber? { attributes.favoriteRank.map { NSNumber(value: UInt64(bitPattern: $0)) } }

	/// Items need a date: the system fails to create items in its replica without one
	static let referenceDate = Date(timeIntervalSince1970: 946_684_800)  // 2000-01-01

	init(
		item: IndexItem, folder: IndexFolder?, isOnDevice: Bool, fallbackDate: Date? = nil,
		attributes: ItemAttributes = ItemAttributes(), remote: RemoteFolderBinding? = nil
	) {
		self.item = item
		self.folder = folder
		self.isOnDevice = isOnDevice
		self.fallbackDate = fallbackDate
		self.attributes = attributes
		self.remote = remote
	}

	var itemIdentifier: NSFileProviderItemIdentifier {
		NSFileProviderItemIdentifier(itemID: item.id)
	}

	var parentItemIdentifier: NSFileProviderItemIdentifier {
		item.parentID.map { NSFileProviderItemIdentifier(itemID: $0) } ?? .rootContainer
	}

	var filename: String {
		item.name
	}

	var contentType: UTType {
		switch item.kind {
		case .directory, .folderRoot:
			return .folder
		case .symlink:
			return .symbolicLink
		case .file:
			let ext = (item.name as NSString).pathExtension
			return ext.isEmpty ? .data : (UTType(filenameExtension: ext) ?? .data)
		}
	}

	/// Items can be changed in folders that send changes. Renaming, moving and deleting is limited to what is kept on
	/// this device (deleting removes the item on all devices).
	/// The system's copy of files and directories can always be removed (their contents are in the folder or on other
	/// devices). This depends on the index only, because the system refreshes capabilities only when the item's version
	/// changes; the extension checks the disk when it makes the change.
	var capabilities: NSFileProviderItemCapabilities {
		if let remote {
			return remoteCapabilities(isWritable: remote.permissions.write)
		}
		let isWritable = folder?.acceptsLocalChanges == true
		let isSelective = folder?.isSelective == true
		let isKept = !isSelective || item.isSelected || item.pending == .create
		switch item.kind {
		case .folderRoot:
			return isWritable
				? [.allowsReading, .allowsContentEnumerating, .allowsAddingSubItems]
				: [.allowsReading, .allowsContentEnumerating]
		case .directory:
			var capabilities: NSFileProviderItemCapabilities = [.allowsReading, .allowsContentEnumerating, .allowsEvicting]
			guard isWritable else {
				return capabilities
			}
			capabilities.insert(.allowsAddingSubItems)
			if isKept || isOnDevice {
				capabilities.insert(.allowsDeleting)
			}
			// In an on-demand folder, only directories that are kept as a whole
			if !isSelective || item.isSelected || item.pending == .create {
				capabilities.formUnion([.allowsRenaming, .allowsReparenting])
			}
			return capabilities
		case .file, .symlink:
			guard isWritable else {
				return [.allowsReading, .allowsEvicting]
			}
			// Saving a file that is only on other devices downloads it first and then keeps it on this device
			return isKept
				? [.allowsReading, .allowsWriting, .allowsRenaming, .allowsReparenting, .allowsDeleting, .allowsEvicting]
				: [.allowsReading, .allowsWriting, .allowsEvicting]
		}
	}

	/// In a folder accessed through a gateway, what may be changed follows the folder's permissions there. Items with a
	/// local change the gateway has not confirmed are not evicted (their contents are staged, but the system keeps its
	/// copy until the change is synchronized anyway).
	private func remoteCapabilities(isWritable: Bool) -> NSFileProviderItemCapabilities {
		switch item.kind {
		case .folderRoot:
			return isWritable
				? [.allowsReading, .allowsContentEnumerating, .allowsAddingSubItems]
				: [.allowsReading, .allowsContentEnumerating]
		case .directory:
			var capabilities: NSFileProviderItemCapabilities = [.allowsReading, .allowsContentEnumerating]
			if item.pending == .none {
				capabilities.insert(.allowsEvicting)
			}
			if isWritable {
				capabilities.formUnion([.allowsAddingSubItems, .allowsRenaming, .allowsReparenting, .allowsDeleting])
			}
			return capabilities
		case .file, .symlink:
			var capabilities: NSFileProviderItemCapabilities = [.allowsReading]
			if item.pending == .none {
				capabilities.insert(.allowsEvicting)
			}
			if isWritable {
				capabilities.formUnion([.allowsWriting, .allowsRenaming, .allowsReparenting, .allowsDeleting])
			}
			return capabilities
		}
	}

	var documentSize: NSNumber? {
		item.kind == .file ? NSNumber(value: item.size) : nil
	}

	var contentModificationDate: Date? {
		item.modificationDate ?? fallbackDate ?? Self.referenceDate
	}

	var creationDate: Date? {
		contentModificationDate
	}

	var itemVersion: NSFileProviderItemVersion {
		NSFileProviderItemVersion(
			contentVersion: Self.versionData(item.contentRevision),
			metadataVersion: Self.versionData(item.metadataRevision))
	}

	/// Whether items can be changed is expressed through capabilities. The POSIX permissions stay writable: the system
	/// cannot finish creating an item in its replica after making it read-only.
	var fileSystemFlags: NSFileProviderFileSystemFlags {
		item.kind.isContainer ? [.userReadable, .userWritable, .userExecutable] : [.userReadable, .userWritable]
	}

	/// Read by the activation rules of custom actions and decorations. In folders accessed through a gateway, keeping
	/// files on this device is up to this device alone (`remoteFile`, `onDevice` and `localChange`).
	var userInfo: [AnyHashable: Any]? {
		if remote != nil {
			return [
				"selective": false,
				"selected": false,
				"onDevice": isOnDevice,
				"folderRoot": item.kind == .folderRoot,
				"remoteFile": item.kind == .file,
				"localChange": item.pending != .none,
			]
		}
		return [
			"selective": folder?.isSelective ?? false,
			"selected": item.isSelected,
			"onDevice": isOnDevice,
			"folderRoot": item.kind == .folderRoot,
			"remoteFile": false,
		]
	}

	/// Badges declared in the extension's Info.plist (NSFileProviderDecorations)
	var decorations: [NSFileProviderItemDecorationIdentifier]? {
		var decorations: [NSFileProviderItemDecorationIdentifier] = []
		if remote == nil, item.kind != .folderRoot, folder?.isSelective == true, item.isSelected {
			decorations.append(Self.decoration("kept"))
		}
		if item.kind == .file, FileNames.isConflictCopy(item.name) {
			decorations.append(Self.decoration("conflict"))
		}
		return decorations
	}

	private static func decoration(_ name: String) -> NSFileProviderItemDecorationIdentifier {
		NSFileProviderItemDecorationIdentifier(rawValue: (Bundle.main.bundleIdentifier ?? "") + "." + name)
	}

	@available(iOS 27.0, *)
	var namespacePolicy: NSFileProviderNamespacePolicy {
		// Folders that are kept on this device are browsable without the app, so keep them fully enumerated
		remote == nil && item.kind.isContainer && item.isSelected && folder?.isSelective == true
			? .materializeEagerly : .inherited
	}

	static func versionData(_ revision: Int64) -> Data {
		withUnsafeBytes(of: revision.bigEndian) { Data($0) }
	}
}
