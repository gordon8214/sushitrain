// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Finding items in the index by their place in a folder
enum IndexPaths {
	/// The live item in the container whose name cannot coexist with the given name
	static func child(_ connection: SQLiteConnection, of parentID: Int64, named name: String) throws(IndexError)
		-> IndexItem?
	{
		try connection.queryOne(
			"""
			SELECT \(IndexStore.itemColumns) FROM items
			WHERE parent_id = ? AND name_key = ? AND deleted = 0 LIMIT 1
			""", [parentID, FileNames.collisionKey(name)], IndexStore.item)
	}

	static func descendants(_ connection: SQLiteConnection, of item: IndexItem) throws(IndexError) -> [IndexItem] {
		let (condition, values) = subtree(below: item.path)
		return try connection.query(
			"""
			SELECT \(IndexStore.itemColumns) FROM items
			WHERE folder_id = ? AND deleted = 0 AND \(condition) ORDER BY path
			""", [item.folderID] + values, IndexStore.item)
	}

	/// Paths below `path` sort between `path + "/"` and `path + "0"` ("0" follows "/" in byte order)
	static func descendantRange(of path: String) -> (String, String) {
		(path + "/", path + "0")
	}

	/// A condition on `path` (with its values) that holds for the items below the path in its folder
	static func subtree(below path: String) -> (String, [any SQLBindable]) {
		if path.isEmpty {
			return ("path != ''", [])
		}
		let (lower, upper) = descendantRange(of: path)
		return ("path >= ? AND path < ?", [lower, upper])
	}
}
