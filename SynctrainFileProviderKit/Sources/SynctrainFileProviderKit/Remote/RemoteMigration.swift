// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Where an explicit switch of a folder between Syncthing on this device and a gateway stands. Every step is recorded
/// before the next one starts, so an interrupted switch resumes from its journal before either engine publishes the
/// folder.
public enum RemoteMigrationState: String, Codable, Hashable, Sendable {
	/// Switching to a gateway: new edits are fenced, and Syncthing on this device still owns the folder
	case fenced
	/// Syncthing on this device no longer scans, pulls or announces the folder; the folder is not bound yet
	case engineStopped
	/// The folder is bound to the gateway, and waits for its first complete snapshot
	case bound
	/// Switching back to Syncthing on this device: new edits are fenced
	case returning
}

/// A switch of a folder between Syncthing on this device and a gateway, from the journal in `remote_migrations`
public struct RemoteMigration: Hashable, Sendable {
	public var folderID: String
	public var state: RemoteMigrationState
	public var journal: Journal
	public var updatedAt: Date

	/// What the switch needs to resume
	public struct Journal: Codable, Hashable, Sendable {
		public enum Direction: String, Codable, Hashable, Sendable {
			/// From Syncthing on this device to the gateway
			case toGateway
			/// From the gateway back to Syncthing on this device
			case toSyncthing
		}

		public var direction: Direction
		public var serviceID: String?
		/// The folder ID on the gateway
		public var remoteFolderID: String?
		public var rootGatewayID: String?
		public var permissions: RemotePermissions?
		/// The label the folder keeps in the Files app
		public var label: String?
		/// To the gateway: the sequence of this device's latest change in Syncthing's index of the folder before the
		/// checks that the gateway has every change made on this device
		public var localSequence: Int64?
		public var startedAt: Date
		/// Why the last attempt to continue did not get further, if it did not
		public var lastError: String?

		public init(
			direction: Direction, serviceID: String? = nil, remoteFolderID: String? = nil,
			rootGatewayID: String? = nil, permissions: RemotePermissions? = nil, label: String? = nil,
			localSequence: Int64? = nil, startedAt: Date = Date(), lastError: String? = nil
		) {
			self.direction = direction
			self.serviceID = serviceID
			self.remoteFolderID = remoteFolderID
			self.rootGatewayID = rootGatewayID
			self.permissions = permissions
			self.label = label
			self.localSequence = localSequence
			self.startedAt = startedAt
			self.lastError = lastError
		}
	}
}

/// Changes of a folder made on this device that Syncthing has not picked up yet
public struct LocalChangeSummary: Hashable, Sendable {
	/// Items with changes made through the Files app that Syncthing has not indexed yet
	public var pendingItems: Int
	/// Work the extension queued for Syncthing (scans, selections, renames) that the app has not done yet
	public var queuedWork: Int

	public var isEmpty: Bool { pendingItems == 0 && queuedWork == 0 }
}

extension IndexStore {
	private static let migrationColumns = "folder_id, state, journal, updated_at"

	private static func migration(from row: SQLiteStatement) -> RemoteMigration? {
		guard let state = RemoteMigrationState(rawValue: row.text(1)),
			let journal = row.optionalText(2).flatMap({
				try? RemoteJSON.decoder().decode(RemoteMigration.Journal.self, from: Data($0.utf8))
			})
		else {
			return nil
		}
		return RemoteMigration(
			folderID: row.text(0), state: state, journal: journal, updatedAt: Date(timeIntervalSince1970: row.double(3)))
	}

	private static func encoded(_ journal: RemoteMigration.Journal) throws(IndexError) -> String {
		do {
			return String(decoding: try RemoteJSON.encoder().encode(journal), as: UTF8.self)
		}
		catch {
			throw .notAllowed("the migration journal cannot be encoded")
		}
	}

	/// The folder's switch in progress. A journal that cannot be read is an error, not a missing switch: the folder
	/// stays fenced, and nothing may start another switch over it.
	private static func migration(_ connection: SQLiteConnection, folderID: String) throws(IndexError)
		-> RemoteMigration?
	{
		guard
			let row = try connection.queryOne(
				"SELECT \(migrationColumns) FROM remote_migrations WHERE folder_id = ?", [folderID], { migration(from: $0) }
			)
		else {
			return nil
		}
		guard let migration = row else {
			throw .notAllowed("the journal of the switch of folder \(folderID) cannot be read")
		}
		return migration
	}

	private static func localChanges(_ connection: SQLiteConnection, folderID: String) throws(IndexError)
		-> LocalChangeSummary
	{
		let pending =
			try connection.queryOne(
				"SELECT COUNT(*) FROM items WHERE folder_id = ? AND pending != 0", [folderID], { $0.int64(0) }) ?? 0
		// Requested downloads and evictions of the system's copies change nothing Syncthing publishes
		let kinds = [WorkKind.select, .deselectSafely, .rescan, .finishRename].map { "'\($0.rawValue)'" }
		let work =
			try connection.queryOne(
				"SELECT COUNT(*) FROM work WHERE folder_id = ? AND kind IN (\(kinds.joined(separator: ", ")))",
				[folderID], { $0.int64(0) }) ?? 0
		return LocalChangeSummary(pendingItems: Int(pending), queuedWork: Int(work))
	}

