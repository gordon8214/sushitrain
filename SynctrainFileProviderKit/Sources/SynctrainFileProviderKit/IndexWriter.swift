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
	/// Number of records skipped because their name (or the name of a directory above them) collides with an existing
	/// item, or a file changed through the File Provider is where a directory above them should be
	public var collisions = 0
	/// Number of items that were added, changed or removed
	public var changed = 0
}

/// Applies Syncthing index records to the mirror inside one transaction. Obtain one through `IndexStore.write`.
public final class IndexWriter {
	private let connection: SQLiteConnection
	private let container: SharedContainer?
	private var nextSequence: Int64
	private let startSequence: Int64
	private var rootIDs: [String: Int64] = [:]
	private var touchedFolders: Set<String> = []
	private var pendingRenamesByFolder: [String: PendingRenames] = [:]
	private let now = Date().timeIntervalSince1970

	/// The short device ID of this device, used to recognize records of our own writes
	public var localShortDeviceID: String?

	init(connection: SQLiteConnection, container: SharedContainer? = nil) throws(IndexError) {
		self.connection = connection
		self.container = container
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

	/// Allocates a sequence for every item at once: the ID is unique, so the returned start + ID is too
	private func allocateSequencesByID() throws(IndexError) -> Int64 {
		let highestID = try connection.queryOne("SELECT MAX(id) FROM items", [], { $0.optionalInt64(0) }) ?? nil
		let start = nextSequence
		nextSequence = start + (highestID ?? 0) + 1
		return start
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
			rootIDs[folder.folderID] = rootID
			let modeChanged = folderType != folder.folderType || selective != folder.isSelective
			if label != folder.label || modeChanged {
				try connection.run(
					"UPDATE folders SET label = ?, folder_type = ?, selective = ? WHERE folder_id = ?",
					[folder.label, folder.folderType, folder.isSelective, folder.folderID])
				if modeChanged {
					// What can be done with every item (its capabilities) depends on the type and mode of its folder
					let start = try allocateSequencesByID()
					try connection.run(
						"UPDATE items SET meta_rev = meta_rev + 1, changed_seq = ? + id WHERE folder_id = ? AND deleted = 0",
						[start, folder.folderID])
				}
				try connection.run(
					"UPDATE items SET selected = ?, meta_rev = meta_rev + 1, changed_seq = ? WHERE id = ?",
					[!folder.isSelective, allocateSequence(), rootID])
			}
			try refreshRootNames()
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
	public func removeFolder(folderID: String) throws(IndexError) {
		let start = try allocateSequencesByID()
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_path = NULL, pending_size = NULL,
				pending_mod_ns = NULL, meta_rev = meta_rev + 1, changed_seq = ? + id
			WHERE folder_id = ? AND deleted = 0
			""", [now, start, folderID])
		// Deletions in the folder no longer need confirmation
		try connection.run(
			"UPDATE items SET pending = 0, pending_path = NULL WHERE folder_id = ? AND deleted = 1 AND pending != 0",
			[folderID])
		try connection.run("DELETE FROM folders WHERE folder_id = ?", [folderID])
		rootIDs[folderID] = nil
		pendingRenamesByFolder[folderID] = nil
		try refreshRootNames()
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
		var pendingAfter: Int64
	}

	/// A version of an entry as Syncthing would record it
	private struct Version {
		var kind: ItemKind
		var size: Int64
		var modNs: Int64
		/// Nil when unknown (the contents were changed through the File Provider and Syncthing has not hashed them)
		var blocksHash: String?
	}

	/// An item renamed through the File Provider while Syncthing has not seen the rename (possibly deleted since)
	private struct PendingRename {
		var id: Int64
		/// Where Syncthing knows the item
		var origin: String
		/// Where the item is now (or where it was deleted)
		var destination: String
		var after: Int64
		var version: Version
		var isDeleted: Bool
	}

	private struct PendingRenames {
		/// Renames whose old path is recorded
		var origins: [PendingRename] = []
		/// Live renamed items (paths below them are where Syncthing has not seen the entries yet)
		var destinations: [(path: String, after: Int64)] = []
	}

	private func liveRow(folderID: String, path: String) throws(IndexError) -> LiveRow? {
		try connection.queryOne(
			"""
			SELECT id, parent_id, name, kind, size, mod_ns, blocks_hash, modified_by, st_seq, selected, implied,
				pending, pending_size, pending_mod_ns, pending_after
			FROM items WHERE folder_id = ? AND path = ? AND deleted = 0
			""", [folderID, path]
		) { row in
			LiveRow(
				id: row.int64(0), parentID: row.optionalInt64(1), name: row.text(2),
				kind: ItemKind(rawValue: Int(row.int64(3))) ?? .file, size: row.int64(4), modNs: row.int64(5),
				blocksHash: row.optionalText(6), modifiedBy: row.optionalText(7), stSeq: row.int64(8),
				selected: row.bool(9), implied: row.bool(10),
				pending: PendingChange(rawValue: Int(row.int64(11))) ?? .none, pendingSize: row.optionalInt64(12),
				pendingModNs: row.optionalInt64(13), pendingAfter: row.int64(14))
		}
	}

	private func hasLiveChildren(_ id: Int64) throws(IndexError) -> Bool {
		try connection.queryOne("SELECT 1 FROM items WHERE parent_id = ? AND deleted = 0 LIMIT 1", [id]) { _ in true }
			?? false
	}

	private func tombstone(id: Int64) throws(IndexError) {
		try connection.run(
			"""
			UPDATE items SET deleted = 1, deleted_at = ?, pending = 0, pending_path = NULL, pending_size = NULL,
				pending_mod_ns = NULL, meta_rev = meta_rev + 1, changed_seq = ?
			WHERE id = ?
			""", [now, allocateSequence(), id])
	}

	private func markSeen(_ id: Int64, sweepToken: Int64?) throws(IndexError) {
		if let sweepToken {
			try connection.run("UPDATE items SET sweep_mark = ? WHERE id = ?", [sweepToken, id])
		}
	}

	/// The ID of the live container at the path, creating implied directories as needed. Nil when that is not possible:
	/// a directory on the way has a name that collides with another entry, or a pending file is in its place.
	private func ensureContainer(folderID: String, path: String) throws(IndexError) -> Int64? {
		if path.isEmpty {
			return try rootID(folderID: folderID)
		}
		let existing = try liveRow(folderID: folderID, path: path)
		if let existing, existing.kind.isContainer {
			return existing.id
		}
		guard let parentID = try ensureContainer(folderID: folderID, path: FileNames.parentPath(path)) else {
			return nil
		}
		if let existing {
			// A file where a directory should be: the index is changing from one to the other
			if existing.pending != .none {
				return nil
			}
			try tombstone(id: existing.id)
		}

		let name = FileNames.lastComponent(path)
		let key = FileNames.collisionKey(name)
		if try liveChild(folderID: folderID, parentID: parentID, key: key) != nil {
			return nil
		}
		try connection.run(
			"""
			INSERT INTO items (folder_id, path, parent_id, name, name_key, kind, changed_seq, implied, selected)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, (SELECT selected FROM items WHERE id = ?))
			""",
			[folderID, path, parentID, name, key, ItemKind.directory.rawValue, allocateSequence(), parentID])
		return connection.lastInsertRowID
	}

	/// The live entry in the container whose name has the collision key
	private func liveChild(folderID: String, parentID: Int64, key: String) throws(IndexError) -> Int64? {
		try connection.queryOne(
			"SELECT id FROM items WHERE folder_id = ? AND parent_id = ? AND name_key = ? AND deleted = 0",
			[folderID, parentID, key], { $0.int64(0) })
	}

	/// Applies records from the Syncthing global index for one folder. When sweeping (see `beginSweep`), pass the sweep
	/// token so that the items seen are not removed by `finishSweep`.
	@discardableResult
	public func apply(_ records: [SyncthingRecord], folderID: String, sweepToken: Int64? = nil) throws(IndexError)
		-> ApplyResult
	{
		var result = ApplyResult()
		touchedFolders.insert(folderID)
		for var record in records {
			// Paths with names that cannot be on disk (empty, "." or "..", or containing NUL) are not shown
			let deliveredPath = record.path
			guard !deliveredPath.isEmpty, FileNames.isValidPath(deliveredPath), !FileNames.isReservedPath(deliveredPath)
			else {
				continue
			}
			// Paths are compared byte by byte, so they must all be in the same form
			record.path = FileNames.normalized(deliveredPath)
			try apply(record, deliveredPath: deliveredPath, folderID: folderID, sweepToken: sweepToken, result: &result)
		}
		return result
	}

	private func apply(
		_ record: SyncthingRecord, deliveredPath: String, folderID: String, sweepToken: Int64?,
		result: inout ApplyResult
	) throws(IndexError) {
		var live = try liveRow(folderID: folderID, path: record.path)

		// A change made through the File Provider that Syncthing may not have indexed yet. Records from before the
		// change must not undo it; a record from after it shows what Syncthing made of it.
		if let pendingRow = live, pendingRow.pending != .none {
			if record.seq <= pendingRow.pendingAfter {
				try markSeen(pendingRow.id, sweepToken: sweepToken)
				return
			}
			if !record.isGone {
				if !record.full {
					result.needsFullRecords.append(deliveredPath)
					try markSeen(pendingRow.id, sweepToken: sweepToken)
					return
				}
				if matchesPendingChange(pendingRow, record) {
					try adopt(record, into: pendingRow, folderID: folderID, sweepToken: sweepToken)
					result.changed += 1
					return
				}
				if isEarlierOwnVersion(record, of: pendingRow, folderID: folderID) {
					try connection.run(
						"UPDATE items SET pending_after = MAX(pending_after, ?) WHERE id = ?", [record.seq, pendingRow.id])
					try markSeen(pendingRow.id, sweepToken: sweepToken)
					return
				}
			}
			// Syncthing indexed something else since (a newer version, a deletion, or the outcome of a conflict)
			try clearPending(pendingRow, folderID: folderID)
			live?.pending = .none
		}
		else if let row = live, try predatesRenameOfAncestor(record, folderID: folderID) {
			try markSeen(row.id, sweepToken: sweepToken)
			return
		}

		if live == nil {
			if record.isGone {
				try confirmDeletion(folderID: folderID, path: record.path)
				return
			}
			if try predatesLocalChange(record, folderID: folderID, sweepToken: sweepToken) {
				return
			}
		}

		if record.isGone {
			guard let live else {
				return
			}
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
				result.needsFullRecords.append(deliveredPath)
				if let live {
					try markSeen(live.id, sweepToken: sweepToken)
				}
			}
			return
		}

		let name = FileNames.lastComponent(record.path)
		guard let parentID = try ensureContainer(folderID: folderID, path: FileNames.parentPath(record.path)) else {
			result.collisions += 1
			return
		}
		let kind = record.kind
		let size = kind == .file ? record.size : 0

		if let live {
			if live.kind != kind {
				try tombstone(id: live.id)
				try insertOrRevive(record, folderID: folderID, parentID: parentID, sweepToken: sweepToken)
				result.changed += 1
				return
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
		if try liveChild(folderID: folderID, parentID: parentID, key: FileNames.collisionKey(name)) != nil {
			result.collisions += 1
			return
		}

		try insertOrRevive(record, folderID: folderID, parentID: parentID, sweepToken: sweepToken)
		result.changed += 1
	}

	/// Whether the record is Syncthing's index entry for the change made through the File Provider
	private func matchesPendingChange(_ row: LiveRow, _ record: SyncthingRecord) -> Bool {
		guard row.kind == record.kind else {
			return false
		}
		// A directory's modification time changes with its contents, so only its existence can be compared
		if row.kind.isContainer {
			return true
		}
		// A rename alone records no expected state: the item is what it was
		return (row.pendingSize ?? row.size) == record.size && (row.pendingModNs ?? row.modNs) == record.modNs
			&& (localShortDeviceID == nil || record.modifiedBy == localShortDeviceID)
	}

	/// Whether the record is Syncthing's entry for an earlier version saved on this device, while the newer version
	/// saved through the File Provider is still the file on disk. Syncthing indexes that version when it scans it (which
	/// was asked for), so the change stays pending. Once something else replaces the file, this no longer holds, and the
	/// change gives way to Syncthing's records as usual.
	private func isEarlierOwnVersion(_ record: SyncthingRecord, of row: LiveRow, folderID: String) -> Bool {
		guard let container, let localShortDeviceID, record.modifiedBy == localShortDeviceID, row.kind == .file,
			record.kind == .file, row.pending == .create || row.pending == .modify || row.pendingSize != nil,
			let onDisk = FileFingerprint(
				path: container.fileURL(folderID: folderID, path: record.path).path(percentEncoded: false))
		else {
			return false
		}
		return onDisk.matches(size: row.pendingSize ?? row.size, modifiedNanoseconds: row.pendingModNs ?? row.modNs)
	}

	/// Takes Syncthing's entry for a change made through the File Provider, without treating it as a new version
	private func adopt(_ record: SyncthingRecord, into row: LiveRow, folderID: String, sweepToken: Int64?)
		throws(IndexError)
	{
		try connection.run(
			"""
			UPDATE items SET blocks_hash = ?, modified_by = ?, st_seq = ?, size = ?, mod_ns = ?, selected = ?,
				implied = 0, pending = 0, pending_size = NULL, pending_mod_ns = NULL, pending_path = NULL,
				meta_rev = meta_rev + 1, changed_seq = ?, sweep_mark = COALESCE(?, sweep_mark)
			WHERE id = ?
			""",
			[
				record.blocksHash, record.modifiedBy, record.seq, row.kind == .file ? record.size : 0, record.modNs,
				record.selected, allocateSequence(), sweepToken, row.id,
			])
		if row.pending == .rename {
			pendingRenamesByFolder[folderID] = nil
		}
	}

	private func clearPending(_ row: LiveRow, folderID: String) throws(IndexError) {
		try connection.run(
			"""
			UPDATE items SET pending = 0, pending_size = NULL, pending_mod_ns = NULL, pending_path = NULL WHERE id = ?
			""", [row.id])
		if row.pending == .rename {
			pendingRenamesByFolder[folderID] = nil
		}
	}

	/// Syncthing has indexed the removal of the path. That confirms deletions made through the File Provider there,
	/// and that items renamed away from it (and deleted afterwards) are gone from their old path.
	private func confirmDeletion(folderID: String, path: String) throws(IndexError) {
		try connection.run(
			"""
			UPDATE items SET pending = 0, pending_path = NULL
			WHERE folder_id = ? AND path = ? AND deleted = 1 AND pending = \(PendingChange.delete.rawValue)
			""", [folderID, path])
		var changed = connection.changes > 0
		// The deletion at the new path may still need confirming (if Syncthing saw the item there at all)
		try connection.run(
			"""
			UPDATE items SET pending_path = NULL
			WHERE folder_id = ? AND deleted = 1 AND pending != 0 AND pending = \(PendingChange.delete.rawValue)
				AND pending_path = ?
			""", [folderID, path])
		changed = changed || connection.changes > 0
		if changed {
			pendingRenamesByFolder[folderID] = nil
		}
	}

	/// Whether a record for a path without a live item is from before a deletion or rename made through the File
	/// Provider (so it would bring back what was just deleted or moved away). Records up to the sequence the change was
	/// recorded after are. So is a newer full record of the very version that was deleted or moved: one that Syncthing
	/// indexed before the change, but that the app had not applied yet (such as its scan of an earlier change made
	/// through the File Provider). Syncthing's record of the change itself is a deletion at this path, or an entry at
	/// another path, so it is never mistaken for one of these.
	private func predatesLocalChange(_ record: SyncthingRecord, folderID: String, sweepToken: Int64?)
		throws(IndexError) -> Bool
	{
		if let marker = try deletionMarker(folderID: folderID, path: record.path) {
			try markSeen(marker.id, sweepToken: sweepToken)
			if record.seq <= marker.after || describes(record, marker.version) {
				return true
			}
		}

		for rename in try pendingRenames(folderID: folderID).origins {
			guard let rest = Self.remainder(of: record.path, below: rename.origin) else {
				continue
			}
			if rename.isDeleted {
				try markSeen(rename.id, sweepToken: sweepToken)
			}
			if record.seq <= rename.after {
				return true
			}
			if rest.isEmpty {
				if describes(record, rename.version) {
					return true
				}
			}
			else if let entry = try entry(folderID: folderID, path: rename.destination + rest) {
				// The entry that was below the renamed item, where it is now
				if entry.isMarker {
					try markSeen(entry.id, sweepToken: sweepToken)
				}
				if describes(record, entry.version) {
					return true
				}
			}
		}
		return false
	}

	/// Whether a record for a live item below an item renamed through the File Provider is from before the rename: it
	/// describes what Syncthing had at the path before the item moved there
	private func predatesRenameOfAncestor(_ record: SyncthingRecord, folderID: String) throws(IndexError) -> Bool {
		try pendingRenames(folderID: folderID).destinations.contains { destination in
			record.seq <= destination.after && Self.remainder(of: record.path, below: destination.path)?.isEmpty == false
		}
	}

	/// Whether a full record describes the version of an entry. For a directory, whose modification time changes with
	/// its contents (and which Syncthing only records anew when it is created or its permissions change), that is a
	/// record made by this device.
	private func describes(_ record: SyncthingRecord, _ version: Version) -> Bool {
		guard record.full, record.kind == version.kind else {
			return false
		}
		if version.kind.isContainer {
			return localShortDeviceID == nil || record.modifiedBy == localShortDeviceID
		}
		if let known = version.blocksHash, let hash = record.blocksHash, known != hash {
			return false
		}
		return record.size == version.size && record.modNs == version.modNs
	}

	/// The latest deletion made through the File Provider at the path that Syncthing has not confirmed
	private func deletionMarker(folderID: String, path: String) throws(IndexError) -> (
		id: Int64, after: Int64, version: Version
	)? {
		try connection.queryOne(
			"""
			SELECT id, pending_after, kind, COALESCE(pending_size, size), COALESCE(pending_mod_ns, mod_ns), blocks_hash
			FROM items
			WHERE folder_id = ? AND path = ? AND deleted = 1 AND pending = \(PendingChange.delete.rawValue)
			ORDER BY changed_seq DESC LIMIT 1
			""", [folderID, path]
		) { row in
			(
				id: row.int64(0), after: row.int64(1),
				version: Version(
					kind: ItemKind(rawValue: Int(row.int64(2))) ?? .file, size: row.int64(3), modNs: row.int64(4),
					blocksHash: row.optionalText(5))
			)
		}
	}

	/// The live item at the path, or else the latest unconfirmed deletion there
	private func entry(folderID: String, path: String) throws(IndexError) -> (
		id: Int64, isMarker: Bool, version: Version
	)? {
		if let live = try liveRow(folderID: folderID, path: path) {
			let hashIsCurrent = live.pending == .none || (live.pending == .rename && live.pendingSize == nil)
			return (
				live.id, false,
				Version(
					kind: live.kind, size: live.size, modNs: live.modNs, blocksHash: hashIsCurrent ? live.blocksHash : nil)
			)
		}
		return try deletionMarker(folderID: folderID, path: path).map { ($0.id, true, $0.version) }
	}

	/// Items renamed through the File Provider while Syncthing has not seen the rename (loaded once, and again after
	/// they change)
	private func pendingRenames(folderID: String) throws(IndexError) -> PendingRenames {
		if let cached = pendingRenamesByFolder[folderID] {
			return cached
		}
		var renames = PendingRenames()
		let row = { (row: SQLiteStatement, isDeleted: Bool) in
			(
				id: row.int64(0), path: row.text(1), origin: row.optionalText(2), after: row.int64(3),
				version: Version(
					kind: ItemKind(rawValue: Int(row.int64(4))) ?? .file, size: row.int64(5), modNs: row.int64(6),
					blocksHash: row.optionalText(7)),
				isDeleted: isDeleted
			)
		}
		// The hash of a renamed item is current unless its contents changed as well
		let live = try connection.query(
			"""
			SELECT id, path, pending_path, pending_after, kind, size, mod_ns,
				CASE WHEN pending_size IS NULL THEN blocks_hash END
			FROM items
			WHERE folder_id = ? AND deleted = 0 AND pending = ?
			""", [folderID, PendingChange.rename.rawValue]
		) { row($0, false) }
		let deleted = try connection.query(
			"""
			SELECT id, path, pending_path, pending_after, kind, COALESCE(pending_size, size),
				COALESCE(pending_mod_ns, mod_ns), blocks_hash
			FROM items
			WHERE folder_id = ? AND deleted = 1 AND pending != 0 AND pending = \(PendingChange.delete.rawValue)
				AND pending_path IS NOT NULL
			""", [folderID]
		) { row($0, true) }
		for entry in live {
			renames.destinations.append((entry.path, entry.after))
		}
		for entry in live + deleted {
			if let origin = entry.origin {
				renames.origins.append(
					PendingRename(
						id: entry.id, origin: origin, destination: entry.path, after: entry.after,
						version: entry.version, isDeleted: entry.isDeleted))
			}
		}
		pendingRenamesByFolder[folderID] = renames
		return renames
	}

	/// The rest of the path below the ancestor: "" for the ancestor itself, "/..." below it, and nil elsewhere. Bytes
	/// are compared, as the index does.
	private static func remainder(of path: String, below ancestor: String) -> String? {
		let bytes = path.utf8
		guard bytes.starts(with: ancestor.utf8) else {
			return nil
		}
		let rest = bytes.dropFirst(ancestor.utf8.count)
		if rest.isEmpty {
			return ""
		}
		return rest.first == UInt8(ascii: "/") ? String(decoding: rest, as: UTF8.self) : nil
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

		if let tombstone = try connection.queryOne(
			"""
			SELECT id, pending FROM items WHERE folder_id = ? AND path = ? AND deleted = 1 AND kind = ?
			ORDER BY changed_seq DESC LIMIT 1
			""", [folderID, record.path, kind.rawValue], { (id: $0.int64(0), pending: $0.int64(1)) })
		{
			try connection.run(
				"""
				UPDATE items SET parent_id = ?, name = ?, name_key = ?, kind = ?, size = ?, mod_ns = ?,
					blocks_hash = ?, modified_by = ?, st_seq = ?, selected = ?, changed_seq = ?, sweep_mark = ?,
					deleted = 0, deleted_at = NULL, implied = 0, pending = 0, pending_path = NULL, pending_size = NULL,
					pending_mod_ns = NULL, pending_after = 0, content_rev = content_rev + 1, meta_rev = meta_rev + 1
				WHERE id = ?
				""", values + [tombstone.id])
			if tombstone.pending != 0 {
				pendingRenamesByFolder[folderID] = nil
			}
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
		// Changes recorded from here on may be newer than what the pass gets to see
		try IndexStore.setMeta(connection, Self.sweepStartKey(folderID), String(nextSequence))
		try connection.run("UPDATE items SET sweep_mark = ? WHERE id = ?", [token, try rootID(folderID: folderID)])
		return token
	}

	private static func sweepStartKey(_ folderID: String) -> String {
		"sweep_start:\(folderID)"
	}

	public func finishSweep(folderID: String, token: Int64) throws(IndexError) {
		touchedFolders.insert(folderID)
		// Entries below a directory renamed through the File Provider are where Syncthing has not seen them yet
		let renamed = try pendingRenames(folderID: folderID).destinations.map(\.path)
		let unseen = try connection.query(
			"""
			SELECT id, kind, path FROM items
			WHERE folder_id = ? AND deleted = 0 AND sweep_mark != ? AND pending = 0 AND implied = 0 AND kind != ?
			ORDER BY id
			""", [folderID, token, ItemKind.folderRoot.rawValue]
		) { (id: $0.int64(0), kind: ItemKind(rawValue: Int($0.int64(1))) ?? .file, path: $0.text(2)) }

		for entry in unseen {
			if renamed.contains(where: { Self.remainder(of: entry.path, below: $0)?.isEmpty == false }) {
				continue
			}
			if entry.kind.isContainer, try hasLiveChildren(entry.id) {
				try connection.run("UPDATE items SET implied = 1 WHERE id = ?", [entry.id])
			}
			else {
				try tombstone(id: entry.id)
			}
		}

		// A deletion made through the File Provider that no record touched: Syncthing has no entry for the path (it never
		// saw what was deleted, e.g. a file deleted right after it was created), so nothing can bring it back and there is
		// nothing to confirm. Deletions recorded after the pass began are left alone: the pass may predate their records.
		let startKey = Self.sweepStartKey(folderID)
		if let start = Int64(try IndexStore.meta(connection, startKey) ?? "") {
			try connection.run(
				"""
				UPDATE items SET pending = 0, pending_path = NULL
				WHERE folder_id = ? AND deleted = 1 AND pending != 0 AND pending = \(PendingChange.delete.rawValue)
					AND sweep_mark != ? AND changed_seq < ?
				""", [folderID, token, start])
			try connection.run("DELETE FROM meta WHERE key = ?", [startKey])
			pendingRenamesByFolder[folderID] = nil
		}
	}

	/// Removes implied directories that no longer have anything below them
	public func removeImpliedDirectoriesWithoutChildren(folderID: String) throws(IndexError) {
		while true {
			// Without statistics, SQLite would rather use the index of pending items (pending = 0 matches nearly all)
			let orphans = try connection.query(
				"""
				SELECT id FROM items AS i INDEXED BY items_implied
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

	/// Marks every item as changed, so that the system reads them all again (for instance because the extension now
	/// describes items differently). Each item gets its own sequence: the ID is unique, so start + ID is too.
	public func touchAllItems() throws(IndexError) {
		let start = try allocateSequencesByID()
		try connection.run(
			"UPDATE items SET meta_rev = meta_rev + 1, changed_seq = ? + id WHERE deleted = 0", [start])
	}

	/// Permanently removes tombstones older than the date. Anchors from before the removed tombstones expire. Deletions
	/// made through the File Provider that Syncthing has not confirmed yet are kept: they keep older records from
	/// bringing the items back.
	public func pruneTombstones(before date: Date) throws(IndexError) {
		let cutoff = date.timeIntervalSince1970
		guard
			let highest = try connection.queryOne(
				"SELECT MAX(changed_seq) FROM items WHERE deleted = 1 AND pending = 0 AND deleted_at < ?", [cutoff],
				{ $0.optionalInt64(0) }), let highest
		else {
			return
		}
		try connection.run("DELETE FROM items WHERE deleted = 1 AND pending = 0 AND deleted_at < ?", [cutoff])
		try connection.run("DELETE FROM item_attributes WHERE item_id NOT IN (SELECT id FROM items)")
		let prunedBelow = Int64(try IndexStore.meta(connection, "pruned_below") ?? "0") ?? 0
		try IndexStore.setMeta(connection, "pruned_below", String(max(prunedBelow, highest)))
	}
}
