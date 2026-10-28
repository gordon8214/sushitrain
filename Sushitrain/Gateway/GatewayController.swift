// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import Foundation
	import os
	@preconcurrency import SushitrainCore

	/// Runs the remote file service (Docs/gateway.md) in the app, so that Synctrain on other devices can browse, open and
	/// edit this Mac's folders. The service only runs while the app does; while it runs, App Nap is kept away.
	@Observable @MainActor final class GatewayController {
		static let shared = GatewayController()

		enum State: Equatable {
			case stopped
			case starting
			case running
			case failed(String)
		}

		/// Whether the user turned the service on
		private(set) var isEnabled: Bool
		/// The settings the service runs with
		private(set) var settings: GatewaySettings
		private(set) var state = State.stopped
		private(set) var status: GatewayStatus? = nil
		/// Exports by folder ID
		private(set) var exports: [String: GatewayExport] = [:]
		/// Paired devices whose credential was not revoked
		private(set) var clients: [GatewayClient] = []
		private(set) var pendingOperations: [GatewayPendingOperation] = []
		/// This Mac's folders that can be exported
		private(set) var folders: [GatewayFolder] = []
		/// Why the service's state could not be read
		private(set) var refreshError: String? = nil

		@ObservationIgnored private var client: SushitrainClient? = nil
		@ObservationIgnored private var service: SushitrainRemoteFileService? = nil
		@ObservationIgnored private var activity: (any NSObjectProtocol)? = nil
		@ObservationIgnored private var transition: Task<Void, Never>? = nil
		@ObservationIgnored private var certificateRefresh: Task<Void, Never>? = nil
		/// The latest refresh: refreshes run one after the other, so that an older snapshot never replaces a newer one
		@ObservationIgnored private var refreshing: Task<Void, Never>? = nil
		@ObservationIgnored private var terminationObserver: (any NSObjectProtocol)? = nil
		@ObservationIgnored private var hasThumbnailRenderer = false

		private static let enabledKey = "remoteFileServiceEnabled"
		private static let refreshInterval: Duration = .seconds(3)
		private static let certificateRefreshInterval: Duration = .seconds(12 * 3600)
		/// How long quitting waits for the service to finish what it is doing, while blocking the main thread
		private static let stopTimeout: TimeInterval = 6
		/// How long quitting waits for the service without blocking: longer than the core takes at most (about 8 s)
		private static let terminationTimeout: TimeInterval = 9

		private init() {
			isEnabled = UserDefaults.standard.bool(forKey: Self.enabledKey)
			settings = GatewaySettings.load()
		}

		/// Starts the service if the user turned it on. Call once the client has started.
		func start(client: SushitrainClient) {
			guard self.client == nil else {
				return
			}
			self.client = client
			terminationObserver = NotificationCenter.default.addObserver(
				forName: NSApplication.willTerminateNotification, object: nil, queue: .main
			) { _ in
				MainActor.assumeIsolated { GatewayController.shared.stopForTermination() }
			}
			if isEnabled {
				enqueue { await $0.startService() }
			}
		}

		func setEnabled(_ enabled: Bool) {
			guard enabled != isEnabled else {
				return
			}
			isEnabled = enabled
			UserDefaults.standard.set(enabled, forKey: Self.enabledKey)
			if enabled {
				enqueue { await $0.startService() }
			}
			else {
				enqueue { await $0.stopService() }
			}
		}

		/// Saves new settings and restarts the service with them
		func apply(_ newSettings: GatewaySettings) {
			guard newSettings != settings else {
				return
			}
			settings = newSettings
			settings.save()
			restart()
		}

		/// Copies a certificate and private key from the chosen files and uses them
		func importCertificate(from urls: [URL]) async throws {
			try await GatewayCertificateStore.importFiles(urls)
			if settings.certificateMode == .files {
				restart()
			}
		}

		/// Starts the service again, as after a change of settings or certificate
		func restart() {
			guard isEnabled else {
				return
			}
			enqueue { controller in
				await controller.stopService()
				await controller.startService()
			}
		}

		/// Refreshes the service's state until the task is cancelled (while its settings are visible)
		func refreshWhileVisible() async {
			while !Task.isCancelled {
				await refresh()
				try? await Task.sleep(for: Self.refreshInterval)
			}
		}

		func refresh() async {
			let previous = refreshing
			let current = Task {
				await previous?.value
				await readState()
			}
			refreshing = current
			await current.value
		}

		private func readState() async {
			guard let client else {
				return
			}
			folders = await Self.exportableFolders(client)
			guard let service else {
				status = nil
				exports = [:]
				clients = []
				pendingOperations = []
				refreshError = nil
				return
			}
			do {
				let snapshot = try await Self.snapshot(service)
				// The service may have stopped meanwhile
				guard self.service === service else {
					return
				}
				status = snapshot.status
				exports = Dictionary(snapshot.exports.map { ($0.folderID, $0) }, uniquingKeysWith: { _, last in last })
				clients = snapshot.clients.filter { !$0.revoked }
				pendingOperations = snapshot.pendingOperations
				refreshError = nil
			}
			catch {
				guard self.service === service else {
					return
				}
				refreshError = error.localizedDescription
			}
		}

		// MARK: - Exports and clients

		/// Exports a folder (read only, or allowing changes), or stops exporting it
		func setExport(folderID: String, exported: Bool, allowWrite: Bool) async throws {
			let service = try runningService()
			try await Self.detached {
				if exported {
					try service.setExport(folderID, allowWrite: allowWrite)
				}
				else {
					try service.removeExport(folderID)
				}
			}
			await refresh()
		}

		func setAccess(clientID: String, folderID: String, access: GatewayAccess) async throws {
			let service = try runningService()
			try await Self.detached { try service.setPermission(clientID, folderID: folderID, access: access.rawValue) }
			await refresh()
		}

		func revoke(clientID: String) async throws {
			let service = try runningService()
			try await Self.detached { try service.revokeClient(clientID) }
			await refresh()
		}

		// MARK: - Pairing

		func startPairing() async throws -> GatewayPairingSession {
			let service = try runningService()
			let data = try await Self.detached { try service.startPairingJSON() }
			return try JSONDecoder.gateway().decode(GatewayPairingSession.self, from: data)
		}

		func pendingPairings() async throws -> [GatewayPendingPairing] {
			let service = try runningService()
			let data = try await Self.detached { try service.pendingPairingsJSON() }
			return try JSONDecoder.gateway().decodeList(GatewayPendingPairing.self, from: data)
		}

		/// Approves a pairing request, granting access to folders (folders without access are left out)
		func approvePairing(requestID: String, grants: [String: GatewayAccess]) async throws {
			let service = try runningService()
			let granted = grants.filter { $0.value != .none }.mapValues(\.rawValue)
			let grantsJSON = try JSONEncoder().encode(granted)
			try await Self.detached { try service.approvePairing(requestID, grantsJSON: grantsJSON) }
			await refresh()
		}

		func denyPairing(requestID: String) async throws {
			let service = try runningService()
			try await Self.detached { try service.denyPairing(requestID) }
		}

		// MARK: - Lifecycle

		/// Runs start and stop one after the other, in the order they were asked for
		private func enqueue(_ operation: @escaping @MainActor (GatewayController) async -> Void) {
			let previous = transition
			transition = Task {
				await previous?.value
				await operation(self)
			}
		}

		private func startService() async {
			guard isEnabled, service == nil, let client else {
				return
			}
			state = .starting
			do {
				let configuration = try await serviceConfiguration()
				if !hasThumbnailRenderer {
					// The core only offers thumbnails if the renderer is set before the service starts
					SushitrainSetRemoteThumbnailRenderer(QuickLookThumbnailRenderer())
					hasThumbnailRenderer = true
				}
				let started = try await Self.startService(client: client, configuration: configuration)
				service = started
				activity = ProcessInfo.processInfo.beginActivity(
					options: [.userInitiatedAllowingIdleSystemSleep],
					reason: String(localized: "Serving folders to paired devices"))
				state = .running
				if settings.certificateMode == .files {
					scheduleCertificateRefresh()
				}
				Log.info("Remote file service started on port \(started.port())")
			}
			catch {
				Log.warn("Could not start the remote file service: \(error.localizedDescription)")
				state = .failed(error.localizedDescription)
			}
			await refresh()
		}

		private func stopService() async {
			certificateRefresh?.cancel()
			certificateRefresh = nil
			guard let service else {
				state = .stopped
				return
			}
			self.service = nil
			state = .stopped
			do {
				try await Self.detached { try service.stop() }
				Log.info("Remote file service stopped")
			}
			catch {
				Log.warn("Could not stop the remote file service cleanly: \(error.localizedDescription)")
			}
			endActivity()
			await refresh()
		}

		/// For the app delegate's applicationShouldTerminate: stops the service before the app quits, without blocking
		/// the main thread. The app quits once the service stopped, or after terminationTimeout. Changes the service
		/// could not finish are completed from its journal at the next start.
		func terminationReply() -> NSApplication.TerminateReply {
			certificateRefresh?.cancel()
			certificateRefresh = nil
			guard let service else {
				return .terminateNow
			}
			self.service = nil
			state = .stopped
			Task {
				let stopped = await Self.stop(service, within: Self.terminationTimeout)
				if !stopped {
					Log.warn("The remote file service did not stop in time")
				}
				endActivity()
				NSApplication.shared.reply(toApplicationShouldTerminate: true)
			}
			return .terminateLater
		}

		/// Stops the service when the app quits, waiting a little for it to finish what it is doing. Changes it could
		/// not finish are completed from its journal at the next start. Only does something when the app quits without
		/// asking terminationReply first.
		private func stopForTermination() {
			certificateRefresh?.cancel()
			guard let service else {
				return
			}
			self.service = nil
			let stopped = DispatchSemaphore(value: 0)
			DispatchQueue.global(qos: .userInitiated).async {
				try? service.stop()
				stopped.signal()
			}
			if stopped.wait(timeout: .now() + Self.stopTimeout) == .timedOut {
				Log.warn("The remote file service did not stop in time")
			}
			endActivity()
		}

		private func endActivity() {
			if let activity {
				ProcessInfo.processInfo.endActivity(activity)
				self.activity = nil
			}
		}

		/// Picks up renewed certificate files now and then; the service reloads the certificate when its copy changes
		private func scheduleCertificateRefresh() {
			certificateRefresh?.cancel()
			certificateRefresh = Task {
				while !Task.isCancelled {
					try? await Task.sleep(for: Self.certificateRefreshInterval)
					if !Task.isCancelled {
						await GatewayCertificateStore.refreshFromOriginals()
					}
				}
			}
		}

		private func serviceConfiguration() async throws -> Data {
			var certFile = ""
			var keyFile = ""
			if settings.certificateMode == .files {
				await GatewayCertificateStore.refreshFromOriginals()
				guard GatewayCertificateStore.hasFiles else {
					throw GatewayError.noCertificateFiles
				}
				certFile = GatewayCertificateStore.certificateURL.path(percentEncoded: false)
				keyFile = GatewayCertificateStore.keyURL.path(percentEncoded: false)
			}
			// They may have changed in System Settings since the last start
			GatewaySettings.refreshSystemNames()
			let configuration = ServiceConfiguration(
				listenAddress: ":\(settings.port)", certFile: certFile, keyFile: keyFile,
				serviceName: settings.effectiveServiceName, publicHost: settings.effectivePublicHost,
				publicPort: settings.port, contentCacheMiB: settings.contentCacheMiB)
			return try JSONEncoder().encode(configuration)
		}

		private func runningService() throws -> SushitrainRemoteFileService {
			guard let service else {
				throw GatewayError.notRunning
			}
			return service
		}

		// MARK: - Calls into the core

		@concurrent nonisolated private static func startService(client: SushitrainClient, configuration: Data) async throws
			-> SushitrainRemoteFileService
		{
			try client.startRemoteFileService(configuration)
		}

		@concurrent nonisolated private static func snapshot(_ service: SushitrainRemoteFileService) async throws
			-> Snapshot
		{
			let decoder = JSONDecoder.gateway()
			return Snapshot(
				status: try decoder.decode(GatewayStatus.self, from: try service.statusJSON()),
				exports: try decoder.decodeList(GatewayExport.self, from: try service.exportsJSON()),
				clients: try decoder.decodeList(GatewayClient.self, from: try service.clientsJSON()),
				pendingOperations: try decoder.decodeList(
					GatewayPendingOperation.self, from: try service.pendingOperationsJSON()))
		}

		@concurrent nonisolated private static func exportableFolders(_ client: SushitrainClient) async -> [GatewayFolder] {
			let folders = (client.folders()?.asArray() ?? []).compactMap { client.folder(withID: $0) }
			return folders.filter { $0.exists() && !$0.isPhotoFolder && !$0.isReceiveEncryptedFolder }
				.sorted()
				.map { folder in
					var restriction: String? = nil
					if folder.folderType() != SushitrainFolderTypeSendReceive {
						restriction = String(localized: "Only send-receive folders can be changed from other devices.")
					}
					else if folder.isSelective() != false {
						restriction = String(
							localized: "Only folders that are kept on this Mac in full can be changed from other devices.")
					}
					return GatewayFolder(id: folder.folderID, name: folder.displayName, writeRestriction: restriction)
				}
		}

		/// Stops the service on another thread, and reports whether it stopped within the timeout (it goes on stopping
		/// otherwise)
		@concurrent nonisolated private static func stop(_ service: SushitrainRemoteFileService, within timeout: TimeInterval)
			async -> Bool
		{
			await withCheckedContinuation { continuation in
				let pending = OSAllocatedUnfairLock<CheckedContinuation<Bool, Never>?>(initialState: continuation)
				let finish: @Sendable (Bool) -> Void = { stopped in
					let waiting = pending.withLock { value in
						defer { value = nil }
						return value
					}
					waiting?.resume(returning: stopped)
				}
				DispatchQueue.global(qos: .userInitiated).async {
					try? service.stop()
					finish(true)
				}
				DispatchQueue.global(qos: .userInitiated).asyncAfter(deadline: .now() + timeout) {
					finish(false)
				}
			}
		}

		/// Runs a blocking call into the core away from the main actor
		@concurrent nonisolated private static func detached<T: Sendable>(_ work: @Sendable () throws -> T) async throws
			-> T
		{
			try work()
		}
	}

	private struct Snapshot: Sendable {
		let status: GatewayStatus
		let exports: [GatewayExport]
		let clients: [GatewayClient]
		let pendingOperations: [GatewayPendingOperation]
	}

	/// The settings `SushitrainClient.startRemoteFileService` takes
	private struct ServiceConfiguration: Encodable {
		let listenAddress: String
		let certFile: String
		let keyFile: String
		let serviceName: String
		let publicHost: String
		let publicPort: Int
		let contentCacheMiB: Int
	}

	enum GatewayError: LocalizedError {
		case notRunning
		case noCertificateFiles

		var errorDescription: String? {
			switch self {
			case .notRunning:
				return String(localized: "The remote file service is not running.")
			case .noCertificateFiles:
				return String(localized: "Choose the certificate and private key files first.")
			}
		}
	}
#endif