	// MARK: - Reading

	/// The switch of the folder in progress, if any
	public func remoteMigration(folderID: String) throws(IndexError) -> RemoteMigration? {
		try withConnection { connection throws(IndexError) in try Self.migration(connection, folderID: folderID) }
	}

	/// All switches in progress (to resume after an interruption)
	public func remoteMigrations() throws(IndexError) -> [RemoteMigration] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT \(Self.migrationColumns) FROM remote_migrations ORDER BY folder_id", [], Self.migration
			)
			.compactMap { $0 }
		}
	}

	/// Whether new edits of the folder must be refused because it is being switched between Syncthing on this
	/// device and a gateway. Check it right before recording an edit; the switch checks again for edits recorded
	/// while it started.
	public func isRemoteMigrationFenced(folderID: String) throws(IndexError) -> Bool {
		// Fenced by any switch, also one whose journal cannot be read
		try withConnection { connection throws(IndexError) in
			try Self.isRemoteMigrationFenced(connection, folderID: folderID)
		}
	}

	/// The same, inside a transaction that records an edit
	static func isRemoteMigrationFenced(_ connection: SQLiteConnection, folderID: String) throws(IndexError) -> Bool {
		try connection.queryOne(
			"SELECT EXISTS (SELECT 1 FROM remote_migrations WHERE folder_id = ?)", [folderID], { $0.bool(0) }) ?? false
	}

	/// Changes of the folder made on this device that Syncthing has not picked up yet
	public func localChanges(folderID: String) throws(IndexError) -> LocalChangeSummary {
		try withConnection { connection throws(IndexError) in try Self.localChanges(connection, folderID: folderID) }
	}

	// MARK: - Writing

	/// Starts switching the folder (fencing new edits), in one transaction with the check that this device has no
	/// changes of the folder that Syncthing has not picked up (for a switch to a gateway) or that the gateway has not
	/// confirmed (for a switch back). Returns the switch in progress when one in the same direction exists already.
	@discardableResult
	public func beginRemoteMigration(folderID: String, journal: RemoteMigration.Journal) throws(IndexError)
		-> RemoteMigration
	{
		try transaction { connection throws(IndexError) in
			if let existing = try Self.migration(connection, folderID: folderID) {
				guard existing.journal.direction == journal.direction else {
					throw .notAllowed("the folder is being switched the other way")
				}
				return existing
			}
			switch journal.direction {
			case .toGateway:
				let changes = try Self.localChanges(connection, folderID: folderID)
				if !changes.isEmpty {
					throw .notAllowed("changes made on this device have not been picked up by Syncthing yet")
				}
				if try Self.remoteBinding(connection, folderID: folderID)?.isRemote == true {
					throw .notAllowed("the folder is accessed through a gateway already")
				}
			case .toSyncthing:
				// Not done, done without a known outcome, or rejected edits that were neither exported nor discarded
				if try connection.queryOne(
					"SELECT EXISTS (SELECT 1 FROM remote_outbox WHERE folder_id = ? AND \(Self.unresolvedCondition))",
					[folderID], { $0.bool(0) }) == true
				{
					throw .notAllowed("changes to this folder were not resolved with its gateway")
				}
			}
			let state: RemoteMigrationState = journal.direction == .toGateway ? .fenced : .returning
			let now = Date().timeIntervalSince1970
			try connection.run(
				"INSERT INTO remote_migrations (folder_id, state, journal, updated_at) VALUES (?, ?, ?, ?)",
				[folderID, state.rawValue, try Self.encoded(journal), now])
			guard let migration = try Self.migration(connection, folderID: folderID) else {
				throw .notFound
			}
			return migration
		}
	}

	/// Records the step the switch reached, or why it could not continue
	@discardableResult
	public func updateRemoteMigration(folderID: String, state: RemoteMigrationState? = nil, lastError: String?)
		throws(IndexError) -> RemoteMigration
	{
		try transaction { connection throws(IndexError) in
			guard var migration = try Self.migration(connection, folderID: folderID) else {
				throw .notFound
			}
			if let state {
				migration.state = state
			}
			migration.journal.lastError = lastError
			try connection.run(
				"UPDATE remote_migrations SET state = ?, journal = ?, updated_at = ? WHERE folder_id = ?",
				[migration.state.rawValue, try Self.encoded(migration.journal), Date().timeIntervalSince1970, folderID])
			guard let updated = try Self.migration(connection, folderID: folderID) else {
				throw .notFound
			}
			return updated
		}
	}

	/// Ends the switch (once complete, or when it was given up before anything changed), which lifts the fence
	public func finishRemoteMigration(folderID: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			try connection.run("DELETE FROM remote_migrations WHERE folder_id = ?", [folderID])
		}
	}
}
