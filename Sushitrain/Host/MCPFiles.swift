// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	extension MCPIntegration {
		func files(_ method: String, parameters: [String: Any], cancellation: MCPCancellation? = nil) async throws -> Data {
			guard let environment = RemoteEnvironment.shared else { throw MCPAppError("The Files environment is unavailable.") }
			if [
				"status", "mcp.files.transfers", "mcp.files.gateways", "mcp.files.folders", "mcp.files.children",
				"mcp.files.search", "mcp.files.pending.list", "mcp.files.storage"
			].contains(method) {
				return try await filesQuery(method, parameters: parameters, environment: environment)
			}
			if [
				"mcp.files.pair", "mcp.files.forget", "mcp.files.available", "mcp.files.add", "mcp.files.remove",
				"mcp.files.pending.discard", "mcp.files.pending.export", "mcp.files.storage.budget"
			].contains(method) {
				return try await filesManagement(method, parameters: parameters, environment: environment)
			}
			if ["mcp.files.mkdir", "mcp.files.import"].contains(method) {
				return try await filesCreation(method, parameters: parameters, environment: environment)
			}
			let itemID = (parameters["Item"] as? NSNumber)?.int64Value ?? 0
			return try await file(
				method, parameters: parameters, itemID: itemID, environment: environment, cancellation: cancellation)
		}
		private func filesQuery(_ method: String, parameters: [String: Any], environment: RemoteEnvironment) async throws
			-> Data {
			let store = environment.store
			let coordinator = RemoteCoordinator.shared
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }
			switch method {
			case "mcp.files.transfers": return try fileTransfers(store: store)
			case "status":
				return try Self.json([
					"mode": "files", "version": Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "",
					"folders": try store.folders().count, "gateways": try store.remoteEndpoints().count,
					"pendingChanges": coordinator.operations.count
				])
			case "mcp.files.gateways":
				return try Self.json(
					try store.remoteEndpoints().map { endpoint in
						[
							"serviceID": endpoint.serviceID, "name": endpoint.displayName, "host": endpoint.host,
							"port": endpoint.port, "state": coordinator.status(serviceID: endpoint.serviceID)?.summary ?? "Not checked"
						] as [String: Any]
					})
			case "mcp.files.folders":
				return try Self.json(
					try store.folders().map { ["id": $0.folderID, "label": $0.label, "rootItem": $0.rootItemID] as [String: Any] })
			case "mcp.files.children":
				return try Self.json(
					try store.children(of: number("Parent") == 0 ? nil : number("Parent"), limit: 100_000).map(Self.item))
			case "mcp.files.search":
				return try await Self.search(store: store, parentID: number("Parent"), query: text("Name"))
			case "mcp.files.pending.list":
				await coordinator.reloadState()
				return try Self.json(
					coordinator.operations.map {
						[
							"id": $0.id, "folder": $0.folderID, "name": $0.name,
							"kind": $0.kind.rawValue, "state": $0.state.rawValue, "error": $0.lastError ?? ""
						] as [String: Any]
					})
			case "mcp.files.storage":
				return try Self.json([
					"budget": CacheSettings.limit,
					"ownership": "macOS manages materialized provider content; the app cache budget excludes unsent edits."
				])
			default: throw MCPAppError("Unsupported Files query.")
			}
		}

		private func file(
			_ method: String, parameters: [String: Any], itemID: Int64, environment: RemoteEnvironment,
			cancellation: MCPCancellation?
		) async throws -> Data {
			let store = environment.store
			guard let item = try store.item(id: itemID), !item.isDeleted else { throw MCPAppError("The file no longer exists.") }
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }
			if method == "mcp.files.inspect" { return try Self.json(Self.item(item)) }
			if method == "mcp.files.keep" || method == "mcp.files.wait" {
				return try await keepFile(method, parameters: parameters, itemID: itemID, environment: environment)
			}
			if method.hasPrefix("mcp.files.version") {
				return try await versionFile(method, parameters: parameters, itemID: itemID, environment: environment)
			}
			if method == "mcp.files.read" || method == "mcp.files.download" {
				return try await readFile(
					method, parameters: parameters, item: item, environment: environment, cancellation: cancellation)
			}
			return try await mutateFile(method, parameters: parameters, item: item, environment: environment)
		}

		func mutation(_ result: RemoteMutationResult, environment: RemoteEnvironment) async throws -> Data {
			environment.removeStagedFiles(result.releasedStagedFiles)
			FilesDomain.shared.signalWorkingSet()
			RemoteCoordinator.shared.pokeOutbox()
			await RemoteCoordinator.shared.reloadState()
			return try Self.json([
				"item": result.item.map(Self.item) ?? [:], "nativeOperationId": result.mutation?.operationID ?? "",
				"state": "queued"
			])
		}
		nonisolated static func item(_ item: IndexItem) -> [String: Any] {
			[
				"Item": item.id, "Node": item.metadataRevision, "folder": item.folderID, "path": item.path, "name": item.name,
				"size": item.size, "directory": item.kind.isContainer, "pending": item.pending.rawValue,
				"contentRevision": item.contentRevision, "parent": item.parentID ?? 0
			]
		}
		nonisolated static func stage(_ source: URL, storage: RemoteStorage) async throws -> StagedFile {
			try Task.checkCancellation()
			let staged = try await Task.detached { try RemoteUploader.stage(source, in: storage) }.value
			if Task.isCancelled {
				try? FileManager.default.removeItem(at: storage.url(relativePath: staged.relativePath))
				throw CancellationError()
			}
			return staged
		}
		nonisolated static func export(_ source: URL, to destination: String) async throws {
			guard destination.hasPrefix("/") else { throw MCPAppError("Choose an absolute export destination.") }
			// The shared import copier uses exclusive creation and preserves metadata.
			try Task.checkCancellation()
			try await Task.detached { try FileImportCopy.copy(source, to: URL(fileURLWithPath: destination)) }.value
		}
		private func keepFile(_ method: String, parameters: [String: Any], itemID: Int64, environment: RemoteEnvironment)
			async throws -> Data {
			let store = environment.store
			guard let item = try store.item(id: itemID) else { throw MCPAppError("The file no longer exists.") }

			guard let manager = NSFileProviderManager(for: FilesDomain.domain) else {
				throw MCPAppError("The Finder location is unavailable.")
			}
			let identifier = NSFileProviderItemIdentifier(rawValue: ItemIdentifiers.string(for: itemID))
			if method == "mcp.files.wait" || parameters["Keep"] as? Bool == true {
				try await manager.requestDownloadForItem(withIdentifier: identifier)
			} else {
				guard item.pending == .none, try store.remoteOverlay(itemID: itemID) == nil else {
					throw MCPAppError("The file has unsent local edits.")
				}
				try await manager.evictItem(identifier: identifier)
			}
			return try Self.json(["item": itemID, "requested": true, "completionOwner": "macOS File Provider"])

		}
		private func fileTransfers(store: IndexStore) throws -> Data {
			return try Self.json(
				try store.transfers(limit: 10000).map { transfer in
					[
						"id": transfer.transferID, "kind": transfer.kind.rawValue,
						"item": transfer.itemID.map { $0 as Any } ?? NSNull(), "state": transfer.state.rawValue,
						"totalBytes": transfer.totalSize, "verifiedBlocks": transfer.verifiedBlockCount,
						"totalBlocks": transfer.blockCount, "error": transfer.lastError.map { $0 as Any } ?? NSNull()
					] as [String: Any]
				})

		}
		private func mutateFile(_ method: String, parameters: [String: Any], item: IndexItem, environment: RemoteEnvironment)
			async throws -> Data {
			let store = environment.store
			let itemID = item.id
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }
			guard number("Node") == item.metadataRevision else {
				throw MCPAppError("The file revision changed; browse again and supply its node.")
			}
			switch method {
			case "mcp.files.replace":
				guard let remote = try store.remoteItem(itemID: itemID) else {
					throw MCPAppError("The remote revision is unavailable.")
				}
				let staged = try await Self.stage(URL(fileURLWithPath: text("Path")), storage: environment.storage)
				var recorded = false
				defer {
					if !recorded {
						try? FileManager.default.removeItem(at: environment.storage.url(relativePath: staged.relativePath))
					}
				}
				try Task.checkCancellation()
				let result = try store.recordRemoteContentChange(
					itemID: itemID,
					modifiedNanoseconds: Int64(Date().timeIntervalSince1970 * 1_000_000_000), stagedFile: staged,
					expectedRevision: remote.revision)
				recorded = true
				return try await mutation(result, environment: environment)
			case "mcp.files.delete":
				return try await mutation(store.recordRemoteDeletion(itemID: itemID), environment: environment)
			case "mcp.files.rename":
				guard let parent = item.parentID else { throw MCPAppError("Folder roots cannot be renamed with a file operation.") }
				return try await mutation(
					store.recordRemoteMove(itemID: itemID, toParentItemID: parent, name: text("NewName")), environment: environment)
			case "mcp.files.move": return try await moveFile(parameters: parameters, item: item, environment: environment)
			default: throw MCPAppError("unsupported Files operation")
			}
		}
		private func moveFile(
 parameters: [String: Any], item: IndexItem, environment: RemoteEnvironment
) async throws -> Data {
			let store = environment.store
			let itemID = item.id
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			let destination = text("Path")
			guard !destination.hasPrefix("/"), !destination.contains("\\"), !destination.split(separator: "/").contains("..")
			else { throw MCPAppError("Use a contained relative destination.") }
			let parentPath = (destination as NSString).deletingLastPathComponent
			guard let parent = try store.item(folderID: item.folderID, path: parentPath) else {
				throw MCPAppError("Destination parent does not exist.")
			}
			return try await mutation(
				store.recordRemoteMove(
					itemID: itemID, toParentItemID: parent.id, name: (destination as NSString).lastPathComponent),
				environment: environment)

		}
	}
#endif
