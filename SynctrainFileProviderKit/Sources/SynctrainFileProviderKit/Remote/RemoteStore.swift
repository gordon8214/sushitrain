// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The result of storing remote metadata
public struct RemoteApplyResult: Sendable, Equatable {
	/// Number of items the system sees added, changed or removed
	public var changed = 0
	/// Whether the page was stored. False when another process stored it (or moved the cursor) first; read the sync
	/// state again and continue from there.
	public var stored = true
	/// Whether this page completed a snapshot
	public var completedSnapshot = false

	public init(changed: Int = 0, stored: Bool = true, completedSnapshot: Bool = false) {
		self.changed = changed
		self.stored = stored
		self.completedSnapshot = completedSnapshot
	}
}

/// What the device may hold of an item's contents, in a folder accessed through a gateway
public struct RemoteLocalContent: Sendable, Hashable {
	/// The content identity of the confirmed remote version (nil for directories)
	public var contentID: ContentIdentity?
	/// The staged file of the item's overlay: the contents its row shows while the gateway has not confirmed the
	/// local change (nil without an overlay, and for changes without contents)
	public var overlayStagedPath: String?
	/// A staged file whose bytes the gateway committed as `contentID`, which serves as a cached copy of it
	public var committedStagedPath: String?
	/// Who made the row, when its overlay is that of a pending creation (nil for rows that existed before the change)
	public var overlayOrigin: RemoteChangeOrigin?

	public init(
		contentID: ContentIdentity?, overlayStagedPath: String?, committedStagedPath: String?,
		overlayOrigin: RemoteChangeOrigin? = nil
	) {
		self.contentID = contentID
		self.overlayStagedPath = overlayStagedPath
		self.committedStagedPath = committedStagedPath
		self.overlayOrigin = overlayOrigin
	}
}

/// How far a folder's remote metadata is stored
public struct RemoteSyncState: Sendable, Hashable {
	public var folderID: String
	/// The change cursor up to which the folder's metadata is stored, or nil until a snapshot completes
	public var cursor: String?
	/// The snapshot in progress, if any
	public var snapshot: SnapshotSession?

	public struct SnapshotSession: Sendable, Hashable {
		/// The gateway's session (nil before the first page is stored)
		public var snapshotID: String?
		/// The change cursor at the start of the session
		public var startCursor: String?
		/// The page to request next (nil for the first page, and once all pages are stored)
		public var nextPage: String?
		/// Where the changes after the snapshot's start continue from, once all pages are stored
		public var catchUpCursor: String?
		public var pages: Int
		public var startedAt: Date
		/// Items stored during this session carry this mark; at completion, items without it are gone
		var mark: Int64

		/// Whether all pages are stored and the changes after the start cursor are being applied
		public var isCatchingUp: Bool { catchUpCursor != nil }
	}

	/// Whether a snapshot must be taken (or continued) before changes can be applied
	public var needsSnapshot: Bool { cursor == nil || snapshot != nil }
}

extension IndexStore {
	// MARK: - Schema

