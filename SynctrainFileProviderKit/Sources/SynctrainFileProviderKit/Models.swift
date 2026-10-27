// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public enum ItemKind: Int, Sendable, Codable {
	case file = 0
	case directory = 1
	case symlink = 2
	/// The root directory of a Syncthing folder
	case folderRoot = 3

	public var isContainer: Bool {
		self == .directory || self == .folderRoot
	}
}

/// Changes made through the File Provider that Syncthing has not picked up yet
public enum PendingChange: Int, Sendable {
	case none = 0
	case create = 1
	case modify = 2
	case delete = 3
	case rename = 4
}

public struct IndexItem: Sendable, Hashable {
	public var id: Int64
	public var folderID: String
	/// Path relative to the folder root, "" for folder roots
	public var path: String
	/// Nil for folder roots (whose parent is the File Provider root container)
	public var parentID: Int64?
	/// The display name (the folder label for folder roots)
	public var name: String
	public var kind: ItemKind
	public var size: Int64
	public var modifiedNanoseconds: Int64
	public var blocksHash: String?
	public var modifiedBy: String?
	/// Incremented whenever the contents change
	public var contentRevision: Int64
	/// Incremented whenever anything about the item that the File Provider shows changes
	public var metadataRevision: Int64
	public var changedSequence: Int64
	/// Whether the item is kept on this device (selected in a selective folder, or not ignored in a regular folder)
	public var isSelected: Bool
	/// A directory that is not in the global index itself, but has entries below it
	public var isImplied: Bool
	public var pending: PendingChange
	public var isDeleted: Bool

	public var modificationDate: Date? {
		modifiedNanoseconds == 0 ? nil : Date(timeIntervalSince1970: Double(modifiedNanoseconds) / 1_000_000_000)
	}
}

/// The version of a file as the index knows it
public struct FileVersion: Sendable, Equatable {
	public let size: Int64
	public let modifiedNanoseconds: Int64

	public init(size: Int64, modifiedNanoseconds: Int64) {
		self.size = size
		self.modifiedNanoseconds = modifiedNanoseconds
	}
}

public struct IndexFolder: Sendable, Hashable {
	public var folderID: String
	public var rootItemID: Int64
	public var label: String
	/// The Syncthing folder type, as reported by SushitrainFolder.folderType()
	public var folderType: String
	public var isSelective: Bool

	public static let typeSendReceive = "sendrecieve"
	public static let typeReceiveOnly = "receiveonly"
	public static let typeSendOnly = "sendonly"

	/// Whether changes made on this device are sent to other devices
	public var acceptsLocalChanges: Bool {
		folderType == Self.typeSendReceive || folderType == Self.typeSendOnly
	}
}

/// An entry from the Syncthing global index, as delivered (in JSON) by SushitrainFolder.globalChanges and friends
public struct SyncthingRecord: Codable, Sendable, Hashable {
	public var path: String
	public var seq: Int64
	public var type: String
	public var deleted: Bool?
	public var size: Int64
	public var modNs: Int64
	public var blocksHash: String?
	public var modifiedBy: String?
	public var symlinkTarget: String?
	public var selected: Bool
	public var hidden: Bool?
	public var full: Bool

	public init(
		path: String, seq: Int64, type: String, deleted: Bool? = nil, size: Int64 = 0, modNs: Int64 = 0,
		blocksHash: String? = nil, modifiedBy: String? = nil, symlinkTarget: String? = nil, selected: Bool = true,
		hidden: Bool? = nil, full: Bool = true
	) {
		self.path = path
		self.seq = seq
		self.type = type
		self.deleted = deleted
		self.size = size
		self.modNs = modNs
		self.blocksHash = blocksHash
		self.modifiedBy = modifiedBy
		self.symlinkTarget = symlinkTarget
		self.selected = selected
		self.hidden = hidden
		self.full = full
	}

	public var kind: ItemKind {
		switch type {
		case "directory": return .directory
		case "symlink": return .symlink
		default: return .file
		}
	}

	/// Whether the entry should not be visible (deleted, hidden, or a symlink, which is not exposed yet)
	public var isGone: Bool {
		deleted == true || hidden == true || kind == .symlink
	}

