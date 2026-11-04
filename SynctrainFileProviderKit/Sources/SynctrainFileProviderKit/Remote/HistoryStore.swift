// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// A gateway can be paired for history without creating a File Provider location.
public struct GatewayUsage: OptionSet, Sendable {
	public let rawValue: Int
	public init(rawValue: Int) { self.rawValue = rawValue }
	public static let files = Self(rawValue: 1)
	public static let history = Self(rawValue: 2)
}

extension IndexStore {
	static func createHistorySchema(_ connection: SQLiteConnection) throws(IndexError) {
		try connection.execute(
			"""
			CREATE TABLE IF NOT EXISTS gateway_usage (service_id TEXT PRIMARY KEY, usage INTEGER NOT NULL);
			CREATE TABLE IF NOT EXISTS history_folders (service_id TEXT NOT NULL, folder_id TEXT NOT NULL,
				PRIMARY KEY(service_id, folder_id));
			""")
	}

	/// Existing pairings keep their Files behavior and are not opted into extra history searches.
	public func gatewayUsage(serviceID: String) throws(IndexError) -> GatewayUsage {
		try withConnection { connection throws(IndexError) in
			GatewayUsage(
				rawValue: try connection.queryOne(
					"SELECT usage FROM gateway_usage WHERE service_id = ?", [serviceID], { Int($0.int64(0)) }) ?? 1)
		}
	}

	public func setGatewayUsage(_ usage: GatewayUsage, serviceID: String) throws(IndexError) {
		try withConnection { connection throws(IndexError) in
			guard
				try connection.queryOne("SELECT service_id FROM remote_endpoints WHERE service_id = ?", [serviceID], { $0.text(0) })
					!= nil
			else { throw .notAllowed("gateway is not paired") }
			try connection.run(
				"INSERT INTO gateway_usage VALUES (?, ?) ON CONFLICT(service_id) DO UPDATE SET usage = excluded.usage",
				[serviceID, usage.rawValue])
		}
	}

	public func historyEndpoints() throws(IndexError) -> [RemoteEndpoint] {
		var result: [RemoteEndpoint] = []
		for endpoint in try remoteEndpoints() where try gatewayUsage(serviceID: endpoint.serviceID).contains(.history) {
			result.append(endpoint)
		}
		return result
	}

	public func hasFilesGateways() throws(IndexError) -> Bool {
		for endpoint in try remoteEndpoints() where try gatewayUsage(serviceID: endpoint.serviceID).contains(.files) {
			return true
		}
		return false
	}

	public func saveHistoryFolders(serviceID: String, folderIDs: [String]) throws(IndexError) {
		try transaction { connection throws(IndexError) in
			try connection.run("DELETE FROM history_folders WHERE service_id = ?", [serviceID])
			for id in Set(folderIDs) { try connection.run("INSERT INTO history_folders VALUES (?, ?)", [serviceID, id]) }
		}
	}

	public func historyFolderIDs() throws(IndexError) -> Set<String> {
		try withConnection { connection throws(IndexError) in
			Set(
				try connection.query(
					"SELECT DISTINCT folder_id FROM history_folders JOIN gateway_usage USING(service_id) JOIN remote_endpoints USING(service_id) WHERE (usage & 2) != 0",
					[], { $0.text(0) }))
		}
	}
}