	/// Version 5: tables for folders accessed through a gateway. Remote metadata is projected into `items` (see
	/// `RemoteProjector`), so the enumerators, sync anchors, attributes and item identifiers work unchanged.
	static func createRemoteSchema(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute(
			"""
			CREATE TABLE IF NOT EXISTS remote_endpoints (
				service_id TEXT PRIMARY KEY,
				host TEXT NOT NULL,
				port INTEGER NOT NULL,
				spki_hash TEXT,
				next_spki_hash TEXT,
				display_name TEXT NOT NULL,
				authority_generation INTEGER,
				updated_at REAL NOT NULL
			);
			CREATE TABLE IF NOT EXISTS remote_bindings (
				folder_id TEXT PRIMARY KEY,
				access_mode TEXT NOT NULL,
				service_id TEXT,
				remote_folder_id TEXT,
				root_gateway_id TEXT,
				can_read INTEGER NOT NULL DEFAULT 0,
				can_write INTEGER NOT NULL DEFAULT 0,
				updated_at REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_bindings_remote ON remote_bindings(service_id, remote_folder_id);
			CREATE TABLE IF NOT EXISTS remote_items (
				gateway_id TEXT PRIMARY KEY,
				item_id INTEGER,
				folder_id TEXT NOT NULL,
				parent_gateway_id TEXT,
				path TEXT NOT NULL,
				name TEXT NOT NULL,
				kind TEXT NOT NULL,
				revision TEXT NOT NULL,
				content_id TEXT,
				size INTEGER NOT NULL DEFAULT 0,
				modified_ns INTEGER NOT NULL DEFAULT 0,
				mode INTEGER NOT NULL DEFAULT 0,
				writable INTEGER NOT NULL DEFAULT 0,
				subtree_generation INTEGER,
				availability TEXT,
				deleted INTEGER NOT NULL DEFAULT 0,
				placed INTEGER NOT NULL DEFAULT 0,
				seen_mark INTEGER NOT NULL DEFAULT 0,
				updated_at REAL NOT NULL
			);
			CREATE UNIQUE INDEX IF NOT EXISTS remote_items_item ON remote_items(item_id) WHERE item_id IS NOT NULL;
			CREATE INDEX IF NOT EXISTS remote_items_waiting ON remote_items(folder_id) WHERE placed = 0 AND deleted = 0;
			CREATE INDEX IF NOT EXISTS remote_items_seen ON remote_items(folder_id, seen_mark);
			CREATE TABLE IF NOT EXISTS remote_overlays (
				item_id INTEGER PRIMARY KEY,
				folder_id TEXT NOT NULL,
				kind INTEGER NOT NULL,
				operation_id TEXT NOT NULL,
				staged_path TEXT,
				created_at REAL NOT NULL
			);
			CREATE TABLE IF NOT EXISTS remote_cursors (
				folder_id TEXT PRIMARY KEY,
				cursor TEXT,
				updated_at REAL NOT NULL
			);
			CREATE TABLE IF NOT EXISTS remote_snapshots (
				folder_id TEXT PRIMARY KEY,
				snapshot_id TEXT,
				start_cursor TEXT,
				next_page TEXT,
				catch_up_cursor TEXT,
				mark INTEGER NOT NULL,
				pages INTEGER NOT NULL DEFAULT 0,
				started_at REAL NOT NULL
			);
			CREATE TABLE IF NOT EXISTS remote_outbox (
				operation_id TEXT PRIMARY KEY,
				folder_id TEXT NOT NULL,
				service_id TEXT,
				item_id INTEGER,
				gateway_id TEXT,
				kind TEXT NOT NULL,
				payload TEXT,
				base_revision TEXT,
				subtree_generation INTEGER,
				target_parent_id INTEGER,
				target_name TEXT,
				target_folder_id TEXT,
				item_kind INTEGER,
				modified_ns INTEGER,
				staged_path TEXT,
				upload_digest TEXT,
				upload_length INTEGER,
				upload_id TEXT,
				upload_offset INTEGER NOT NULL DEFAULT 0,
				upload_token_ref TEXT,
				upload_expires REAL,
				server_operation_id TEXT,
				state TEXT NOT NULL,
				claim_owner TEXT,
				claim_expires REAL,
				generation INTEGER NOT NULL DEFAULT 0,
				attempts INTEGER NOT NULL DEFAULT 0,
				not_before REAL,
				last_error TEXT,
				authority_generation INTEGER,
				committed_content_id TEXT,
				result TEXT,
				depends_on TEXT,
				parent_operation_id TEXT,
				step INTEGER,
				created_at REAL NOT NULL,
				updated_at REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_outbox_state ON remote_outbox(state, created_at);
			CREATE INDEX IF NOT EXISTS remote_outbox_item ON remote_outbox(item_id);
			CREATE INDEX IF NOT EXISTS remote_outbox_depends ON remote_outbox(depends_on) WHERE depends_on IS NOT NULL;
			CREATE INDEX IF NOT EXISTS remote_outbox_parent ON remote_outbox(parent_operation_id)
				WHERE parent_operation_id IS NOT NULL;
			CREATE TABLE IF NOT EXISTS remote_transfers (
				transfer_id TEXT PRIMARY KEY,
				kind TEXT NOT NULL,
				item_id INTEGER,
				operation_id TEXT,
				lease_id TEXT,
				token_ref TEXT,
				content_id TEXT,
				etag TEXT,
				total_size INTEGER NOT NULL DEFAULT 0,
				block_count INTEGER NOT NULL DEFAULT 0,
				verified_blocks TEXT,
				verified_count INTEGER NOT NULL DEFAULT 0,
				manifest_verified INTEGER NOT NULL DEFAULT 0,
				owner TEXT,
				claim_expires REAL,
				session_identifier TEXT,
				task_identifier INTEGER,
				task_description TEXT,
				task_offset INTEGER,
				staging_path TEXT,
				state TEXT NOT NULL,
				last_error TEXT,
				created_at REAL NOT NULL,
				updated_at REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_transfers_item ON remote_transfers(item_id, content_id);
			CREATE UNIQUE INDEX IF NOT EXISTS remote_transfers_task ON remote_transfers(task_description)
				WHERE task_description IS NOT NULL;
			CREATE TABLE IF NOT EXISTS remote_transfer_slots (
				slot_id TEXT PRIMARY KEY,
				slot_class TEXT NOT NULL,
				owner TEXT NOT NULL,
				transfer_id TEXT,
				expires REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_transfer_slots_class ON remote_transfer_slots(slot_class, expires);
			CREATE TABLE IF NOT EXISTS remote_staged_files (
				path TEXT PRIMARY KEY,
				item_id INTEGER,
				operation_id TEXT,
				size INTEGER NOT NULL,
				digest TEXT NOT NULL,
				content_id TEXT,
				created_at REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_staged_content ON remote_staged_files(content_id)
				WHERE content_id IS NOT NULL;
			CREATE TABLE IF NOT EXISTS remote_migrations (
				folder_id TEXT PRIMARY KEY,
				state TEXT NOT NULL,
				journal TEXT,
				updated_at REAL NOT NULL
			);
			""")
		// The stable client identity, kept across credential rotation and re-pairing (and rebuilds of the location)
		try connection.run(
			"INSERT OR IGNORE INTO meta (key, value) VALUES ('client_id', ?)", [UUID().uuidString.lowercased()])
	}