	public static func decodeBatch(_ data: Data) throws -> [SyncthingRecord] {
		try JSONDecoder().decode([SyncthingRecord].self, from: data)
	}
}

/// A position in the stream of index changes. Anchors from another generation (after a rebuild) are invalid.
public struct SyncAnchor: Sendable, Hashable {
	public var generation: UUID
	public var sequence: Int64

	public init(generation: UUID, sequence: Int64) {
		self.generation = generation
		self.sequence = sequence
	}

	public init?(data: Data) {
		guard data.count == 24 else {
			return nil
		}
		let uuidBytes = Array(data.prefix(16))
		generation = UUID(
			uuid: (
				uuidBytes[0], uuidBytes[1], uuidBytes[2], uuidBytes[3], uuidBytes[4], uuidBytes[5], uuidBytes[6],
				uuidBytes[7], uuidBytes[8], uuidBytes[9], uuidBytes[10], uuidBytes[11], uuidBytes[12], uuidBytes[13],
				uuidBytes[14], uuidBytes[15]
			))
		var seq: Int64 = 0
		for byte in data.suffix(8) {
			seq = (seq << 8) | Int64(byte)
		}
		sequence = seq
	}

	public var data: Data {
		var result = withUnsafeBytes(of: generation.uuid) { Data($0) }
		for shift in stride(from: 56, through: 0, by: -8) {
			result.append(UInt8(truncatingIfNeeded: sequence >> Int64(shift)))
		}
		return result
	}
}

/// Encodes item identifiers and enumeration pages
public enum ItemIdentifiers {
	public static func string(for itemID: Int64) -> String {
		String(itemID)
	}

	public static func itemID(from string: String) -> Int64? {
		guard let id = Int64(string), id > 0 else {
			return nil
		}
		return id
	}

	/// A page token that continues after the given item ID
	public static func page(after itemID: Int64) -> Data {
		withUnsafeBytes(of: itemID.bigEndian) { Data($0) }
	}

	public static func itemID(fromPage data: Data) -> Int64? {
		guard data.count == 8 else {
			return nil
		}
		var value: Int64 = 0
		for byte in data {
			value = (value << 8) | Int64(byte)
		}
		return value
	}
}

public enum WorkKind: String, Sendable, Codable, CaseIterable {
	/// Keep a path on this device (select it in a selective folder)
	case select
	/// Stop keeping a path on this device, if another device has a full copy
	case deselectSafely
	/// Rescan a path after it was changed through the File Provider
	case rescan
	/// Complete a rename in a selective folder (the new name was hard linked)
	case finishRename
	/// The user tried to open a file that is not on this device while the app was not running
	case fetch
	/// Remove the system's copy of an item that is no longer kept on this device (queued by the app itself, and tried
	/// again while the file is in use)
	case evict
}

public struct WorkItem: Sendable, Hashable {
	public var id: Int64
	public var kind: WorkKind
	public var folderID: String
	public var path: String
	/// Extra parameters (e.g. the new path for a rename, or the item ID for a fetch)
	public var extra: String?
	public var attempts: Int
	public var lastError: String?
	public var createdAt: Date
}

public struct ChangeSet: Sendable {
	public var updated: [IndexItem]
	/// Removed items (with the location they had before they were removed)
	public var deleted: [IndexItem]
	/// The anchor to continue from
	public var anchor: SyncAnchor
	public var hasMore: Bool

	public var deletedItemIDs: [Int64] {
		deleted.map(\.id)
	}
}

/// What the system keeps about an item that Syncthing does not synchronize: tags, the favorite rank and when the item was
/// last used. Kept for this device only.
public struct ItemAttributes: Sendable, Equatable {
	public var tagData: Data?
	/// `NSFileProviderFavoriteRankUnranked` and other ranks above Int64.max are kept as their bit pattern
	public var favoriteRank: Int64?
	public var lastUsedDate: Date?

	public init(tagData: Data? = nil, favoriteRank: Int64? = nil, lastUsedDate: Date? = nil) {
		self.tagData = tagData
		self.favoriteRank = favoriteRank
		self.lastUsedDate = lastUsedDate
	}

	public var isEmpty: Bool {
		tagData == nil && favoriteRank == nil && lastUsedDate == nil
	}
}
