// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import CryptoKit
	import FinderSync
	import SwiftUI
	@preconcurrency import SushitrainCore
	import SynctrainHostKit

	/// Serializes atomic publications with shutdown; engine queries never hold this lock.
	private final class FinderPublication: @unchecked Sendable {
		private let lock = NSLock()
		private var active = true
		func disable() {
			lock.lock()
			active = false
			lock.unlock()
		}
		func write<Value: Encodable>(_ value: Value, store: FinderSyncStore, name: String) throws {
			lock.lock()
			defer { lock.unlock() }
			if active { try store.write(value, name: name) }
		}
	}

	@MainActor final class FinderIntegration {
		static let shared = FinderIntegration()
		private weak var appState: AppState?
		private var store: FinderSyncStore?
		private var watcher: FinderDirectoryWatcher?
		private var worker: Task<Void, Never>?
		private var stopped = false
		private var lastRefresh = Date.distantPast
		private let publication = FinderPublication()

		nonisolated static func configurationID(_ directory: URL, deviceID: String, roots: [FinderRoot]) -> String {
			let identity = directory.resolvingSymlinksInPath().path + "\n" + deviceID + "\n"
			let encoder = JSONEncoder()
			encoder.outputFormatting = [.sortedKeys]
			let rootData = (try? encoder.encode(roots.sorted { $0.folderID < $1.folderID })) ?? Data()
			return SHA256.hash(data: Data(identity.utf8) + rootData).map { String(format: "%02x", $0) }.joined()
		}

		nonisolated static var directory: URL? {
			#if DEBUG
				if let root = EngineLocations.testRoot,
					ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_FINDER_SYNC"] == "1",
					FileManager.default.fileExists(atPath: root.appending(path: ".synctrain-finder-fixture").path)
				{
					return FinderSyncStore.sharedDirectory()
				}
			#endif
			return EngineLocations.testRoot.map { $0.appending(path: "Group/FinderSync") } ?? FinderSyncStore.sharedDirectory()
		}

		func start(_ appState: AppState) {
			self.appState = appState
			let directory = Self.directory
			guard let directory else { return }
			let store = FinderSyncStore(directory: directory)
			do {
				try store.prepare()
				self.store = store
				watcher = try FinderDirectoryWatcher(directory: directory) { Task { @MainActor in self.refresh() } }
				refresh()
			}
			catch { Log.warn("Could not start Finder integration: \(error)") }
		}

		func refresh() {
			guard !stopped, worker == nil, let appState, let store else { return }
			let client = appState.client
			let configDirectory = appState.configDirectory
			let publication = publication
			worker = Task {
				let delay = max(0, 1 - Date().timeIntervalSince(lastRefresh))
				try? await Task.sleep(for: .seconds(delay))
				guard !Task.isCancelled else { return }
				lastRefresh = Date()
				let again = await Task.detached(priority: .utility) {
					do {
						guard ["loaded", "running", "restarting", "stopped", "shutdown-requested"].contains(client.engineState()) else {
							return false
						}
						let data = try client.finderRootsJSON()
						let roots = try JSONDecoder().decode([FinderRoot].self, from: data)
						let configuration = Self.configurationID(configDirectory, deviceID: client.deviceID(), roots: roots)
						let registry = FinderRegistry(
							configuration: configuration, running: client.engineState() == "running", roots: roots)
						if store.read(FinderRegistry.self, name: "registry.json") != registry {
							try publication.write(registry, store: store, name: "registry.json")
						}
						let demands = store.demands()
						var capabilities =
							store.read(FinderCapabilities.self, name: "capabilities.json")
							?? FinderCapabilities(configuration: configuration)
						if capabilities.configuration != configuration { capabilities = FinderCapabilities(configuration: configuration) }
						let previousCapabilities = capabilities
						for demand in demands {
							guard demand.configuration == configuration else {
								store.remove(name: FinderSyncStore.responseName(demand.instance))
								continue
							}
							guard registry.running else { continue }
							let items = Array(Set(demand.items.filter { registry.url(for: $0) != nil }).prefix(FinderPaths.maximumItems))
								.sorted { ($0.folderID, $0.path) < ($1.folderID, $1.path) }
							let request = try JSONEncoder().encode(items)
							let result = try client.finderStatusJSON(request)
							let statuses = try JSONDecoder().decode([FinderItemStatus].self, from: result)
							capabilities.merge(statuses)
							try publication.write(
								FinderSnapshot(configuration: configuration, statuses: registry.running ? statuses : []),
								store: store, name: FinderSyncStore.responseName(demand.instance))
						}
						if capabilities != previousCapabilities {
							try publication.write(capabilities, store: store, name: "capabilities.json")
						}
						return !demands.isEmpty
					}
					catch {
						Log.warn("Could not refresh Finder status: \(error)")
						return false
					}
				}.value
				worker = nil
				if !stopped && again {
					refresh()
				}
			}
		}

		func stop() async {
			stopped = true
			watcher = nil
			publication.disable()
			worker?.cancel()
			guard let store else { return }
			await Task.detached(priority: .utility) {
				guard var registry = store.read(FinderRegistry.self, name: "registry.json") else { return }
				registry.running = false
				try? store.write(registry, name: "registry.json")
			}.value
		}

		static func clearRegistration() {
			guard let directory = Self.directory else { return }
			let store = FinderSyncStore(directory: directory)
			try? store.prepare()
			try? store.write(FinderRegistry(configuration: "", running: false, roots: []), name: "registry.json")
		}
	}

	struct FinderIntegrationSection: View {
		@Environment(\.scenePhase) private var scenePhase
		@State private var enabled = FIFinderSyncController.isExtensionEnabled
		var body: some View {
			Section {
				LabeledContent("Finder extension") {
					if enabled {
						Text("Enabled")
					}
					else {
						Text("Disabled")
					}
				}
				Button("Manage Finder Extension…") { FIFinderSyncController.showExtensionManagementInterface() }
			} footer: {
				Text(
					"Adds Synctrain menus and sync badges to synchronized folders. Badges show this Mac’s status while Synctrain is running."
				)
			}
			.onChange(of: scenePhase) { _, phase in
				if phase == .active { enabled = FIFinderSyncController.isExtensionEnabled }
			}
		}
	}

	/// Navigation requests are validated after startup, against the current configuration rather than Finder's cache.
	struct FinderActionModifier: ViewModifier {
		@Environment(AppState.self) private var appState
		let request: FinderActionRequest?
		let claim: (UUID) -> Bool
		let finish: (UUID) -> Void
		@State private var presented: FinderAction?
		@State private var error: String?
		@State private var activeRequest: UUID?
		@State private var delivery: Task<Void, Never>?
		@Environment(\.navigateTo) private var navigate

		func body(content: Content) -> some View {
			content.onChange(of: request?.id, initial: true) { _, id in
				guard let request, id == request.id, claim(request.id) else { return }
				activeRequest = request.id
				delivery = Task { await receive(request) }
			}
			.onDisappear {
				delivery?.cancel()
				complete()
			}
			.sheet(item: $presented, onDismiss: complete) { action in
				if action.kind == .versions, let folder = appState.client.folder(withID: action.item.folderID) {
					NavigationStack {
						FileVersionsView(folder: folder, path: action.item.path)
							.toolbar { Button("Done") { presented = nil } }
					}.frame(minWidth: 700, minHeight: 600)
				}
			}
			.alert(
				"Cannot Open Finder Item",
				isPresented: Binding(
					get: { error != nil },
					set: {
						if !$0 {
							error = nil
							complete()
						}
					})
			) {
				Button("OK") {
					error = nil
					complete()
				}
			} message: {
				Text(error ?? "")
			}
		}

		private func complete() {
			guard let activeRequest else { return }
			self.activeRequest = nil
			finish(activeRequest)
		}

		private func receive(_ request: FinderActionRequest) async {
			let action = request.action
			let result = await FinderActionValidator.status(for: action, appState: appState)
			guard !Task.isCancelled, activeRequest == request.id else { return }
			guard let result else {
				error = String(localized: "The requested item is unavailable or belongs to a different configuration.")
				return
			}
			NSApplication.shared.activate()
			if action.kind == .browse {
				let prefix =
					result.directory || action.item.path.isEmpty
					? action.item.path : (action.item.path as NSString).deletingLastPathComponent
				navigate(
					.folder(
						folderID: action.item.folderID, prefix: prefix.isEmpty ? "" : prefix.precomposedStringWithCanonicalMapping + "/"))
				complete()
			}
			else {
				var normalized = action
				normalized.item.path = action.item.path.precomposedStringWithCanonicalMapping
				presented = normalized
			}
		}
	}
#endif
