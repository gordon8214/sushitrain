// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore
	extension MCPIntegration {
		func filesManagement(
			_ method: String, parameters: [String: Any], environment: RemoteEnvironment
		) async throws -> Data {
			let store = environment.store
			let coordinator = RemoteCoordinator.shared
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }
			let serviceID = text("ServiceID")
			switch method {
			case "mcp.files.pair": return try Self.encoded(try await RemotePairing.pair(link: text("Link")))
			case "mcp.files.forget": try await coordinator.forgetGateway(serviceID: serviceID)
			case "mcp.files.available", "mcp.files.add":
				return try await filesAvailable(method, parameters: parameters, environment: environment)
			case "mcp.files.remove":
				let id = (parameters["Item"] as? String) ?? String(number("Item"))
				try await coordinator.removeFolder(folderID: id)
			case "mcp.files.pending.discard": try await coordinator.discard(operationID: text("ID"))
			case "mcp.files.pending.export":
				guard let summary = coordinator.operations.first(where: { $0.id == text("ID") }) else {
					throw MCPAppError("Pending change no longer exists.")
				}
				let temporary = try await coordinator.exportCopy(of: summary)
				try await Self.export(temporary, to: text("Dest"))
			case "mcp.files.storage.budget":
				guard number("Bytes") > 0 else { throw MCPAppError("Enter a positive cache budget.") }
				UserDefaults.standard.set(number("Bytes"), forKey: CacheSettings.storageKey)
			default: throw MCPAppError("Unsupported Files operation.")
			}
			return Data("null".utf8)
		}
		func filesCreation(_ method: String, parameters: [String: Any], environment: RemoteEnvironment) async throws -> Data {
			let store = environment.store
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			func number(_ key: String) -> Int64 { (parameters[key] as? NSNumber)?.int64Value ?? 0 }
			switch method {
			case "mcp.files.mkdir", "mcp.files.import":
				guard let parent = try store.item(id: number("Parent")), parent.kind.isContainer else {
					throw MCPAppError("Choose a folder parent item.")
				}
				let source = URL(fileURLWithPath: text("Path"))
				let name =
					method == "mcp.files.mkdir" ? text("Name") : (text("Name").isEmpty ? source.lastPathComponent : text("Name"))
				guard FileNames.isValid(name) else { throw MCPAppError("Enter a valid file name.") }
				let staged: StagedFile?
				if method == "mcp.files.import" {
					staged = try await Self.stage(source, storage: environment.storage)
				} else {
					staged = nil
				}
				var recorded = false
				defer {
					if !recorded, let staged {
						try? FileManager.default.removeItem(at: environment.storage.url(relativePath: staged.relativePath))
					}
				}
				try Task.checkCancellation()
				let result = try store.recordRemoteCreation(
					folderID: parent.folderID, parentItemID: parent.id, name: name,
					kind: staged == nil ? .directory : .file, modifiedNanoseconds: Int64(Date().timeIntervalSince1970 * 1_000_000_000),
					stagedFile: staged, origin: .provider)
				recorded = true
				return try await mutation(result, environment: environment)
			default: throw MCPAppError("Unsupported creation operation.")
			}
		}
		private func filesAvailable(_ method: String, parameters: [String: Any], environment: RemoteEnvironment) async throws
			-> Data {
			func text(_ key: String) -> String { parameters[key] as? String ?? "" }
			let serviceID = text("ServiceID")
			if method == "mcp.files.available" {
				guard let client = environment.client(serviceID: serviceID) else { throw MCPAppError("Gateway is not paired.") }
				return try Self.encoded(try await client.folders())
			}

			guard let client = environment.client(serviceID: serviceID),
				let folder = try await client.folders().first(where: { $0.id == text("FolderID") })
			else {
				throw MCPAppError("The gateway no longer permits this folder.")
			}
			return try Self.json(["folder": try await RemoteCoordinator.shared.addFolder(folder, serviceID: serviceID)])

		}
	}
#endif
