// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import UniformTypeIdentifiers

/// Where the contents a file's row shows are
public enum RemoteContentSource: Sendable, Hashable {
	/// A local change the gateway has not confirmed: its staged file is the newest version
	case staged(URL)
	/// The gateway's version
	case remote(RemoteItemState)

	/// Equal for the same contents
	public var version: String {
		switch self {
		case .staged(let url): return "staged " + url.path(percentEncoded: false)
		case .remote(let state): return "remote " + (state.contentID?.rawValue ?? "")
		}
	}
}

extension IndexStore {
	/// The version of the file the item's row shows, or nil when it is not known yet (such as before the first snapshot
	/// completed, or while a local rename holds the row): that does not mean the file is gone.
	public func contentSource(of item: IndexItem, storage: RemoteStorage) throws(IndexError) -> RemoteContentSource? {
		if let path = try remoteOverlay(itemID: item.id)?.stagedPath {
			let url = storage.url(relativePath: path)
			if FileManager.default.fileExists(atPath: url.path(percentEncoded: false)) {
				return .staged(url)
			}
		}
		guard let state = try remoteItem(itemID: item.id), state.kind == .file, !state.isDeleted,
			state.contentID?.rawValue == item.blocksHash
		else {
			return nil
		}
		return .remote(state)
	}

	/// Live items whose names contain all the terms, ignoring case, in the order of their names. Folder roots are left
	/// out. Items the index does not hold yet, such as those of a folder still on its first snapshot, are not found.
	public func searchItems(terms: [String], folderID: String? = nil, limit: Int = 200) throws(IndexError)
		-> [IndexItem]
	{
		let terms = terms.filter { !$0.isEmpty }
		guard !terms.isEmpty else {
			return []
		}
		var conditions = ["deleted = 0", "kind != \(ItemKind.folderRoot.rawValue)"]
		var values: [any SQLBindable] = []
		if let folderID {
			conditions.append("folder_id = ?")
			values.append(folderID)
		}
		for term in terms {
			conditions.append("name_key LIKE ? ESCAPE '\\'")
			values.append("%" + Self.likePattern(FileNames.collisionKey(term)) + "%")
		}
		return try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT \(Self.itemColumns) FROM items WHERE \(conditions.joined(separator: " AND "))
				ORDER BY name_key, id LIMIT ?
				""", values + [limit], Self.item)
		}
	}

	/// The text with what `LIKE` treats as wildcards escaped
	private static func likePattern(_ text: String) -> String {
		text.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "%", with: "\\%")
			.replacingOccurrences(of: "_", with: "\\_")
	}
}

extension Capabilities {
	/// The feature a gateway must have to render thumbnails of the type, or nil when no gateway renders it
	public static func thumbnailFeature(for type: UTType) -> Feature? {
		if type.conforms(to: .image) {
			return .imageThumbnails
		}
		if type.conforms(to: .movie) {
			return .videoThumbnails
		}
		if type.conforms(to: .pdf) {
			return .pdfThumbnails
		}
		return nil
	}
}

extension RemoteStorage {
	/// Remembers that a cached file was used, so that it is among the last to go
	public func markContentUsed(_ url: URL) {
		utimes(url.path(percentEncoded: false), nil)
	}

	private func cachedFiles() -> [(url: URL, size: Int64, used: Date)] {
		let keys: Set<URLResourceKey> = [.totalFileAllocatedSizeKey, .contentModificationDateKey]
		let urls =
			(try? FileManager.default.contentsOfDirectory(at: contentURL, includingPropertiesForKeys: Array(keys))) ?? []
		return urls.compactMap { url in
			guard let values = try? url.resourceValues(forKeys: keys) else {
				return nil
			}
			return (url, Int64(values.totalFileAllocatedSize ?? 0), values.contentModificationDate ?? .distantPast)
		}
	}

	/// How much space the cached files take
	public var contentByteCount: Int64 {
		cachedFiles().reduce(0) { $0 + $1.size }
	}

	/// Removes the cached files used longest ago, until they take at most the given space. Cached files are copies of
	/// what gateways hold, so all this costs is downloading them again; a file being handed out elsewhere just misses
	/// the cache there. Call this from the app only, so that the extension never spends its time on it.
	public func trimContent(toBytes limit: Int64) {
		var total = cachedFiles().reduce(0) { $0 + $1.size }
		for file in cachedFiles().sorted(by: { $0.used < $1.used }) {
			if total <= limit {
				return
			}
			if (try? FileManager.default.removeItem(at: file.url)) != nil {
				total -= file.size
			}
		}
	}
}
