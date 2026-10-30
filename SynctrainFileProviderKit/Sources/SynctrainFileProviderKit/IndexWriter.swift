// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public struct FolderDescription: Sendable, Hashable {
	public var folderID: String
	public var label: String

	public init(folderID: String, label: String) {
		self.folderID = folderID
		self.label = label
	}
}

/// Adds, removes and maintains the rows of folders inside one transaction
final class IndexWriter {
	private let connection: SQLiteConnection
	private var nextSequence: Int64
	private let startSequence: Int64
	private let now = Date().timeIntervalSince1970

	init(connection: SQLiteConnection) throws(IndexError) {
		self.connection = connection
		nextSequence = Int64(try IndexStore.meta(connection, "next_seq") ?? "1") ?? 1
		startSequence = nextSequence
	}

	func finish() throws(IndexError) {
		if nextSequence != startSequence {
			try IndexStore.setMeta(connection, "next_seq", String(nextSequence))
		}
	}

	private func allocateSequence() -> Int64 {
		defer { nextSequence += 1 }
		return nextSequence
	}

	/// Allocates a sequence for every item at once: the ID is unique, so the returned start + ID is too
	private func allocateSequencesByID() throws(IndexError) -> Int64 {
		let highestID = try connection.queryOne("SELECT MAX(id) FROM items", [], { $0.optionalInt64(0) }) ?? nil
		let start = nextSequence
		nextSequence = start + (highestID ?? 0) + 1
		return start
	}

	// MARK: - Folders

	/// Adds a folder (with its root item), or updates its label. Returns the ID of the root item.
	@discardableResult
	func upsertFolder(_ folder: FolderDescription) throws(IndexError) -> Int64 {
		if let existing = try connection.queryOne(
			"SELECT root_id, label FROM folders WHERE folder_id = ?", [folder.folderID], { ($0.int64(0), $0.text(1)) })
		{
			let (rootID, label) = existing
			if label != folder.label {
				try connection.run("UPDATE folders SET label = ? WHERE folder_id = ?", [folder.label, folder.folderID])
				try refreshRootNames()
			}
			return rootID
		}

		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, changed_seq)
			VALUES (?, '', NULL, ?, ?, ?, ?)
			""",
			[
				folder.folderID, folder.label, FileNames.collisionKey(folder.label), ItemKind.folderRoot.rawValue,
				allocateSequence(),
			])
		let rootID = connection.lastInsertRowID
		try connection.run(
			"INSERT INTO folders (folder_id, root_id, label) VALUES (?, ?, ?)", [folder.folderID, rootID, folder.label])
		try refreshRootNames()
		return rootID
	}

	/// Names the folder roots after the labels of their folders. Folders whose labels cannot coexist as names (in the
	/// same container) are told apart by their IDs, except for the first one in ID order. Only names that differ are
	/// written.
	private func refreshRootNames() throws(IndexError) {
		let roots = try connection.query(
			"""
			SELECT f.folder_id, f.root_id, f.label, i.name FROM folders AS f JOIN items AS i ON i.id = f.root_id
			ORDER BY f.folder_id
			"""
		) { (folderID: $0.text(0), rootID: $0.int64(1), label: $0.text(2), name: $0.text(3)) }
		var labelsSeen: Set<String> = []
		for root in roots {
			let isFirst = labelsSeen.insert(FileNames.collisionKey(root.label)).inserted
			let name = isFirst ? root.label : "\(root.label) (\(root.folderID))"
			if !name.utf8.elementsEqual(root.name.utf8) {
				try connection.run(
					"UPDATE items SET name = ?, name_key = ?, meta_rev = meta_rev + 1, changed_seq = ? WHERE id = ?",
					[name, FileNames.collisionKey(name), allocateSequence(), root.rootID])
			}
		}
	}

	/// Removes a folder and all of its items
	func removeFolder(folderID: String) throws(IndexError) {
		let start = try allocateSequencesByID()
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, meta_rev = meta_rev + 1, changed_seq = ? + id
			WHERE folder_id = ? AND deleted = 0
			""", [now, start, folderID])
		// Deletions in the folder no longer need confirmation
		try connection.run("UPDATE items SET pending = 0 WHERE folder_id = ? AND deleted = 1 AND pending != 0", [folderID])
		try connection.run("DELETE FROM folders WHERE folder_id = ?", [folderID])
		try refreshRootNames()
	}

	/// Marks the items of the folder as changed, so that the system reads them again (what can be done with them
	/// changed, for instance)
	func touchItems(folderID: String) throws(IndexError) {
		let start = try allocateSequencesByID()
		try connection.run(
			"UPDATE items SET meta_rev = meta_rev + 1, changed_seq = ? + id WHERE folder_id = ? AND deleted = 0",
			[start, folderID])
	}

	// MARK: - Maintenance

	/// Permanently removes tombstones older than the date. Anchors from before the removed tombstones expire. The rows of
	/// items whose change a gateway has not confirmed (they have an overlay) are kept: a rejected deletion brings the
	/// same row back.
	func pruneTombstones(before date: Date) throws(IndexError) {
		let cutoff = date.timeIntervalSince1970
		let prunable = """
			deleted = 1 AND pending = 0 AND deleted_at < ?
				AND NOT EXISTS (SELECT 1 FROM remote_overlays AS o WHERE o.item_id = items.id)
			"""
		guard
			let highest = try connection.queryOne(
				"SELECT MAX(changed_seq) FROM items WHERE \(prunable)", [cutoff], { $0.optionalInt64(0) }), let highest
		else {
			return
		}
		try connection.run("DELETE FROM items WHERE \(prunable)", [cutoff])
		try connection.run("DELETE FROM item_attributes WHERE item_id NOT IN (SELECT id FROM items)")
		try IndexStore.forgetPrunedRemoteRows(connection)
		let prunedBelow = Int64(try IndexStore.meta(connection, "pruned_below") ?? "0") ?? 0
		try IndexStore.setMeta(connection, "pruned_below", String(max(prunedBelow, highest)))
	}
}
