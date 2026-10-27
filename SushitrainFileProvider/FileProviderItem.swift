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
final class FileProviderItem: NSObject, NSFileProviderItem {
	let item: IndexItem
	let folder: IndexFolder?
	/// Whether the current version is on this device (so it can be opened while Synctrain is not running)
	let isOnDevice: Bool
	/// Used when the index has no modification time (folder roots and implied directories)
	let fallbackDate: Date?

	/// User metadata that only the system keeps (Syncthing does not sync it); echoed back after the system changes it
	var lastUsedDate: Date?
	var tagData: Data?
	var favoriteRank: NSNumber?

	/// Items need a date: the system fails to create items in its replica without one
	static let referenceDate = Date(timeIntervalSince1970: 946_684_800)  // 2000-01-01

	init(item: IndexItem, folder: IndexFolder?, isOnDevice: Bool, fallbackDate: Date? = nil) {
		self.item = item
		self.folder = folder
		self.isOnDevice = isOnDevice
		self.fallbackDate = fallbackDate
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

	var capabilities: NSFileProviderItemCapabilities {
		if item.kind.isContainer {
			return [.allowsReading, .allowsContentEnumerating]
		}
		return [.allowsReading]
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

	/// Read by the activation rules of custom actions and decorations
	var userInfo: [AnyHashable: Any]? {
		[
			"selective": folder?.isSelective ?? false,
			"selected": item.isSelected,
			"onDevice": isOnDevice,
			"folderRoot": item.kind == .folderRoot,
		]
	}

	@available(iOS 27.0, *)
	var namespacePolicy: NSFileProviderNamespacePolicy {
		// Folders that are kept on this device are browsable without the app, so keep them fully enumerated
		item.kind.isContainer && item.isSelected && folder?.isSelective == true ? .materializeEagerly : .inherited
	}

	static func versionData(_ revision: Int64) -> Data {
		withUnsafeBytes(of: revision.bigEndian) { Data($0) }
	}
}
