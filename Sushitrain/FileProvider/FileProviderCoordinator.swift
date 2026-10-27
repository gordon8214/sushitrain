// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	@preconcurrency import FileProvider
	import UIKit
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Runs the app's side of the File Provider: keeps the shared index current, serves file contents to the extension
	/// while the app is awake, and handles work the extension queued.
	@Observable @MainActor final class FileProviderCoordinator {
		static let shared = FileProviderCoordinator()

		/// Whether the user enabled the Synctrain location in the Files app (nil while unknown)
		private(set) var isDomainEnabled: Bool? = nil
		private(set) var lastError: String? = nil
		/// Number of downloads requested in the Files app while the app was not running
		private(set) var pendingFetchCount = 0

		@ObservationIgnored private var client: SushitrainClient? = nil
		@ObservationIgnored private var store: IndexStore? = nil
		@ObservationIgnored private var container: SharedContainer? = nil
		@ObservationIgnored private var indexer: FileProviderIndexer? = nil
		@ObservationIgnored private var server: SushitrainFileProviderServer? = nil
		@ObservationIgnored private var heartbeatTimer: Timer? = nil
		@ObservationIgnored private var lastHeartbeatAt: Date? = nil
		@ObservationIgnored private var coreDelegate: FileProviderCoreDelegate? = nil
		@ObservationIgnored private var workObserver: SharedNotificationObserver? = nil
		@ObservationIgnored private var signalTask: Task<Void, Never>? = nil
		@ObservationIgnored private var observers: [any NSObjectProtocol] = []
		@ObservationIgnored private var isProcessingWork = false

		static let heartbeatInterval: TimeInterval = 2
		static let heartbeatValidity: TimeInterval = 6
		private static let fullPassInterval: TimeInterval = 6 * 3600
		private static let lastFullPassKey = "fileProviderLastFullPass"
		/// Set (e.g. from the Settings app) to rebuild the File Provider domain on the next launch
		private static let resetDomainKey = "resetFileProviderDomain"

		nonisolated static var domain: NSFileProviderDomain {
			let domain = NSFileProviderDomain(
				identifier: NSFileProviderDomainIdentifier(rawValue: SharedContainer.domainIdentifier),
				displayName: "Synctrain")
			if #available(iOS 18.0, *) {
				// Deleting through the Files app deletes on all devices; Syncthing versioning keeps old copies if enabled
				domain.supportsSyncingTrash = false
			}
			return domain
		}

		private var manager: NSFileProviderManager? {
			NSFileProviderManager(for: Self.domain)
		}

		/// Starts the File Provider integration. Call once the client has started.
		func start(client: SushitrainClient) {
			guard self.client == nil else {
				return
			}
			guard let container = FileProviderStorage.container else {
				Log.warn("Not starting the File Provider: the app group container is unavailable")
				return
			}

			self.client = client
			self.container = container
			let store = IndexStore(container: container)
			self.store = store
			self.indexer = FileProviderIndexer(client: client, store: store) { [weak self] in
				self?.signalWorkingSet()
			}

			startServer()
			heartbeatTimer = Timer.scheduledTimer(withTimeInterval: Self.heartbeatInterval, repeats: true) { _ in
				Task { @MainActor in FileProviderCoordinator.shared.heartbeat() }
			}

			let coreDelegate = FileProviderCoreDelegate()
			self.coreDelegate = coreDelegate
			client.setFileProviderDelegate(coreDelegate)

			workObserver = SharedNotificationObserver(.workQueued) {
				Task { @MainActor in FileProviderCoordinator.shared.processWork() }
			}

			let center = NotificationCenter.default
			observers.append(
				center.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) {
					_ in
					MainActor.assumeIsolated { FileProviderCoordinator.shared.didEnterForeground() }
				})
			observers.append(
				center.addObserver(forName: .fileProviderDomainDidChange, object: nil, queue: .main) { _ in
					Task { @MainActor in await FileProviderCoordinator.shared.refreshDomainState() }
				})

			Task {
				if UserDefaults.standard.bool(forKey: Self.resetDomainKey) {
					UserDefaults.standard.set(false, forKey: Self.resetDomainKey)
					await self.rebuild()
				}
				else {
					await self.registerDomain()
				}
			}

			// Catch up with everything that happened while the app was not running
			let lastFullPass = UserDefaults.standard.double(forKey: Self.lastFullPassKey)
			let needsFullPass = Date().timeIntervalSince1970 - lastFullPass > Self.fullPassInterval
			if needsFullPass {
				UserDefaults.standard.set(Date().timeIntervalSince1970, forKey: Self.lastFullPassKey)
			}
			indexer?.scheduleFolderReconciliation(fullPass: needsFullPass)
			for folderID in client.folders()?.asArray() ?? [] {
				indexer?.schedule(folderID: folderID, fullPass: false)
			}
			processWork()
		}

		// MARK: - Domain

		private func registerDomain() async {
			do {
				try await NSFileProviderManager.add(Self.domain)
				lastError = nil
			}
			catch {
				Log.warn("Could not register the File Provider domain: \(error.localizedDescription)")
				lastError = error.localizedDescription
			}
			await refreshDomainState()
		}

		func refreshDomainState() async {
			do {
				let domains = try await NSFileProviderManager.domains()
				isDomainEnabled =
					domains.first { $0.identifier.rawValue == SharedContainer.domainIdentifier }?.userEnabled
			}
			catch {
				Log.warn("Could not query File Provider domains: \(error.localizedDescription)")
			}
		}

		/// Removes the Synctrain location (and the system's copy of its files) and sets it up again from a new index.
		/// Nothing is lost: all files are in the Syncthing folders. The user has to enable the location again in Files.
		func rebuild() async {
			guard let store else {
				return
			}
			do {
				Log.info("Rebuilding the File Provider domain")
				try await NSFileProviderManager.remove(Self.domain)
				try store.reset()
				try await NSFileProviderManager.add(Self.domain)
				lastError = nil
				indexer?.scheduleFolderReconciliation(fullPass: true)
			}
			catch {
				Log.warn("Could not rebuild the Files index: \(error.localizedDescription)")
				lastError = error.localizedDescription
			}
			await refreshDomainState()
		}

		// MARK: - Changes

		fileprivate func folderIndexChanged(folderID: String, fullPass: Bool) {
			indexer?.schedule(folderID: folderID, fullPass: fullPass)
		}

		fileprivate func folderConfigurationChanged() {
			indexer?.scheduleFolderReconciliation()
		}

		/// Tells the system to fetch changes from the extension, after a short delay to coalesce updates
		private func signalWorkingSet() {
			signalTask?.cancel()
			signalTask = Task {
				try? await Task.sleep(for: .milliseconds(500))
				if Task.isCancelled {
					return
				}
				do {
					try await manager?.signalEnumerator(for: .workingSet)
				}
				catch {
					Log.warn("Could not signal the File Provider: \(error.localizedDescription)")
				}
			}
		}

		// MARK: - Serving files to the extension

		private func startServer() {
			guard let client else {
				return
			}
			let server = self.server ?? SushitrainNewFileProviderServer(client)
			self.server = server
			do {
				try server?.listen()
			}
			catch {
				Log.warn("Could not start the File Provider server: \(error.localizedDescription)")
			}
			heartbeat()
		}

		/// Tells the extension the app can serve files for a few more seconds. While the app is suspended this does not
		/// run, so the heartbeat goes stale and the extension stops trying to reach the app.
		private func heartbeat() {
			guard let container, let server else {
				return
			}
			let now = Date()
			if let previous = lastHeartbeatAt, now.timeIntervalSince(previous) > Self.heartbeatValidity {
				// We were suspended; the listening socket may have been reclaimed
				lastHeartbeatAt = now
				startServer()
				return
			}
			lastHeartbeatAt = now

			let heartbeat = Heartbeat(
				awakeUntil: now.addingTimeInterval(Self.heartbeatValidity), port: server.port(), token: server.token())
			do {
				try heartbeat.write(to: container.heartbeatURL)
			}
			catch {
				Log.warn("Could not write the File Provider heartbeat: \(error.localizedDescription)")
			}
		}

		private func didEnterForeground() {
			startServer()
			processWork()
			Task {
				await refreshDomainState()
			}
		}

		// MARK: - Work queued by the extension

		func processWork() {
			guard let store, !isProcessingWork else {
				return
			}
			isProcessingWork = true
			Task {
				defer { isProcessingWork = false }
				do {
					let work = try store.pendingWork()
					pendingFetchCount = work.filter { $0.kind == .fetch }.count
					for item in work {
						await perform(item, store: store)
					}
					pendingFetchCount = try store.pendingWork().filter { $0.kind == .fetch }.count
				}
				catch {
					Log.warn("Could not process File Provider work: \(error)")
				}
			}
		}

		private func perform(_ item: WorkItem, store: IndexStore) async {
			switch item.kind {
			case .fetch:
				// The user opened a file in the Files app while we were not running; now that we are, have the
				// system ask the extension again (which will fetch through us)
				guard UIApplication.shared.applicationState != .background, let manager, let extra = item.extra else {
					return
				}
				do {
					try await manager.requestDownloadForItem(withIdentifier: NSFileProviderItemIdentifier(extra))
					try store.completeWork(id: item.id)
				}
				catch {
					Log.warn("Could not request download of item \(extra): \(error.localizedDescription)")
					try? store.failWork(id: item.id, error: error.localizedDescription)
					if item.attempts >= 3 {
						try? store.completeWork(id: item.id)
					}
				}

			case .select, .deselectSafely, .rescan, .finishRename:
				// Handled once the File Provider supports making changes
				break
			}
		}
	}

	/// Receives notifications from the core (on arbitrary threads) and forwards them to the coordinator
	final class FileProviderCoreDelegate: NSObject, SushitrainFileProviderDelegateProtocol, @unchecked Sendable {
		func onFolderIndexChanged(_ folderID: String?, needsFullPass: Bool) {
			guard let folderID else {
				return
			}
			Task { @MainActor in
				FileProviderCoordinator.shared.folderIndexChanged(folderID: folderID, fullPass: needsFullPass)
			}
		}

		func onFolderConfigurationChanged() {
			Task { @MainActor in
				FileProviderCoordinator.shared.folderConfigurationChanged()
			}
		}
	}
#endif