	/// Version 6: what a replace does when the file changed since its base revision (`OperationRequest.onConflict`),
	/// for edits the system asks to have rejected rather than kept as a conflict copy
	static func addOutboxConflictPolicy(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute("ALTER TABLE remote_outbox ADD COLUMN on_conflict TEXT")
	}

	/// Version 7: who made the rows that local changes created (`RemoteChangeOrigin`: only those the system made are
	/// left out of enumerations while their creation is pending), the identifier the system gave the pending item of a
	/// creation, and the changes the user discarded (`RemoteDiscard`), which the system may still try to make again
	static func addOriginsAndDiscards(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute(
			"""
			ALTER TABLE remote_overlays ADD COLUMN origin TEXT;
			ALTER TABLE remote_outbox ADD COLUMN template_identifier TEXT;
			CREATE INDEX IF NOT EXISTS remote_outbox_template ON remote_outbox(template_identifier)
				WHERE template_identifier IS NOT NULL;
			CREATE TABLE IF NOT EXISTS remote_discards (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				operation_id TEXT NOT NULL,
				folder_id TEXT NOT NULL,
				kind TEXT NOT NULL,
				item_id INTEGER,
				template_identifier TEXT,
				parent_item_id INTEGER,
				name TEXT,
				item_kind INTEGER,
				digest TEXT,
				discarded_at REAL NOT NULL
			);
			CREATE INDEX IF NOT EXISTS remote_discards_operation ON remote_discards(operation_id);
			CREATE INDEX IF NOT EXISTS remote_discards_item ON remote_discards(item_id) WHERE kind = 'replace';
			CREATE INDEX IF NOT EXISTS remote_discards_template ON remote_discards(template_identifier)
				WHERE kind = 'create';
			""")
		// Pending creations recorded before: the system made them, except the conflict copies the extension made (named
		// like Syncthing's) and the copies of cross-host moves (rows the system knows, which keep no origin)
		try connection.run(
			"""
			UPDATE remote_overlays SET origin = '\(RemoteChangeOrigin.system.rawValue)'
			WHERE kind = \(PendingChange.create.rawValue) AND EXISTS (
				SELECT 1 FROM remote_outbox AS m
				WHERE m.item_id = remote_overlays.item_id AND m.kind = 'create' AND m.parent_operation_id IS NULL)
			""")
		try connection.run(
			"""
			UPDATE remote_overlays SET origin = '\(RemoteChangeOrigin.provider.rawValue)'
			WHERE origin IS NOT NULL AND EXISTS (
				SELECT 1 FROM items AS i WHERE i.id = remote_overlays.item_id AND instr(i.name, '.sync-conflict-') > 0)
			""")
	}

	static func remoteManagedFolderIDs(_ connection: SQLiteConnection) throws(IndexError) -> Set<String> {
		Set(
			try connection.query(
				"SELECT folder_id FROM remote_bindings WHERE access_mode = ?", [RemoteAccessMode.remoteAPI.rawValue]
			) { $0.text(0) })
	}

	static func hasUnresolvedRemoteMutations(_ connection: SQLiteConnection) throws(IndexError) -> Bool {
		let terminal = MutationState.allCases.filter(\.isTerminal).map { "'\($0.rawValue)'" }.joined(separator: ", ")
		return try connection.queryOne(
			"SELECT EXISTS (SELECT 1 FROM remote_outbox WHERE state NOT IN (\(terminal)))", [], { $0.bool(0) }) ?? false
	}

	/// After tombstones were pruned: remote records no longer point at removed rows, and remote deletions whose rows
	/// are gone are forgotten
	static func forgetPrunedRemoteRows(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.run(
			"""
			UPDATE remote_items SET item_id = NULL
			WHERE item_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM items WHERE items.id = remote_items.item_id)
			""")
		try connection.run("DELETE FROM remote_items WHERE deleted = 1 AND item_id IS NULL")
	}

	// MARK: - Client identity

