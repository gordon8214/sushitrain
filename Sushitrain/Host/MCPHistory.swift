// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore
	extension MCPIntegration {
		func history(_ method: String, parameters: [String: Any]) async throws -> Data {
			guard let environment = RemoteEnvironment.shared else { throw MCPAppError("History sources are unavailable.") }
			let store = environment.store
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			if ["history.sources", "history.pair", "history.enable", "history.forget"].contains(method) {
				return try await historyManagement(method, parameters: parameters, environment: environment)
			}
			let endpoints = try store.historyEndpoints()
			var listings: [[String: Any]] = []
			for endpoint in endpoints where text("ID").isEmpty || text("ID") == endpoint.serviceID {
				try Task.checkCancellation()
				let source = HistorySource(endpoint: endpoint, folderID: text("Folder"), path: text("Path"))
				let history = RemoteHistory(store: store, source: source, client: environment.client(for: endpoint))
				do {
					let listing = try await history.versions()
					if method == "history.list" {
						let versions = try JSONSerialization.jsonObject(with: Self.encoded(listing.versions))
						listings.append([
							"sourceID": endpoint.serviceID, "sourceName": endpoint.displayName, "deviceID": listing.deviceID ?? "",
							"versions": versions
						])
						continue
					}
					guard let version = listing.versions.first(where: { $0.id == text("Version") }),
						version.entityTag == text("ExpectedTag")
					else { throw MCPAppError("The selected archive changed; refresh its version and token.") }
					try await exportHistory(version, history: history, parameters: parameters)
					return Data("null".utf8)
				} catch {
					if method != "history.list" { throw error }
					listings.append(["sourceID": endpoint.serviceID, "error": error.localizedDescription])
				}
			}
			if method == "history.list" { return try Self.json(listings) }
			throw MCPAppError("History source not found or disabled.")
		}
		private func historyManagement(_ method: String, parameters: [String: Any], environment: RemoteEnvironment)
			async throws -> Data {
			let store = environment.store
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			if method == "history.sources" {
				return try Self.json(
					try store.remoteEndpoints().map { endpoint in
						[
							"id": endpoint.serviceID, "name": endpoint.displayName, "host": endpoint.host,
							"enabled": try store.gatewayUsage(serviceID: endpoint.serviceID).contains(.history)
						] as [String: Any]
					})
			}
			if method == "history.pair" {
				return try Self.encoded(try await RemotePairing.pair(link: text("Link"), purpose: .history))
			}
			if method == "history.enable" {
				var usage = try store.gatewayUsage(serviceID: text("ID"))
				if parameters["Enabled"] as? Bool == true {
					usage.insert(.history)
				} else {
					usage.remove(.history)
				}
				try store.setGatewayUsage(usage, serviceID: text("ID"))
				return Data("null".utf8)
			}
			if method == "history.forget" {
				let serviceID = text("ID")
				var usage = try store.gatewayUsage(serviceID: serviceID)
				usage.remove(.history)
				if usage.isEmpty {
					try await RemoteCoordinator.shared.forgetGateway(serviceID: serviceID)
				} else {
					try store.setGatewayUsage(usage, serviceID: serviceID)
					await RemoteCoordinator.shared.reloadState()
				}
				return Data("null".utf8)
			}
			throw MCPAppError("Unsupported history operation.")
		}
		private func exportHistory(_ version: RemoteVersion, history: RemoteHistory, parameters: [String: Any])
			async throws {
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			let directory = AppLocations.current.temporary.appending(path: "MCP-History-\(UUID().uuidString)")
			try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
			defer { try? FileManager.default.removeItem(at: directory) }
			let output = directory.appending(path: (text("Path") as NSString).lastPathComponent)
			if version.metadataStatus == "complete" {
				let archive = directory.appending(path: "snapshot.tar")
				try await history.client.downloadVersionSnapshot(
					folderID: text("Folder"), path: text("Path"), version: version, to: archive)
				let folder = text("Folder")
				let path = text("Path")
				let versionID = version.id
				try await goBlocking {
					var error: NSError?
					guard SushitrainMaterializeVersionSnapshot(archive.path, output.path, folder, path, versionID, &error) else {
						throw error ?? MCPAppError("Invalid version snapshot.") as NSError
					}
				}
			} else {
				try await history.download(version, to: output)
			}
			try history.checkSource()
			try await Self.export(output, to: text("Destination"))

		}
	}
#endif
