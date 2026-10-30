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
	/// The root directory of a folder
	case folderRoot = 3

	public var isContainer: Bool {
		self == .directory || self == .folderRoot
	}
}

/// Changes made through the File Provider that the gateway has not confirmed yet
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
	/// The content identity of the version on the gateway
	public var blocksHash: String?
	/// Incremented whenever the contents change
	public var contentRevision: Int64
	/// Incremented whenever anything about the item that the File Provider shows changes
	public var metadataRevision: Int64
	public var changedSequence: Int64
	public var pending: PendingChange
	public var isDeleted: Bool

	public var modificationDate: Date? {
		modifiedNanoseconds == 0 ? nil : Date(timeIntervalSince1970: Double(modifiedNanoseconds) / 1_000_000_000)
	}
}

public struct IndexFolder: Sendable, Hashable {
	public var folderID: String
	public var rootItemID: Int64
	public var label: String
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

/// What the system keeps about an item that gateways do not know about: tags, the favorite rank and when the item was
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