	/// The stable client identity sent when pairing and used by gateways to deduplicate operations
	public func remoteClientID() throws(IndexError) -> String {
		try withConnection { connection throws(IndexError) in
			if let id = try Self.meta(connection, "client_id") {
				return id
			}
			let id = UUID().uuidString.lowercased()
			try connection.run("INSERT OR IGNORE INTO meta (key, value) VALUES ('client_id', ?)", [id])
			return try Self.meta(connection, "client_id") ?? id
		}
	}

	// MARK: - Endpoints

	static let endpointColumns = "service_id, host, port, spki_hash, next_spki_hash, display_name"

	static func endpoint(from row: SQLiteStatement) -> RemoteEndpoint {
		RemoteEndpoint(
			serviceID: row.text(0), host: row.text(1), port: Int(row.int64(2)), pinnedSPKIHash: row.optionalText(3),
			nextPinnedSPKIHash: row.optionalText(4), displayName: row.text(5))
	}

	/// Adds a paired gateway or updates its address, pins and name (its credential is kept in the Keychain)
	public func saveRemoteEndpoint(_ endpoint: RemoteEndpoint) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"""
				INSERT INTO remote_endpoints (service_id, host, port, spki_hash, next_spki_hash, display_name, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(service_id) DO UPDATE SET host = excluded.host, port = excluded.port,
					spki_hash = excluded.spki_hash, next_spki_hash = excluded.next_spki_hash,
					display_name = excluded.display_name, updated_at = excluded.updated_at
				""",
				[
					endpoint.serviceID, endpoint.host, endpoint.port, endpoint.pinnedSPKIHash,
					endpoint.nextPinnedSPKIHash, endpoint.displayName, Date().timeIntervalSince1970,
				])
		}
	}

	public func remoteEndpoint(serviceID: String) throws(IndexError) -> RemoteEndpoint? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.endpointColumns) FROM remote_endpoints WHERE service_id = ?", [serviceID], Self.endpoint)
		}
	}

	public func remoteEndpoints() throws(IndexError) -> [RemoteEndpoint] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.endpointColumns) FROM remote_endpoints ORDER BY display_name, service_id", [],
				Self.endpoint)
		}
	}

	/// The endpoint serving the host and port (to evaluate the server trust of background tasks)
	public func remoteEndpoint(host: String, port: Int) throws(IndexError) -> RemoteEndpoint? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"""
				SELECT \(Self.endpointColumns) FROM remote_endpoints WHERE host = ? COLLATE NOCASE AND port = ? LIMIT 1
				""", [host, port], Self.endpoint)
		}
	}

	/// Removes a gateway that no folder is bound to anymore
	public func removeRemoteEndpoint(serviceID: String) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			if try connection.queryOne(
				"SELECT EXISTS (SELECT 1 FROM remote_bindings WHERE service_id = ? AND access_mode = ?)",
				[serviceID, RemoteAccessMode.remoteAPI.rawValue], { $0.bool(0) }) == true
			{
				throw .notAllowed("folders are still accessed through this gateway")
			}
			try connection.run("DELETE FROM remote_endpoints WHERE service_id = ?", [serviceID])
		}
	}

	/// The authority generation last seen for the gateway
	public func remoteAuthorityGeneration(serviceID: String) throws(IndexError) -> Int64? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT authority_generation FROM remote_endpoints WHERE service_id = ?", [serviceID],
				{ $0.optionalInt64(0) }) ?? nil
		}
	}

	/// Records the gateway's authority generation (from its capabilities). When it differs from the one seen before,
	/// the gateway may have lost state: the folders it serves take a fresh snapshot, and operations whose outcome it
	/// had not confirmed become outcome-unknown, to be resolved by review rather than resubmission. Returns whether the
	/// generation changed.
	@discardableResult
	public func recordAuthorityGeneration(_ generation: Int64, serviceID: String) throws(IndexError) -> Bool {
		try transaction { connection throws(IndexError) in
			let previous =
				try connection.queryOne(
					"SELECT authority_generation FROM remote_endpoints WHERE service_id = ?", [serviceID],
					{ $0.optionalInt64(0) }) ?? nil
			try connection.run(
				"UPDATE remote_endpoints SET authority_generation = ? WHERE service_id = ?", [generation, serviceID])
			guard let previous, previous != generation else {
				return false
			}
			let folders = try connection.query(
				"SELECT folder_id FROM remote_bindings WHERE service_id = ?", [serviceID], { $0.text(0) })
			for folderID in folders {
				try connection.run("DELETE FROM remote_cursors WHERE folder_id = ?", [folderID])
				try connection.run("DELETE FROM remote_snapshots WHERE folder_id = ?", [folderID])
			}
			try Self.markUnconfirmedOutcomeUnknown(connection, serviceID: serviceID, generation: generation)
			return true
		}
	}

	// MARK: - Bindings

	static let bindingColumns = "folder_id, access_mode, service_id, remote_folder_id, can_read, can_write"

	static func binding(from row: SQLiteStatement) -> RemoteFolderBinding {
		RemoteFolderBinding(
			folderID: row.text(0), accessMode: RemoteAccessMode(rawValue: row.text(1)) ?? .syncthing,
			serviceID: row.optionalText(2), remoteFolderID: row.optionalText(3),
			permissions: RemotePermissions(read: row.bool(4), write: row.bool(5)))
	}

	/// Binds a local folder to a folder on a gateway, or changes how it is accessed. For `remoteAPI`, the folder's
	/// rows are projected from the gateway's metadata from now on: the next refresh takes a snapshot, which keeps the
	/// identifiers of existing rows at the same paths. The folder (and its root item) is added to the index when it is
	/// not there yet. Switching a folder back to `syncthing` (or to another gateway folder) is refused while its remote
	/// operations are unresolved: not done, of unknown outcome, or rejected edits that were neither exported nor
	/// discarded.
	public func bindRemoteFolder(
		_ binding: RemoteFolderBinding, folder: FolderDescription? = nil, rootGatewayID: String? = nil
	) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			let existing = try connection.queryOne(
				"SELECT \(Self.bindingColumns) FROM remote_bindings WHERE folder_id = ?", [binding.folderID],
				Self.binding)
			if existing?.isRemote == true,
				!binding.isRemote || existing?.serviceID != binding.serviceID
					|| existing?.remoteFolderID != binding.remoteFolderID
			{
				if try connection.queryOne(
					"SELECT EXISTS (SELECT 1 FROM remote_outbox WHERE folder_id = ? AND \(Self.unresolvedCondition))",
					[binding.folderID], { $0.bool(0) }) == true
				{
					throw .notAllowed("changes to this folder were not resolved with its gateway")
				}
			}
			if let folder {
				let writer = try IndexWriter(connection: connection)
				try writer.upsertFolder(folder)
				try writer.finish()
			}
			try connection.run(
				"""
				INSERT INTO remote_bindings (folder_id, access_mode, service_id, remote_folder_id, root_gateway_id,
					can_read, can_write, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(folder_id) DO UPDATE SET access_mode = excluded.access_mode,
					service_id = excluded.service_id, remote_folder_id = excluded.remote_folder_id,
					root_gateway_id = COALESCE(excluded.root_gateway_id, root_gateway_id),
					can_read = excluded.can_read, can_write = excluded.can_write, updated_at = excluded.updated_at
				""",
				[
					binding.folderID, binding.accessMode.rawValue, binding.serviceID, binding.remoteFolderID,
					rootGatewayID, binding.permissions.read, binding.permissions.write, Date().timeIntervalSince1970,
				])
			if existing?.accessMode != binding.accessMode || existing?.serviceID != binding.serviceID
				|| existing?.remoteFolderID != binding.remoteFolderID
			{
				// Another source of metadata: start over with a snapshot
				try connection.run("DELETE FROM remote_cursors WHERE folder_id = ?", [binding.folderID])
				try connection.run("DELETE FROM remote_snapshots WHERE folder_id = ?", [binding.folderID])
				if existing?.isRemote == true {
					try connection.run("DELETE FROM remote_items WHERE folder_id = ?", [binding.folderID])
					// Every operation is resolved: an overlay left over would hide the rows' new source
					try connection.run("DELETE FROM remote_overlays WHERE folder_id = ?", [binding.folderID])
					// What the system may still try to make again is for the new source to judge
					try connection.run("DELETE FROM remote_discards WHERE folder_id = ?", [binding.folderID])
				}
			}
		}
	}

	/// Updates a remote folder's effective permissions (from `GET /v1/folders`)
	public func updateRemotePermissions(_ permissions: RemotePermissions, folderID: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run(
				"UPDATE remote_bindings SET can_read = ?, can_write = ?, updated_at = ? WHERE folder_id = ?",
				[permissions.read, permissions.write, Date().timeIntervalSince1970, folderID])
		}
	}

	public func remoteBinding(folderID: String) throws(IndexError) -> RemoteFolderBinding? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.bindingColumns) FROM remote_bindings WHERE folder_id = ?", [folderID], Self.binding)
		}
	}

	public func remoteBindings() throws(IndexError) -> [RemoteFolderBinding] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.bindingColumns) FROM remote_bindings ORDER BY folder_id", [], Self.binding)
		}
	}

	/// The gateway item ID of a remote folder's root
	public func remoteRootGatewayID(folderID: String) throws(IndexError) -> String? {
		try withConnection { connection throws(IndexError) in try Self.rootGatewayID(connection, folderID: folderID) }
	}

	static func rootGatewayID(_ connection: SQLiteConnection, folderID: String) throws(IndexError) -> String? {
		try connection.queryOne(
			"SELECT root_gateway_id FROM remote_bindings WHERE folder_id = ?", [folderID], { $0.optionalText(0) })
			?? nil
	}

	static func remoteBinding(_ connection: SQLiteConnection, folderID: String) throws(IndexError)
		-> RemoteFolderBinding?
	{
		try connection.queryOne(
			"SELECT \(bindingColumns) FROM remote_bindings WHERE folder_id = ?", [folderID], binding)
	}

	/// The local folder bound to the folder on the gateway
	static func localFolderID(_ connection: SQLiteConnection, serviceID: String, remoteFolderID: String)
		throws(IndexError) -> String?
	{
		try connection.queryOne(
			"""
			SELECT folder_id FROM remote_bindings WHERE service_id = ? AND remote_folder_id = ? AND access_mode = ?
			""", [serviceID, remoteFolderID, RemoteAccessMode.remoteAPI.rawValue], { $0.text(0) })
	}

	// MARK: - Sync state

	public func remoteSyncState(folderID: String) throws(IndexError) -> RemoteSyncState {
		try withConnection { connection throws(IndexError) in try Self.syncState(connection, folderID: folderID) }
	}

	static func syncState(_ connection: SQLiteConnection, folderID: String) throws(IndexError) -> RemoteSyncState {
		let cursor =
			try connection.queryOne(
				"SELECT cursor FROM remote_cursors WHERE folder_id = ?", [folderID], { $0.optionalText(0) }) ?? nil
		let session = try connection.queryOne(
			"""
			SELECT snapshot_id, start_cursor, next_page, catch_up_cursor, pages, started_at, mark
			FROM remote_snapshots WHERE folder_id = ?
			""", [folderID]
		) { row in
			RemoteSyncState.SnapshotSession(
				snapshotID: row.optionalText(0), startCursor: row.optionalText(1), nextPage: row.optionalText(2),
				catchUpCursor: row.optionalText(3), pages: Int(row.int64(4)),
				startedAt: Date(timeIntervalSince1970: row.double(5)), mark: row.int64(6))
		}
		return RemoteSyncState(folderID: folderID, cursor: cursor, snapshot: session)
	}

	/// Starts a snapshot session for the folder, unless one is in progress already (which is kept: another process
	/// may be storing its pages; drop it with `abandonRemoteSnapshot` to start over). Stored metadata, overlays and
	/// pending operations are kept: the snapshot refreshes the metadata, and removes only what it shows is gone once
	/// complete.
	@discardableResult
	public func beginRemoteSnapshot(folderID: String) throws(IndexError) -> RemoteSyncState {
		try transaction { connection throws(IndexError) in
			let state = try Self.syncState(connection, folderID: folderID)
			if state.snapshot != nil {
				return state
			}
			let mark = (Int64(try Self.meta(connection, "remote_mark") ?? "0") ?? 0) + 1
			try Self.setMeta(connection, "remote_mark", String(mark))
			try connection.run(
				"""
				INSERT INTO remote_snapshots (folder_id, snapshot_id, start_cursor, next_page, catch_up_cursor,
					mark, pages, started_at)
				VALUES (?, NULL, NULL, NULL, NULL, ?, 0, ?)
				""", [folderID, mark, Date().timeIntervalSince1970])
			return try Self.syncState(connection, folderID: folderID)
		}
	}

	/// Drops the snapshot in progress (after `410 snapshot_expired`); the next refresh starts a new one
	public func abandonRemoteSnapshot(folderID: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM remote_snapshots WHERE folder_id = ?", [folderID])
		}
	}

	/// Forgets the folder's change cursor (after `410 cursor_expired`), so that the next refresh takes a fresh
	/// snapshot. Remote metadata, item mappings, overlays, outbox operations and staged files are kept.
	public func resetRemoteCursor(folderID: String) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try connection.run("DELETE FROM remote_cursors WHERE folder_id = ?", [folderID])
			try connection.run("DELETE FROM remote_snapshots WHERE folder_id = ?", [folderID])
		}
	}

	/// Stores a snapshot page, together with the session's position, in one transaction. `requestedPage` is the page
	/// token the page was requested with (nil for the first page); a page another process stored already is skipped.
	public func applyRemoteSnapshotPage(_ page: SnapshotPage, folderID: String, requestedPage: String?)
		throws(IndexError) -> RemoteApplyResult
	{
		try transaction { connection throws(IndexError) in
			let state = try Self.syncState(connection, folderID: folderID)
			guard let session = state.snapshot, !session.isCatchingUp, session.nextPage == requestedPage,
				requestedPage == nil ? session.snapshotID == nil : session.snapshotID == page.snapshot
			else {
				return RemoteApplyResult(stored: false)
			}
			let projector = try RemoteProjector(connection: connection)
			for item in page.items {
				try projector.apply(item, folderID: folderID, mark: session.mark)
			}
			try connection.run(
				"""
				UPDATE remote_snapshots SET snapshot_id = ?, start_cursor = COALESCE(start_cursor, ?), next_page = ?,
					catch_up_cursor = CASE WHEN ? IS NULL THEN COALESCE(start_cursor, ?) END, pages = pages + 1
				WHERE folder_id = ?
				""", [page.snapshot, page.cursor, page.next, page.next, page.cursor, folderID])
			try projector.finish()
			return RemoteApplyResult(changed: projector.visibleChanges)
		}
	}

	/// Stores a page of changes after `cursor`, and advances the folder's cursor in the same transaction. While a
	/// snapshot catches up, this continues it; the page without `more` completes it, which removes what the snapshot
	/// did not show. A page whose cursor another process moved already is skipped.
	public func applyRemoteChangesPage(_ page: ChangesPage, folderID: String, after cursor: String)
		throws(IndexError) -> RemoteApplyResult
	{
		try transaction { connection throws(IndexError) in
			let state = try Self.syncState(connection, folderID: folderID)
			let projector = try RemoteProjector(connection: connection)
			if let session = state.snapshot {
				guard session.catchUpCursor == cursor else {
					return RemoteApplyResult(stored: false)
				}
				for item in page.changes {
					try projector.apply(item, folderID: folderID, mark: session.mark)
				}
				if page.more {
					try connection.run(
						"UPDATE remote_snapshots SET catch_up_cursor = ? WHERE folder_id = ?", [page.cursor, folderID])
					try projector.finish()
					return RemoteApplyResult(changed: projector.visibleChanges)
				}
				try projector.finish()
				try projector.removeUnseen(folderID: folderID, mark: session.mark)
				try connection.run("DELETE FROM remote_snapshots WHERE folder_id = ?", [folderID])
				try Self.setCursor(connection, page.cursor, folderID: folderID)
				try projector.finish()
				return RemoteApplyResult(changed: projector.visibleChanges, completedSnapshot: true)
			}
			guard state.cursor == cursor else {
				return RemoteApplyResult(stored: false)
			}
			for item in page.changes {
				try projector.apply(item, folderID: folderID, mark: nil)
			}
			try Self.setCursor(connection, page.cursor, folderID: folderID)
			try projector.finish()
			return RemoteApplyResult(changed: projector.visibleChanges)
		}
	}

	private static func setCursor(_ connection: SQLiteConnection, _ cursor: String, folderID: String)
		throws(IndexError)
	{
		try connection.run(
			"INSERT OR REPLACE INTO remote_cursors (folder_id, cursor, updated_at) VALUES (?, ?, ?)",
			[folderID, cursor, Date().timeIntervalSince1970])
	}

	/// Stores current metadata of items outside the change feed (such as `GET /v1/items/{id}`, or the current item of a
	/// `412`), without moving any cursor. Items of folders not bound to the gateway are ignored.
	///
	/// Such an item may have been captured before a change the feed stored since (the cursor is past it already), so
	/// it is stored only where it cannot overwrite a newer state: when no metadata of it is stored, or when the stored
	/// revision is the one the client knew when it sent the request (`knownRevisions`, by gateway item ID). Other
	/// items are left to the change feed.
	@discardableResult
	public func applyRemoteItems(
		_ items: [RemoteItem], serviceID: String, knownRevisions: [String: RemoteRevision] = [:]
	) throws(IndexError) -> RemoteApplyResult {
		try transaction { connection throws(IndexError) in
			let projector = try RemoteProjector(connection: connection)
			for item in items {
				guard
					let folderID = try Self.localFolderID(
						connection, serviceID: serviceID, remoteFolderID: item.folderID)
				else {
					continue
				}
				try projector.apply(
					item, folderID: folderID, mark: nil, onlyOver: Set([knownRevisions[item.id]].compactMap { $0 }))
			}
			try projector.finish()
			return RemoteApplyResult(changed: projector.visibleChanges)
		}
	}

	// MARK: - Remote metadata

	static let remoteItemColumns = """
		item_id, gateway_id, folder_id, parent_gateway_id, path, name, kind, revision, content_id, size, modified_ns, \
		mode, writable, subtree_generation, availability, deleted
		"""

	static func remoteItemState(from row: SQLiteStatement) -> RemoteItemState {
		RemoteItemState(
			itemID: row.optionalInt64(0), gatewayID: row.text(1), folderID: row.text(2),
			parentGatewayID: row.optionalText(3), path: row.text(4), name: row.text(5),
			kind: RemoteItemKind(rawValue: row.text(6)), revision: RemoteRevision(row.text(7)),
			contentID: row.optionalText(8).map { ContentIdentity($0) }, size: row.int64(9), modifiedNs: row.int64(10),
			mode: Int(row.int64(11)), isWritable: row.bool(12), subtreeGeneration: row.optionalInt64(13),
			availability: row.optionalText(14).map { RemoteAvailability(rawValue: $0) }, isDeleted: row.bool(15))
	}

	/// The confirmed remote metadata of a local item (not its pending local state, see `remoteOverlay(itemID:)`)
	public func remoteItem(itemID: Int64) throws(IndexError) -> RemoteItemState? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.remoteItemColumns) FROM remote_items WHERE item_id = ?", [itemID], Self.remoteItemState)
		}
	}

	public func remoteItem(gatewayID: String) throws(IndexError) -> RemoteItemState? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT \(Self.remoteItemColumns) FROM remote_items WHERE gateway_id = ?", [gatewayID],
				Self.remoteItemState)
		}
	}

	/// Remote items of the folder that are not shown, because their parent is not known yet or their name collides
	/// with another item's
	public func waitingRemoteItems(folderID: String, limit: Int = 100) throws(IndexError) -> [RemoteItemState] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT \(Self.remoteItemColumns) FROM remote_items WHERE folder_id = ? AND placed = 0 AND deleted = 0
				ORDER BY path LIMIT ?
				""", [folderID, limit], Self.remoteItemState)
		}
	}

	// MARK: - Staged files

	/// A staged file whose bytes the gateway committed with the content identity, usable as a cached copy of it
	public func stagedFile(contentID: ContentIdentity) throws(IndexError) -> String? {
		try withConnection { connection throws(IndexError) in
			try connection.queryOne(
				"SELECT path FROM remote_staged_files WHERE content_id = ? LIMIT 1", [contentID.rawValue],
				{ $0.text(0) })
		}
	}

	/// What the device may hold of the contents of items in folders accessed through a gateway, by item ID (items
	/// without remote metadata or an overlay are left out). The paths are relative to `RemoteStorage.baseURL`; check
	/// that the files exist before using them. One query for a whole page of items, for enumerations.
	public func remoteLocalContent(of itemIDs: [Int64]) throws(IndexError) -> [Int64: RemoteLocalContent] {
		guard !itemIDs.isEmpty else {
			return [:]
		}
		return try withConnection { connection throws(IndexError) in
			var result: [Int64: RemoteLocalContent] = [:]
			// Within SQLite's limit on the number of parameters
			for start in stride(from: 0, to: itemIDs.count, by: 500) {
				let ids = Array(itemIDs[start..<min(start + 500, itemIDs.count)])
				let placeholders = Array(repeating: "?", count: ids.count).joined(separator: ", ")
				let rows = try connection.query(
					"""
					SELECT i.id, r.content_id, o.staged_path,
						(SELECT s.path FROM remote_staged_files AS s
							WHERE r.content_id IS NOT NULL AND s.content_id = r.content_id LIMIT 1), o.origin
					FROM items AS i
					LEFT JOIN remote_items AS r ON r.item_id = i.id AND r.deleted = 0
					LEFT JOIN remote_overlays AS o ON o.item_id = i.id
					WHERE i.id IN (\(placeholders)) AND (r.gateway_id IS NOT NULL OR o.item_id IS NOT NULL)
					""", ids
				) { row in
					(
						row.int64(0),
						RemoteLocalContent(
							contentID: row.optionalText(1).map { ContentIdentity($0) },
							overlayStagedPath: row.optionalText(2), committedStagedPath: row.optionalText(3),
							overlayOrigin: row.optionalText(4).flatMap(RemoteChangeOrigin.init(rawValue:)))
					)
				}
				for (id, content) in rows {
					result[id] = content
				}
			}
			return result
		}
	}

	/// Staged files that no unresolved operation or overlay needs anymore (to remove from disk, then forget with
	/// `forgetStagedFile`). Files whose bytes became a committed version are kept while their item shows that version.
	public func releasableStagedFiles(limit: Int = 100) throws(IndexError) -> [String] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"""
				SELECT s.path FROM remote_staged_files AS s
				WHERE NOT EXISTS (
						SELECT 1 FROM remote_outbox AS o WHERE o.staged_path = s.path
							AND o.state NOT IN ('committed', 'conflict', 'canceled'))
					AND NOT EXISTS (SELECT 1 FROM remote_overlays AS v WHERE v.staged_path = s.path)
					AND NOT EXISTS (
						SELECT 1 FROM remote_items AS r WHERE s.content_id IS NOT NULL AND r.content_id = s.content_id
							AND r.deleted = 0)
				LIMIT ?
				""", [limit], { $0.text(0) })
		}
	}

	public func forgetStagedFile(path: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM remote_staged_files WHERE path = ?", [path])
		}
	}

	static func registerStagedFile(
		_ connection: SQLiteConnection, _ file: StagedFile, itemID: Int64?, operationID: String
	) throws(IndexError) {
		try connection.run(
			"""
			INSERT OR REPLACE INTO remote_staged_files (path, item_id, operation_id, size, digest, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			""", [file.relativePath, itemID, operationID, file.size, file.digest, Date().timeIntervalSince1970])
	}
}
