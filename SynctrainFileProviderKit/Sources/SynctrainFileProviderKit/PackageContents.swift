// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

extension IndexStore {
	/// Makes existing packages report their corrected size after an upgrade, without changing identifiers or
	/// rebuilding the location. The marker and sequences are committed together, once per index.
	@discardableResult
	public func refreshPackageMetadata() throws(IndexError) -> Int {
		try transaction { connection throws(IndexError) in
			let key = "package_contents_metadata_v1"
			guard try Self.meta(connection, key) == nil else { return 0 }
			var sequence = Int64(try Self.meta(connection, "next_seq") ?? "1") ?? 1
			var after: Int64 = 0
			var changed = 0
			while true {
				let items = try connection.query(
					"""
					SELECT \(Self.itemColumns) FROM items
					WHERE kind = ? AND deleted = 0 AND id > ? AND instr(name, '.') > 0 ORDER BY id LIMIT 200
					""", [ItemKind.directory.rawValue, after], Self.item)
				for item in items where PackageTypes.type(ofDirectoryNamed: item.name, markedByGateway: item.isPackage) != nil {
					try connection.run(
						"UPDATE items SET meta_rev = meta_rev + 1, changed_seq = ? WHERE id = ?", [sequence, item.id])
					sequence += 1
					changed += 1
				}
				guard items.count == 200, let last = items.last else { break }
				after = last.id
			}
			try Self.setMeta(connection, "next_seq", String(sequence))
			try Self.setMeta(connection, key, "1")
			return changed
		}
	}

	/// Detects an edit, addition, removal, or move while a package is being assembled, without loading its tree.
	public func directoryContentStamp(of item: IndexItem) throws(IndexError) -> String {
		let (condition, values) = IndexPaths.subtree(below: item.path)
		return try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT COUNT(*), COALESCE(MAX(changed_seq), 0) FROM items
				WHERE folder_id = ? AND deleted = 0 AND \(condition)
				""", [item.folderID] + values, { "\($0.int64(0)).\($0.int64(1))" }) ?? "0.0"
		}
	}

	/// The bytes of regular files inside a directory, including nested packages. Sum in SQLite so that a large
	/// package's listing does not load its entire tree into the File Provider extension's memory.
	public func directoryContentSize(of item: IndexItem) throws(IndexError) -> Int64 {
		let (condition, values) = IndexPaths.subtree(below: item.path)
		return try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT COALESCE(SUM(size), 0) FROM items
				WHERE folder_id = ? AND deleted = 0 AND kind = ? AND \(condition)
				""", [item.folderID, ItemKind.file.rawValue] + values, { $0.int64(0) }) ?? 0
		}
	}
}
