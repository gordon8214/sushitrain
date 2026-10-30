// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

extension IndexStore {
	/// Adds a folder of a gateway to this device: its root item, its folder row and its binding. Returns the local
	/// folder ID, which is the gateway's unless another folder has it already. A folder that was added before stays as
	/// it is. Its items follow with the next refresh.
	@discardableResult
	public func addRemoteFolder(_ remoteFolder: RemoteFolder, serviceID: String) throws(IndexError) -> String {
		try transaction { connection throws(IndexError) in
			if let existing = try Self.localFolderID(
				connection, serviceID: serviceID, remoteFolderID: remoteFolder.id)
			{
				return existing
			}
			let folderID = try Self.unusedFolderID(connection, for: remoteFolder, serviceID: serviceID)
			let writer = try IndexWriter(connection: connection)
			try writer.upsertFolder(
				FolderDescription(folderID: folderID, label: remoteFolder.label.isEmpty ? remoteFolder.id : remoteFolder.label))
			try writer.finish()
			try Self.insertBinding(
				connection,
				RemoteFolderBinding(
					folderID: folderID, serviceID: serviceID, remoteFolderID: remoteFolder.id,
					permissions: remoteFolder.permissions), rootGatewayID: remoteFolder.rootItemID)
			return folderID
		}
	}

	/// The gateway's own folder ID, unless a folder of this device (of another gateway, say) has it
	private static func unusedFolderID(
		_ connection: SQLiteConnection, for remoteFolder: RemoteFolder, serviceID: String
	) throws(IndexError) -> String {
		func isTaken(_ id: String) throws(IndexError) -> Bool {
			try connection.queryOne(
				"""
				SELECT EXISTS (SELECT 1 FROM folders WHERE folder_id = ?)
					OR EXISTS (SELECT 1 FROM remote_bindings WHERE folder_id = ?)
				""", [id, id], { $0.bool(0) }) ?? false
		}
		if try !isTaken(remoteFolder.id) {
			return remoteFolder.id
		}
		let base = "\(remoteFolder.id)~\(serviceID.suffix(8))"
		var candidate = base
		var counter = 1
		while try isTaken(candidate) {
			counter += 1
			candidate = "\(base)-\(counter)"
		}
		return candidate
	}

	/// Removes a folder from this device; it stays on its gateway. Its items are reported as deleted, so that Files lets
	/// go of them, and what the device kept for the folder is forgotten. Refused while edits of the folder (or moves
	/// into it) are unresolved. Returns the staged and partial files to remove from disk, with
	/// `RemoteUploader.removeReleasedStagedFiles`.
	@discardableResult
	public func removeRemoteFolder(folderID: String) throws(IndexError) -> [String] {
		try transaction { connection throws(IndexError) in
			if try connection.queryOne(
				"""
				SELECT EXISTS (SELECT 1 FROM remote_outbox
					WHERE (folder_id = ? OR target_folder_id = ?) AND \(Self.unresolvedCondition))
				""", [folderID, folderID], { $0.bool(0) }) == true
			{
				throw .notAllowed("changes to this folder were not resolved with its gateway")
			}

			let items = "SELECT id FROM items WHERE folder_id = ?"
			var paths = try connection.query(
				"SELECT staged_path FROM remote_outbox WHERE folder_id = ? AND staged_path IS NOT NULL", [folderID],
				{ $0.text(0) })
			paths += try connection.query(
				"SELECT staged_path FROM remote_overlays WHERE folder_id = ? AND staged_path IS NOT NULL", [folderID],
				{ $0.text(0) })
			paths += try connection.query(
				"SELECT path FROM remote_staged_files WHERE item_id IN (\(items))", [folderID], { $0.text(0) })
			paths += try connection.query(
				"SELECT staging_path FROM remote_transfers WHERE item_id IN (\(items)) AND staging_path IS NOT NULL",
				[folderID], { $0.text(0) })

			try Self.releaseTransferTokens(connection, where: "item_id IN (\(items))", [folderID])
			try connection.run(
				"""
				DELETE FROM remote_transfer_slots WHERE transfer_id IN
					(SELECT transfer_id FROM remote_transfers WHERE item_id IN (\(items)))
				""", [folderID])
			try connection.run("DELETE FROM remote_transfers WHERE item_id IN (\(items))", [folderID])
			for table in ["remote_outbox", "remote_overlays", "remote_items", "remote_cursors", "remote_snapshots"] {
				try connection.run("DELETE FROM \(table) WHERE folder_id = ?", [folderID])
			}
			try connection.run("DELETE FROM remote_discards WHERE folder_id = ?", [folderID])
			try connection.run("DELETE FROM remote_bindings WHERE folder_id = ?", [folderID])

			let writer = try IndexWriter(connection: connection)
			try writer.removeFolder(folderID: folderID)
			try writer.finish()
			return Array(Set(paths)).sorted()
		}
	}
}
