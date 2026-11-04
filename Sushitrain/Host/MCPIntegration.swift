// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import AppKit
	import FileProvider
	import Observation
	import os
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit
	import SynctrainHostKit

	@Observable @MainActor final class MCPIntegration {
		static let shared = MCPIntegration()
		private(set) var enabled = false
		private(set) var error: String?
		@ObservationIgnored private var server: HostbridgeServer?
		@ObservationIgnored private var delegate: MCPAppDelegate?
		@ObservationIgnored weak var appState: AppState?
		@ObservationIgnored var preferenceEditors: [String: MCPPreferenceEditor] = [:]

		static var directory: URL {
			if let root = EngineLocations.testRoot { return root.appending(path: "MCP") }
			return FileManager.default.homeDirectoryForCurrentUser.appending(path: "Library/Application Support/Synctrain/MCP")
		}
		var command: String { Bundle.main.bundleURL.appending(path: "Contents/MacOS/synctrain-mcp").path }
		var configuration: String {
			var server: [String: Any] = ["command": command]
			if EngineLocations.testRoot != nil {
				server["args"] = ["--socket", Self.directory.appending(path: "control.sock").path]
			}
			let value: [String: Any] = ["mcpServers": ["synctrain": server]]
			return (try? JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]))
				.flatMap { String(data: $0, encoding: .utf8) } ?? ""
		}
		func start(_ state: AppState? = nil) {
			guard server == nil else { return }
			appState = state
			let delegate = MCPAppDelegate()
			self.delegate = delegate
			var failure: NSError?
			server = HostbridgeNewServer(state?.client, delegate, Self.directory.path, &failure)
			enabled = server?.isEnabled() ?? false
			error = failure?.localizedDescription
		}
		func setEnabled(_ value: Bool) {
			do {
				guard let server else { throw MCPAppError("The MCP service is unavailable. Restart Synctrain.") }
				try server.setEnabled(value)
				enabled = server.isEnabled()
				error = nil
			} catch { self.error = error.localizedDescription }
		}
		func refresh() { enabled = server?.isEnabled() ?? false }
		func stop() async {
			guard let server else { return }
			self.server = nil
			try? await goBlocking { server.close() }
			delegate = nil
		}

		func perform(_ method: String, data: Data, cancellation: MCPCancellation? = nil) async throws -> Data {
			try Task.checkCancellation()
			if cancellation?.isCanceled == true { throw CancellationError() }
			let parameters = try JSONSerialization.jsonObject(with: data) as? [String: Any] ?? [:]
			if method == "native.pause" {
				guard let appState else { throw MCPAppError("The engine is not running.") }
				let ids = parameters["ids"] as? [String] ?? []
				let paused = parameters["paused"] as? Bool ?? false
				appState.followDevicePauses(paused: paused ? ids : [], resumed: paused ? [] : ids)
				return Data("null".utf8)
			}
			if method.hasPrefix("mcp.preferences.") || method.hasPrefix("policy.") || method == "resources.set" {
				return try preferences(method, parameters: parameters)
			}
			if method.hasPrefix("service.") || method.hasPrefix("exports.") || method.hasPrefix("clients.")
				|| method.hasPrefix("pairing.") {
				guard appState != nil else { throw MCPAppError("The remote file service requires Synchronize mode.") }
				return try await GatewayController.shared.mcpCall(method, parameters: parameters)
			}
			if method.hasPrefix("history.") { return try await history(method, parameters: parameters) }
			return try await files(method, parameters: parameters, cancellation: cancellation)
		}
		nonisolated static func json(_ value: Any) throws -> Data {
			try JSONSerialization.data(withJSONObject: value, options: [.fragmentsAllowed, .sortedKeys])
		}
		nonisolated static func encoded<T: Encodable>(_ value: T) throws -> Data { try JSONEncoder().encode(value) }
	}

	struct MCPAppError: LocalizedError, Sendable {
		let message: String
		init(_ message: String) { self.message = message }
		var errorDescription: String? { message }
	}

	/// Go cancellation and progress methods synchronize their state internally.
	final class MCPCancellation: @unchecked Sendable {
		private let value: HostbridgeCancellation?
		nonisolated init(_ value: HostbridgeCancellation?) { self.value = value }
		nonisolated var isCanceled: Bool { value?.isCanceled() == true }
		nonisolated func reportProgress(_ completed: Int64, total: Int64) { value?.reportProgress(completed, total: total) }
	}

	/// The Go listener invokes this delegate on its own worker threads. Only Data
	/// crosses to the main actor; no UI object or untyped JSON crosses actors.
	private final class MCPAppDelegate: NSObject, HostbridgeDelegateProtocol, @unchecked Sendable {
		nonisolated func call(_ method: String?, params: Data?, cancellation: HostbridgeCancellation?) throws -> Data {
			let method = method ?? ""
			if method == "native.methods" {
				return try MCPIntegration.json(Self.methods)
			}
			let result = OSAllocatedUnfairLock<Result<Data, any Error>?>(initialState: nil)
			let finished = DispatchSemaphore(value: 0)
			let data = params ?? Data("{}".utf8)
			let progress = MCPCancellation(cancellation)
			let operation = Task { @MainActor in
				let outcome: Result<Data, any Error>
				do {
					outcome = .success(try await MCPIntegration.shared.perform(method, data: data, cancellation: progress))
				} catch { outcome = .failure(error) }
				result.withLock { $0 = outcome }
				finished.signal()
			}
			while finished.wait(timeout: .now() + 0.1) == .timedOut {
				if cancellation?.isCanceled() == true {
					operation.cancel()
					throw CancellationError()
				}
			}
			return try result.withLock { try $0!.get() }
		}
		private nonisolated static let methods = [
			"status", "native.pause", "policy.get", "policy.save", "resources.set", "service.status", "service.set",
			"exports.list", "exports.set", "exports.remove",
			"clients.list", "clients.revoke", "clients.permission", "pairing.pending", "pairing.approve", "pairing.deny",
			"mcp.files.folders", "mcp.files.gateways", "mcp.files.add", "mcp.files.remove", "mcp.files.pair", "mcp.files.forget",
			"mcp.files.available", "mcp.files.children", "mcp.files.search", "mcp.files.inspect", "mcp.files.read",
			"mcp.files.replace", "mcp.files.mkdir", "mcp.files.import", "mcp.files.rename", "mcp.files.move", "mcp.files.delete",
			"mcp.files.download",
			"mcp.files.wait", "mcp.files.versions", "mcp.files.versionRestore", "mcp.files.versionExport",
			"mcp.files.versionCopy", "mcp.files.pending.list", "mcp.files.pending.export", "mcp.files.pending.discard",
			"mcp.files.transfers", "mcp.files.storage", "mcp.files.storage.budget", "mcp.preferences.get",
			"mcp.preferences.save",
			"mcp.preferences.close",
			"history.sources", "history.pair", "history.enable", "history.forget", "history.list", "history.export"
		]
	}
#endif
