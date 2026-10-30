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

/// The root of the Synctrain location, containing one directory per folder
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
	/// Whether the contents the item shows are on this device
	let isOnDevice: Bool

	/// What only the system keeps about the item (the gateway does not know it), stored for it in the index
	let attributes: ItemAttributes
	/// The binding of the item's folder
	let remote: RemoteFolderBinding

	var lastUsedDate: Date? { attributes.lastUsedDate }
	var tagData: Data? { attributes.tagData }
	var favoriteRank: NSNumber? { attributes.favoriteRank.map { NSNumber(value: UInt64(bitPattern: $0)) } }

	/// Items need a date: the system fails to create items in its replica without one
	static let referenceDate = Date(timeIntervalSince1970: 946_684_800)  // 2000-01-01

	init(item: IndexItem, isOnDevice: Bool, attributes: ItemAttributes = ItemAttributes(), remote: RemoteFolderBinding) {
		self.item = item
		self.isOnDevice = isOnDevice
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

	/// What may be changed follows the folder's permissions on the gateway. Items with a local change the gateway has not
	/// confirmed are not evicted (their contents are staged, but the system keeps its copy until the change is
	/// synchronized anyway). This depends on the index only, because the system refreshes capabilities only when the
	/// item's version changes.
	var capabilities: NSFileProviderItemCapabilities {
		let isWritable = remote.permissions.write
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
		item.modificationDate ?? Self.referenceDate
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

	/// Read by the activation rules of custom actions. Keeping files on this device is up to this device alone.
	var userInfo: [AnyHashable: Any]? {
		[
			"onDevice": isOnDevice,
			"folderRoot": item.kind == .folderRoot,
			"remoteFile": item.kind == .file,
			"localChange": item.pending != .none,
		]
	}

	/// Badges declared in the extension's Info.plist (NSFileProviderDecorations)
	var decorations: [NSFileProviderItemDecorationIdentifier]? {
		item.kind == .file && FileNames.isConflictCopy(item.name) ? [Self.decoration("conflict")] : []
	}

	private static func decoration(_ name: String) -> NSFileProviderItemDecorationIdentifier {
		NSFileProviderItemDecorationIdentifier(rawValue: (Bundle.main.bundleIdentifier ?? "") + "." + name)
	}

	static func versionData(_ revision: Int64) -> Data {
		withUnsafeBytes(of: revision.bigEndian) { Data($0) }
	}

	/// The revision a version's data stands for, or nil when it is not one of ours
	static func revision(from data: Data) -> Int64? {
		guard data.count == 8 else {
			return nil
		}
		return data.reduce(Int64(0)) { ($0 << 8) | Int64($1) }
	}
}
