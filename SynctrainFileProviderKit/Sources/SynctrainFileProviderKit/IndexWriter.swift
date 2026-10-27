// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

public struct FolderDescription: Sendable, Hashable {
	public var folderID: String
	public var label: String
	public var folderType: String
	public var isSelective: Bool

	public init(folderID: String, label: String, folderType: String, isSelective: Bool) {
		self.folderID = folderID
		self.label = label
		self.folderType = folderType
		self.isSelective = isSelective
	}
}

public struct ApplyResult: Sendable {
	/// Paths of partial records whose full information is needed (pass these to GlobalRecordsJSON)
	public var needsFullRecords: [String] = []
	/// Number of records skipped because their name collides with an existing item
	public var collisions = 0
	/// Number of items that were added, changed or removed
	public var changed = 0
}

/// Applies Syncthing index records to the mirror inside one transaction. Obtain one through `IndexStore.write`.
public final class IndexWriter {
	private let connection: SQLiteConnection
	private var nextSequence: Int64
	private let startSequence: Int64
	private var rootIDs: [String: Int64] = [:]
	private var touchedFolders: Set<String> = []
	private let now = Date().timeIntervalSince1970

	/// The short device ID of this device, used to recognize records of our own writes
	public var localShortDeviceID: String?

	init(connection: SQLiteConnection) throws(IndexError) {
		self.connection = connection
		nextSequence = Int64(try IndexStore.meta(connection, "next_seq") ?? "1") ?? 1
		startSequence = nextSequence
		localShortDeviceID = try IndexStore.meta(connection, "local_short_id")
	}

	func finish() throws(IndexError) {
		for folderID in touchedFolders {
			try removeImpliedDirectoriesWithoutChildren(folderID: folderID)
		}
		if nextSequence != startSequence {
			try IndexStore.setMeta(connection, "next_seq", String(nextSequence))
		}
	}

	private func allocateSequence() -> Int64 {
		defer { nextSequence += 1 }
		return nextSequence
	}

	public func setLocalShortDeviceID(_ shortID: String) throws(IndexError) {
		localShortDeviceID = shortID
		try IndexStore.setMeta(connection, "local_short_id", shortID)
	}

	// MARK: - Folders

	public func folderIDs() throws(IndexError) -> [String] {
		try connection.query("SELECT folder_id FROM folders ORDER BY folder_id") { $0.text(0) }
	}

