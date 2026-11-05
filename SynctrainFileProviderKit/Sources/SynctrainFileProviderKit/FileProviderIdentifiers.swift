// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

extension IndexStore {
	static func createFileProviderIdentifierSchema(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute(
			"""
			CREATE TABLE IF NOT EXISTS file_provider_legacy_identifiers (
				identifier TEXT PRIMARY KEY,
				item_id INTEGER UNIQUE,
				gateway_id TEXT,
				folder_id TEXT NOT NULL,
				path TEXT NOT NULL,
				kind INTEGER NOT NULL
			);
			CREATE INDEX IF NOT EXISTS file_provider_legacy_gateway ON file_provider_legacy_identifiers(gateway_id)
				WHERE item_id IS NULL;
			CREATE INDEX IF NOT EXISTS file_provider_legacy_path ON file_provider_legacy_identifiers(folder_id, path, kind)
				WHERE item_id IS NULL;
			CREATE TRIGGER IF NOT EXISTS file_provider_legacy_remote AFTER UPDATE OF item_id ON remote_items
			WHEN NEW.item_id IS NOT NULL BEGIN
				UPDATE file_provider_legacy_identifiers SET item_id = NEW.item_id
				WHERE item_id IS NULL AND gateway_id = NEW.gateway_id
					AND NOT EXISTS (SELECT 1 FROM file_provider_legacy_identifiers WHERE item_id = NEW.item_id);
			END;
			CREATE TRIGGER IF NOT EXISTS file_provider_legacy_remote_insert AFTER INSERT ON remote_items
			WHEN NEW.item_id IS NOT NULL BEGIN
				UPDATE file_provider_legacy_identifiers SET item_id = NEW.item_id
				WHERE item_id IS NULL AND gateway_id = NEW.gateway_id
					AND NOT EXISTS (SELECT 1 FROM file_provider_legacy_identifiers WHERE item_id = NEW.item_id);
			END;
			CREATE TRIGGER IF NOT EXISTS file_provider_legacy_local AFTER INSERT ON items BEGIN
				UPDATE file_provider_legacy_identifiers SET item_id = NEW.id
				WHERE item_id IS NULL AND (gateway_id IS NULL OR kind = \(ItemKind.folderRoot.rawValue))
					AND folder_id = NEW.folder_id AND path = NEW.path AND kind = NEW.kind;
			END;
			""")
	}

	/// The gateway-only client changed databases, but kept the system's domain. Its numeric identifiers must keep
	/// naming the old items, even when the new database allocated those numbers to completely different files.
	/// Import identity only, without changing the old index or the new index's content, edits, or row IDs.
	static func importLegacyFileProviderIdentifiers(_ connection: SQLiteConnection, container: SharedContainer)
		throws(IndexError)
	{
		guard try meta(connection, "legacy_file_provider_identifiers") == nil else { return }
		let legacy = container.fileProviderURL.appending(path: "index.sqlite")
		guard FileManager.default.fileExists(atPath: legacy.path(percentEncoded: false)) else { return }
		try connection.run("ATTACH DATABASE ? AS legacy_file_provider", [legacy.absoluteString + "?mode=ro"])
		defer { try? connection.execute("DETACH DATABASE legacy_file_provider") }
		try connection.transaction { () throws(IndexError) in
			guard try meta(connection, "legacy_file_provider_identifiers") == nil else { return }
			let hasRemoteItems =
				try connection.queryOne(
					"SELECT 1 FROM legacy_file_provider.sqlite_master WHERE type = 'table' AND name = 'remote_items'",
					[], { $0.bool(0) }) ?? false
			let gatewayID = hasRemoteItems ? "r.gateway_id" : "NULL"
			let remoteJoin =
				hasRemoteItems
				? "LEFT JOIN legacy_file_provider.remote_items AS r ON r.item_id = i.id" : ""
			try connection.execute(
				"""
				INSERT INTO file_provider_legacy_identifiers (identifier, gateway_id, folder_id, path, kind)
				SELECT CAST(i.id AS TEXT), \(gatewayID), i.folder_id, i.path, i.kind
				FROM legacy_file_provider.items AS i
				\(remoteJoin)
				WHERE i.deleted = 0;
				UPDATE file_provider_legacy_identifiers SET item_id = (
					SELECT item_id FROM remote_items WHERE gateway_id = file_provider_legacy_identifiers.gateway_id)
				WHERE gateway_id IS NOT NULL;
				UPDATE file_provider_legacy_identifiers SET item_id = (
					SELECT id FROM items WHERE deleted = 0
						AND folder_id = file_provider_legacy_identifiers.folder_id
						AND path = file_provider_legacy_identifiers.path AND kind = file_provider_legacy_identifiers.kind)
				WHERE item_id IS NULL AND (gateway_id IS NULL OR kind = \(ItemKind.folderRoot.rawValue));
				""")
			try setMeta(connection, "legacy_file_provider_identifiers", "1")
			// Make the system refresh metadata, retaining the identifiers of its pending changes.
			try setMeta(connection, "generation", UUID().uuidString)
		}
	}

	/// Resolves a persistent system identifier to the current database row, never treating a legacy number as a new
	/// row ID on migrated devices. New items there use a separate namespace, so an old number cannot be reused.
	public func fileProviderItemID(for identifier: String) throws(IndexError) -> Int64? {
		try withConnection { connection throws(IndexError) in
			if let mapped = try connection.queryOne(
				"SELECT item_id FROM file_provider_legacy_identifiers WHERE identifier = ?", [identifier],
				{ $0.optionalInt64(0) })
			{
				return mapped
			}
			if try Self.meta(connection, "legacy_file_provider_identifiers") != nil {
				guard identifier.hasPrefix("gateway:") else { return nil }
				return ItemIdentifiers.itemID(from: String(identifier.dropFirst("gateway:".count)))
			}
			return ItemIdentifiers.itemID(from: identifier)
		}
	}
}
