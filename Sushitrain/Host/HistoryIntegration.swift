// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import AppKit
	import Foundation
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit
	import SynctrainHostKit

	/// Keeps gateway credentials and engine coordination inside their owning app.
	@MainActor final class HistoryIntegration {
		static let shared = HistoryIntegration()
		private weak var state: AppState?
		private var server: HistoryBridgeServer?
		private struct Handle {
			var fileURL: URL
			var item: FinderItem
			var configuration: String
			var sourceID: String
			var version: HistoryBridgeVersion
			var remote: RemoteHistory?
			var remoteVersion: RemoteVersion?
			var created = Date()
		}
		private var handles: [String: Handle] = [:]
		private var leases: [String: URL] = [:]
		private let directory = HistoryBridgeLocation.directory.appending(path: "Downloads")

		func start(_ state: AppState) {
			guard EngineLocations.testRoot == nil, server == nil else { return }
			self.state = state
			do {
				try FileManager.default.createDirectory(
					at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
				removeLeftovers()
				server = try HistoryBridgeServer { [weak self] request, send in
					guard let self else { throw HistoryBridgeError.message("The history service stopped.") }
					try await self.handle(request, send: send)
				}
			}
			catch { Log.warn("Could not start BetterFinder history integration: \(error.localizedDescription)") }
		}
		private func registry() throws -> FinderRegistry {
			guard let state else { throw HistoryError.sourceChanged }
			let data = try state.client.finderRootsJSON()
			let roots = try JSONDecoder().decode([FinderRoot].self, from: data)
			return FinderRegistry(
				configuration: FinderIntegration.configurationID(
					state.configDirectory, deviceID: state.client.deviceID(), roots: roots), running: true, roots: roots)
		}
		private func handle(_ request: HistoryBridgeRequest, send: @escaping @Sendable (HistoryBridgeResponse) throws -> Void)
			async throws
		{
			removeLeftovers()
			leases = leases.filter { FileManager.default.fileExists(atPath: $0.value.path) }
			handles = handles.filter { Date().timeIntervalSince($0.value.created) < 3600 }
			if request.action == "release" {
				if let id = request.leaseID, let url = leases.removeValue(forKey: id) {
					try? FileManager.default.removeItem(at: url)
				}
				return
			}
			if request.action == "list" {
				guard let path = request.filePath else { throw HistoryError.sourceChanged }
				try await list(URL(fileURLWithPath: path), send: send)
				return
			}
			guard let id = request.handle, let selected = handles[id], selected.version.metadataStatus == "complete",
				let state, try registry().configuration == selected.configuration,
				let folder = state.client.folder(withID: selected.item.folderID)
			else { throw HistoryError.sourceChanged }
			try selected.remote?.checkSource()
			if request.action == "validateExport" {
				guard let path = request.filePath else { throw HistoryError.sourceChanged }
				let exclusive = try await goBlocking(returning: {
					try folder.validateVersionExportDestination(path)
					return folder.versionExportRequiresExclusive(path)
				})
				var response = HistoryBridgeResponse(event: "exportPolicy")
				response.exclusiveExport = exclusive
				try send(response)
				return
			}
			guard request.action == "materialize" || request.action == "restore" else { throw HistoryError.sourceChanged }
			let token: String?
			if request.action == "restore" {
				token = try await goBlocking(returning: {
					var error: NSError?
					let token = folder.captureRestoreTarget(selected.item.path, error: &error)
					if let error { throw error }
					return token
				})
			}
			else {
				token = nil
			}
			let lease = UUID().uuidString
			let parent = directory.appending(path: lease)
			try FileManager.default.createDirectory(
				at: parent, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
			var keep = false
			defer { if !keep { try? FileManager.default.removeItem(at: parent) } }
			let archive = parent.appending(path: "snapshot.tar")
			if let remote = selected.remote, let version = selected.remoteVersion {
				try remote.checkSource()
				try await remote.client.downloadVersionSnapshot(
					folderID: selected.item.folderID, path: selected.item.path, version: version, to: archive)
				try remote.checkSource()
			}
			else {
				try await goBlocking {
					try folder.exportVersionSnapshot(
						selected.item.path, id_: selected.version.id, expected: selected.version.snapshotETag ?? "",
						destination: archive.path)
				}
			}
			try Task.checkCancellation()
			let files = parent.appending(path: "file")
			try FileManager.default.createDirectory(
				at: files, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
			let output = files.appending(path: selected.fileURL.lastPathComponent)
			try await goBlocking {
				var error: NSError?
				guard
					SushitrainMaterializeVersionSnapshot(
						archive.path, output.path, selected.item.folderID, selected.item.path, selected.version.id, &error)
				else { throw error ?? HistoryError.invalidDownload as NSError }
			}
			try Task.checkCancellation()
			guard try registry().configuration == selected.configuration else { throw HistoryError.sourceChanged }
			try selected.remote?.checkSource()
			if let token {
				try await goBlocking {
					try folder.restoreSnapshotVersion(
						selected.item.path, source: output.path, expected: token, modifiedNs: selected.version.modifiedNs)
				}
				try send(HistoryBridgeResponse(event: "restored"))
			}
			else {
				try FileManager.default.removeItem(at: archive)
				leases[lease] = parent
				var response = HistoryBridgeResponse(event: "materialized")
				response.leaseID = lease
				response.filePath = output.path
				try send(response)
				keep = true
			}
		}
		private func list(_ fileURL: URL, send: @escaping @Sendable (HistoryBridgeResponse) throws -> Void) async throws {
			let registry = try registry()
			guard let rootItem = registry.item(at: fileURL), !rootItem.path.isEmpty, let state,
				let folder = state.client.folder(withID: rootItem.folderID)
			else { return }
			let info = try fileURL.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey])
			guard info.isRegularFile == true, info.isSymbolicLink != true else { return }
			let item = FinderItem(folderID: rootItem.folderID, path: rootItem.path.precomposedStringWithCanonicalMapping)
			var context = HistoryBridgeResponse(event: "context")
			context.folderID = item.folderID
			context.relativePath = item.path
			context.canRestore = folder.canRestoreDownloadedVersion()
			context.versioningEnabled = !folder.versioningType().isEmpty
			context.receiveOnly = folder.folderType() == SushitrainFolderTypeReceiveOnly
			try send(context)
			do {
				let text = try await goBlocking(returning: {
					var error: NSError?
					let text = folder.canBrowseVersions() ? folder.versionSnapshots(item.path, error: &error) : "[]"
					if let error { throw error }
					return text
				})
				let versions = try JSONDecoder().decode([HistoryBridgeVersion].self, from: Data(text.utf8))
				try publish(
					versions, sourceID: "local", name: "This Mac", deviceID: folder.clientDeviceID(), fileURL: fileURL, item: item,
					configuration: registry.configuration, remote: nil, remoteVersions: [], send: send)
			}
			catch {
				var response = HistoryBridgeResponse(event: "source")
				response.sourceID = "local"
				response.sourceName = "This Mac"
				response.message = error.localizedDescription
				try send(response)
			}
			guard let environment = RemoteEnvironment.shared else { return }
			let histories = try environment.store.historyEndpoints().map { endpoint in
				RemoteHistory(
					store: environment.store, source: HistorySource(endpoint: endpoint, folderID: item.folderID, path: item.path),
					client: environment.client(for: endpoint))
			}
			await withTaskGroup(of: Void.self) { group in
				for history in histories {
					group.addTask {
						await self.load(history, fileURL: fileURL, item: item, configuration: registry.configuration, send: send)
					}
				}
			}
			try Task.checkCancellation()
		}
		private func load(
			_ history: RemoteHistory, fileURL: URL, item: FinderItem, configuration: String,
			send: @escaping @Sendable (HistoryBridgeResponse) throws -> Void
		) async {
			do {
				let listing = try await history.versions()
				let versions = listing.versions.map { version in
					HistoryBridgeVersion(
						id: version.id, versionTime: ISO8601DateFormatter().string(from: version.versionTime),
						modifiedNs: version.modifiedNs, size: version.size, metadataStatus: version.metadataStatus,
						snapshotETag: version.snapshotETag)
				}
				try publish(
					versions, sourceID: history.source.id, name: history.source.endpoint.displayName, deviceID: listing.deviceID,
					fileURL: fileURL, item: item, configuration: configuration, remote: history, remoteVersions: listing.versions,
					send: send)
			}
			catch {
				var response = HistoryBridgeResponse(event: "source")
				response.sourceID = history.source.id
				response.sourceName = history.source.endpoint.displayName
				response.message = HistoryError.message(for: error)
				try? send(response)
			}
		}
		private func publish(
			_ versions: [HistoryBridgeVersion], sourceID: String, name: String, deviceID: String?, fileURL: URL,
			item: FinderItem, configuration: String, remote: RemoteHistory?, remoteVersions: [RemoteVersion],
			send: @Sendable (HistoryBridgeResponse) throws -> Void
		) throws {
			guard try registry().configuration == configuration else { throw HistoryError.sourceChanged }
			try remote?.checkSource()
			var result: [HistoryBridgeVersion] = []
			for var version in versions {
				if version.metadataStatus == "complete", version.snapshotETag != nil {
					let id = UUID().uuidString
					handles[id] = Handle(
						fileURL: fileURL, item: item, configuration: configuration, sourceID: sourceID, version: version, remote: remote,
						remoteVersion: remoteVersions.first { $0.id == version.id })
					version.handle = id
				}
				result.append(version)
			}
			var response = HistoryBridgeResponse(event: "source")
			response.sourceID = sourceID
			response.sourceName = name
			response.deviceID = deviceID
			response.versions = result
			try send(response)
		}
		private func removeLeftovers() {
			for entry
				in (try? FileManager.default.contentsOfDirectory(
					at: directory, includingPropertiesForKeys: [.contentModificationDateKey])) ?? []
			{
				if let date = try? entry.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate,
					date < .now.addingTimeInterval(-3600)
				{
					try? FileManager.default.removeItem(at: entry)
				}
			}
		}
	}
#endif