	/// Adds a folder (with its root item) or updates its label and type. Returns the ID of the root item.
	@discardableResult
	public func upsertFolder(_ folder: FolderDescription) throws(IndexError) -> Int64 {
		if let existing = try connection.queryOne(
			"SELECT root_id, label, folder_type, selective FROM folders WHERE folder_id = ?", [folder.folderID],
			{ ($0.int64(0), $0.text(1), $0.text(2), $0.bool(3)) })
		{
			let (rootID, label, folderType, selective) = existing
			if label != folder.label || folderType != folder.folderType || selective != folder.isSelective {
				try connection.run(
					"UPDATE folders SET label = ?, folder_type = ?, selective = ? WHERE folder_id = ?",
					[folder.label, folder.folderType, folder.isSelective, folder.folderID])
				try connection.run(
					"""
					UPDATE items SET name = ?, name_key = ?, selected = ?, meta_rev = meta_rev + 1, changed_seq = ?
					WHERE id = ?
					""",
					[
						folder.label, FileNames.collisionKey(folder.label), !folder.isSelective,
						allocateSequence(), rootID,
					])
			}
			rootIDs[folder.folderID] = rootID
			return rootID
		}

		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, changed_seq, selected)
			VALUES (?, '', NULL, ?, ?, ?, ?, ?)
			""",
			[
				folder.folderID, folder.label, FileNames.collisionKey(folder.label), ItemKind.folderRoot.rawValue,
				allocateSequence(), !folder.isSelective,
			])
		let rootID = connection.lastInsertRowID
		try connection.run(
			"INSERT INTO folders (folder_id, root_id, label, folder_type, selective) VALUES (?, ?, ?, ?, ?)",
			[folder.folderID, rootID, folder.label, folder.folderType, folder.isSelective])
		rootIDs[folder.folderID] = rootID
		return rootID
	}

	/// Removes a folder and all of its items
	public func removeFolder(folderID: String) throws(IndexError) {
		let ids = try connection.query(
			"SELECT id FROM items WHERE folder_id = ? AND deleted = 0 ORDER BY id DESC", [folderID]
		) { $0.int64(0) }
		for id in ids {
			try tombstone(id: id)
		}
		try connection.run("DELETE FROM folders WHERE folder_id = ?", [folderID])
		rootIDs[folderID] = nil
	}

	/// The Syncthing sequence up to which the folder has been mirrored
	public func cursor(folderID: String) throws(IndexError) -> Int64 {
		try connection.queryOne("SELECT cursor FROM folders WHERE folder_id = ?", [folderID]) { $0.int64(0) } ?? 0
	}

	public func setCursor(_ cursor: Int64, folderID: String) throws(IndexError) {
		try connection.run("UPDATE folders SET cursor = ? WHERE folder_id = ?", [cursor, folderID])
	}

	private func rootID(folderID: String) throws(IndexError) -> Int64 {
		if let cached = rootIDs[folderID] {
			return cached
		}
		guard
			let id = try connection.queryOne(
				"SELECT root_id FROM folders WHERE folder_id = ?", [folderID], { $0.int64(0) })
		else {
			throw .notFound
		}
		rootIDs[folderID] = id
		return id
	}

	// MARK: - Records

	private struct LiveRow {
		var id: Int64
		var parentID: Int64?
		var name: String
		var kind: ItemKind
		var size: Int64
		var modNs: Int64
		var blocksHash: String?
		var modifiedBy: String?
		var stSeq: Int64
		var selected: Bool
		var implied: Bool
		var pending: PendingChange
		var pendingSize: Int64?
		var pendingModNs: Int64?
	}

	private func liveRow(folderID: String, path: String) throws(IndexError) -> LiveRow? {
		try connection.queryOne(
			"""
			SELECT id, parent_id, name, kind, size, mod_ns, blocks_hash, modified_by, st_seq, selected, implied,
				pending, pending_size, pending_mod_ns
			FROM items WHERE folder_id = ? AND path = ? AND deleted = 0
			""", [folderID, path]
		) { row in
			LiveRow(
				id: row.int64(0), parentID: row.optionalInt64(1), name: row.text(2),
				kind: ItemKind(rawValue: Int(row.int64(3))) ?? .file, size: row.int64(4), modNs: row.int64(5),
				blocksHash: row.optionalText(6), modifiedBy: row.optionalText(7), stSeq: row.int64(8),
				selected: row.bool(9), implied: row.bool(10),
				pending: PendingChange(rawValue: Int(row.int64(11))) ?? .none, pendingSize: row.optionalInt64(12),
				pendingModNs: row.optionalInt64(13))
		}
	}

	private func hasLiveChildren(_ id: Int64) throws(IndexError) -> Bool {
		try connection.queryOne("SELECT 1 FROM items WHERE parent_id = ? AND deleted = 0 LIMIT 1", [id]) { _ in true }
			?? false
	}

	private func tombstone(id: Int64) throws(IndexError) {
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_size = NULL, pending_mod_ns = NULL,
				meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""", [now, allocateSequence(), id])
	}

	private func markSeen(_ id: Int64, sweepToken: Int64?) throws(IndexError) {
		if let sweepToken {
			try connection.run("UPDATE items SET sweep_mark = ? WHERE id = ?", [sweepToken, id])
		}
	}

	/// The ID of the live container at the path, creating implied directories as needed
	private func ensureContainer(folderID: String, path: String, sweepToken: Int64?) throws(IndexError) -> Int64 {
		if path.isEmpty {
			return try rootID(folderID: folderID)
		}
		if let existing = try liveRow(folderID: folderID, path: path) {
			if existing.kind.isContainer {
				return existing.id
			}
			// A file where a directory should be: the index is changing from one to the other
			if existing.pending != .none {
				throw .notAllowed("a pending file occupies the parent path \(path)")
			}
			try tombstone(id: existing.id)
		}

		let parentID = try ensureContainer(
			folderID: folderID, path: FileNames.parentPath(path), sweepToken: sweepToken)
		let name = FileNames.lastComponent(path)
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, changed_seq, implied, selected)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, (SELECT selected FROM items WHERE id = ?))
			""",
			[
				folderID, path, parentID, name, FileNames.collisionKey(name), ItemKind.directory.rawValue,
				allocateSequence(), parentID,
			])
		return connection.lastInsertRowID
	}

	/// Applies records from the Syncthing global index for one folder. When sweeping (see `beginSweep`), pass the sweep
	/// token so that the items seen are not removed by `finishSweep`.
	@discardableResult
	public func apply(_ records: [SyncthingRecord], folderID: String, sweepToken: Int64? = nil) throws(IndexError)
		-> ApplyResult
	{
		var result = ApplyResult()
		touchedFolders.insert(folderID)
		for record in records {
			if record.path.isEmpty || FileNames.isReservedPath(record.path) {
				continue
			}
			try apply(record, folderID: folderID, sweepToken: sweepToken, result: &result)
		}
		return result
	}

	private func apply(_ record: SyncthingRecord, folderID: String, sweepToken: Int64?, result: inout ApplyResult)
		throws(IndexError)
	{
		let live = try liveRow(folderID: folderID, path: record.path)

		if record.isGone {
			guard let live else {
				return
			}
			switch live.pending {
			case .create, .modify, .rename:
				// The extension's change is newer than what Syncthing knows; it will be picked up by a scan
				try markSeen(live.id, sweepToken: sweepToken)
			case .delete:
				try tombstone(id: live.id)
				result.changed += 1
			case .none:
				if live.kind.isContainer, try hasLiveChildren(live.id) {
					// Entries below remain (e.g. because they have not been processed yet), keep it as implied
					if !live.implied {
						try connection.run("UPDATE items SET implied = 1 WHERE id = ?", [live.id])
					}
					try markSeen(live.id, sweepToken: sweepToken)
				}
				else {
					try tombstone(id: live.id)
					result.changed += 1
				}
			}
			return
		}

		if !record.full {
			// Partial (snapshot) records only tell us whether something changed since we last saw it
			if let live, live.stSeq == record.seq, live.kind == record.kind {
				if live.selected != record.selected {
					try connection.run(
						"UPDATE items SET selected = ?, meta_rev = meta_rev + 1, changed_seq = ? WHERE id = ?",
						[record.selected, allocateSequence(), live.id])
					result.changed += 1
				}
				try markSeen(live.id, sweepToken: sweepToken)
			}
			else {
				result.needsFullRecords.append(record.path)
				if let live {
					try markSeen(live.id, sweepToken: sweepToken)
				}
			}
			return
		}

		let name = FileNames.lastComponent(record.path)
		let parentID = try ensureContainer(
			folderID: folderID, path: FileNames.parentPath(record.path), sweepToken: sweepToken)
		let kind = record.kind
		let size = kind == .file ? record.size : 0

		if let live {
			if live.kind != kind {
				if live.pending != .none {
					try markSeen(live.id, sweepToken: sweepToken)
					return
				}
				try tombstone(id: live.id)
				try insertOrRevive(record, folderID: folderID, parentID: parentID, sweepToken: sweepToken)
				result.changed += 1
				return
			}

			switch live.pending {
			case .create, .modify:
				// Syncthing indexed the file we wrote: adopt its blocks hash without treating it as a new version
				let isOurWrite =
					live.pendingSize == record.size && live.pendingModNs == record.modNs
					&& (localShortDeviceID == nil || record.modifiedBy == localShortDeviceID)
				if isOurWrite {
					try connection.run(
						"""
						UPDATE items SET blocks_hash = ?, modified_by = ?, st_seq = ?, size = ?, mod_ns = ?,
							selected = ?, pending = 0, pending_size = NULL, pending_mod_ns = NULL,
							meta_rev = meta_rev + 1, changed_seq = ?, sweep_mark = COALESCE(?, sweep_mark)
						WHERE id = ?
						""",
						[
							record.blocksHash, record.modifiedBy, record.seq, size, record.modNs, record.selected,
							allocateSequence(), sweepToken, live.id,
						])
					result.changed += 1
				}
				else {
					try markSeen(live.id, sweepToken: sweepToken)
				}
				return
			case .delete, .rename:
				try markSeen(live.id, sweepToken: sweepToken)
				return
			case .none:
				break
			}

			let contentChanged = kind == .file && (live.blocksHash != record.blocksHash || live.size != size)
			let metadataChanged =
				contentChanged || live.name != name || live.parentID != parentID || live.modNs != record.modNs
				|| live.selected != record.selected || live.implied
			if metadataChanged {
				try connection.run(
					"""
					UPDATE items SET name = ?, name_key = ?, parent_id = ?, size = ?, mod_ns = ?, blocks_hash = ?,
						modified_by = ?, st_seq = ?, selected = ?, implied = 0,
						content_rev = content_rev + ?, meta_rev = meta_rev + 1, changed_seq = ?,
						sweep_mark = COALESCE(?, sweep_mark)
					WHERE id = ?
					""",
					[
						name, FileNames.collisionKey(name), parentID, size, record.modNs, record.blocksHash,
						record.modifiedBy, record.seq, record.selected, contentChanged ? 1 : 0, allocateSequence(),
						sweepToken, live.id,
					])
				result.changed += 1
			}
			else {
				try connection.run(
					"""
					UPDATE items SET st_seq = ?, modified_by = ?, sweep_mark = COALESCE(?, sweep_mark) WHERE id = ?
					""", [record.seq, record.modifiedBy, sweepToken, live.id])
			}
			return
		}

		// Two entries whose names cannot coexist: keep the one we have
		let key = FileNames.collisionKey(name)
		if try connection.queryOne(
			"SELECT id FROM items WHERE folder_id = ? AND parent_id = ? AND name_key = ? AND deleted = 0",
			[folderID, parentID, key], { $0.int64(0) }) != nil
		{
			result.collisions += 1
			return
		}

		try insertOrRevive(record, folderID: folderID, parentID: parentID, sweepToken: sweepToken)
		result.changed += 1
	}

	/// Inserts an item for the record, reusing the identifier of an earlier item of the same kind at the same path, so
	/// that a file that was deleted and created again on another device shows up as an update of the same item.
	private func insertOrRevive(_ record: SyncthingRecord, folderID: String, parentID: Int64, sweepToken: Int64?)
		throws(IndexError)
	{
		let name = FileNames.lastComponent(record.path)
		let kind = record.kind
		let size = kind == .file ? record.size : 0
		let values: [any SQLBindable] = [
			parentID, name, FileNames.collisionKey(name), kind.rawValue, size, record.modNs, record.blocksHash,
			record.modifiedBy, record.seq, record.selected, allocateSequence(), sweepToken ?? 0,
		]

		if let tombstoneID = try connection.queryOne(
			"""
			SELECT id FROM items WHERE folder_id = ? AND path = ? AND deleted = 1 AND kind = ?
			ORDER BY changed_seq DESC LIMIT 1
			""", [folderID, record.path, kind.rawValue], { $0.int64(0) })
		{
			try connection.run(
				"""
				UPDATE items SET parent_id = ?, name = ?, name_key = ?, kind = ?, size = ?, mod_ns = ?,
					blocks_hash = ?, modified_by = ?, st_seq = ?, selected = ?, changed_seq = ?, sweep_mark = ?,
					deleted = 0, deleted_at = NULL, implied = 0, pending = 0,
					content_rev = content_rev + 1, meta_rev = meta_rev + 1
				WHERE id = ?
				""", values + [tombstoneID])
			return
		}

		try connection.run(
			"""
			INSERT INTO items (parent_id, name, name_key, kind, size, mod_ns, blocks_hash, modified_by, st_seq,
				selected, changed_seq, sweep_mark, folder_id, path)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			""", values + [folderID, record.path])
	}

	// MARK: - Sweeping

	/// Starts a full pass for the folder. Items not seen (through `apply` with the returned token) before
	/// `finishSweep` are removed, except for pending changes made through the File Provider.
	public func beginSweep(folderID: String) throws(IndexError) -> Int64 {
		let token = (Int64(try IndexStore.meta(connection, "sweep_token") ?? "0") ?? 0) + 1
		try IndexStore.setMeta(connection, "sweep_token", String(token))
		try connection.run("UPDATE items SET sweep_mark = ? WHERE id = ?", [token, try rootID(folderID: folderID)])
		return token
	}

	public func finishSweep(folderID: String, token: Int64) throws(IndexError) {
		touchedFolders.insert(folderID)
		let unseen = try connection.query(
			"""
			SELECT id, kind FROM items
			WHERE folder_id = ? AND deleted = 0 AND sweep_mark != ? AND pending = 0 AND implied = 0 AND kind != ?
			ORDER BY id
			""", [folderID, token, ItemKind.folderRoot.rawValue]
		) { ($0.int64(0), ItemKind(rawValue: Int($0.int64(1))) ?? .file) }

		for (id, kind) in unseen {
			if kind.isContainer, try hasLiveChildren(id) {
				try connection.run("UPDATE items SET implied = 1 WHERE id = ?", [id])
			}
			else {
				try tombstone(id: id)
			}
		}
	}

	/// Removes implied directories that no longer have anything below them
	public func removeImpliedDirectoriesWithoutChildren(folderID: String) throws(IndexError) {
		while true {
			let orphans = try connection.query(
				"""
				SELECT id FROM items AS i
				WHERE folder_id = ? AND implied = 1 AND deleted = 0 AND pending = 0
					AND NOT EXISTS (SELECT 1 FROM items AS c WHERE c.parent_id = i.id AND c.deleted = 0)
				""", [folderID]
			) { $0.int64(0) }
			if orphans.isEmpty {
				return
			}
			for id in orphans {
				try tombstone(id: id)
			}
		}
	}

	// MARK: - Maintenance

	/// Permanently removes tombstones older than the date. Anchors from before the removed tombstones expire.
	public func pruneTombstones(before date: Date) throws(IndexError) {
		let cutoff = date.timeIntervalSince1970
		guard
			let highest = try connection.queryOne(
				"SELECT MAX(changed_seq) FROM items WHERE deleted = 1 AND deleted_at < ?", [cutoff],
				{ $0.optionalInt64(0) }), let highest
		else {
			return
		}
		try connection.run("DELETE FROM items WHERE deleted = 1 AND deleted_at < ?", [cutoff])
		let prunedBelow = Int64(try IndexStore.meta(connection, "pruned_below") ?? "0") ?? 0
		try IndexStore.setMeta(connection, "pruned_below", String(max(prunedBelow, highest)))
	}
}
