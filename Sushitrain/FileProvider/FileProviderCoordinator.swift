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
		/// Files the user tried to open in the Files app while the app was not running (newest first)
		private(set) var requestedDownloads: [RequestedDownload] = []
		/// Why items the user wanted to stop keeping on this device were kept (newest first)
		private(set) var keepNotices: [String] = []

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
		@ObservationIgnored private var hasMoreWork = false
		@ObservationIgnored private var retryTask: Task<Void, Never>? = nil
		@ObservationIgnored private var retryRejectedTask: Task<Void, Never>? = nil

		static let heartbeatInterval: TimeInterval = 2
		static let heartbeatValidity: TimeInterval = 6
		private static let workRetryInterval: TimeInterval = 15
		private static let requestedDownloadLifetime: TimeInterval = 24 * 3600
		private static let evictionLifetime: TimeInterval = 24 * 3600
		private static let fullPassInterval: TimeInterval = 6 * 3600
		private static let lastFullPassKey = "fileProviderLastFullPass"
		/// Set (e.g. from the Settings app) to rebuild the File Provider domain on the next launch
		private static let resetDomainKey = "resetFileProviderDomain"
		/// Increase when items are presented differently (e.g. other capabilities), so the system reads them again
		private static let itemFormat = 5
		private static let itemFormatKey = "fileProviderItemFormat"

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
				await refreshItemsIfFormatChanged()
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
				FileProviderStorage.setLocationURL(try? await manager?.getUserVisibleURL(for: .rootContainer))
			}
			catch {
				Log.warn("Could not query File Provider domains: \(error.localizedDescription)")
			}
		}

		/// The system keeps what it knows about items (such as what can be done with them) until their version changes.
		/// When a new version of the extension describes items differently, report all items as changed.
		private func refreshItemsIfFormatChanged() async {
			guard UserDefaults.standard.integer(forKey: Self.itemFormatKey) < Self.itemFormat, let store else {
				return
			}
			do {
				try await Task.detached(priority: .utility) {
					try store.write { writer throws(IndexError) in try writer.touchAllItems() }
				}.value
				UserDefaults.standard.set(Self.itemFormat, forKey: Self.itemFormatKey)
				Log.info("Reported all File Provider items as changed (item format \(Self.itemFormat))")
				signalWorkingSet()
			}
			catch {
				Log.warn("Could not refresh File Provider items: \(error.localizedDescription)")
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
				UserDefaults.standard.set(Self.itemFormat, forKey: Self.itemFormatKey)
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
			// A file may have been downloaded that a rejected change was waiting for
			retryRejectedChanges()
		}

		/// Has the system retry changes the extension rejected because they were not possible yet (such as saving a file
		/// that was still being downloaded), after a short delay to coalesce events
		private func retryRejectedChanges() {
			retryRejectedTask?.cancel()
			retryRejectedTask = Task {
				try? await Task.sleep(for: .seconds(1))
				if !Task.isCancelled {
					signalResolved(.cannotSynchronize)
				}
			}
		}

		private func signalResolved(_ code: NSFileProviderError.Code) {
			manager?.signalErrorResolved(NSFileProviderError(code)) { error in
				if let error {
					Log.warn("Could not signal the File Provider that an error was resolved: \(error.localizedDescription)")
				}
			}
		}

		/// For background runs: applies changes made in the Files app while the app was not running, and brings the
		/// index up to date, so that changes travel in both directions without the app being opened
		func backgroundRefresh() {
			guard let client else {
				return
			}
			processWork()
			indexer?.scheduleFolderReconciliation()
			for folderID in client.folders()?.asArray() ?? [] {
				indexer?.schedule(folderID: folderID, fullPass: false)
			}
		}

		/// Call when something changes which folders are shown in the Files app
		func folderConfigurationChanged() {
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

			// Files requested while the app was not running can be fetched now, and rejected changes may be possible now
			signalResolved(.serverUnreachable)
			signalResolved(.cannotSynchronize)
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

		/// Completes changes made in the Files app (selecting, finishing renames, scanning) and retries downloads that
		/// were requested while the app was not running
		func processWork() {
			guard let store, let client, let container else {
				return
			}
			guard !isProcessingWork else {
				hasMoreWork = true
				return
			}
			isProcessingWork = true
			hasMoreWork = false
			retryTask?.cancel()
			let runner = FileProviderWorkRunner(client: client, store: store, container: container)
			let backgroundTask = UIApplication.shared.beginBackgroundTask(withName: "Applying changes from Files")

			Task {
				var failed = false
				do {
					let work = try store.pendingWork(limit: 500)
					let changes = work.filter { $0.kind != .fetch && $0.kind != .evict }
					if !changes.isEmpty {
						let outcome = await Task.detached(priority: .utility) { runner.run(changes) }.value
						failed = outcome.shouldRetry
						for item in outcome.deselected {
							try store.enqueue(.evict, folderID: item.folderID, path: item.path)
						}
						if !outcome.refusals.isEmpty {
							keepNotices = Array((outcome.refusals + keepNotices).prefix(10))
						}
					}
					let evictions = try store.pendingWork(limit: 500).filter { $0.kind == .evict }
					if await !evict(evictions, store: store) {
						failed = true
					}
					try updateRequestedDownloads(work.filter { $0.kind == .fetch }, store: store)
				}
				catch {
					Log.warn("Could not process File Provider work: \(error)")
				}
				UIApplication.shared.endBackgroundTask(backgroundTask)
				isProcessingWork = false
				retryRejectedChanges()

				if hasMoreWork {
					processWork()
				}
				else if failed {
					// Selections are refused while a folder is busy (e.g. scanning); try again in a while
					retryTask = Task {
						try? await Task.sleep(for: .seconds(Self.workRetryInterval))
						if !Task.isCancelled {
							processWork()
						}
					}
				}
			}
		}

		/// Items that are no longer kept on this device do not need to take up space in the system's copy either. A file
		/// that was open recently (or is shown in Files) stays in use for a while; that is tried again on later passes,
		/// for a day. Returns false when some evictions have to be tried again.
		private func evict(_ evictions: [WorkItem], store: IndexStore) async -> Bool {
			guard let manager else {
				return true
			}
			var done = true
			for work in evictions {
				guard let item = try? store.item(folderID: work.folderID, path: work.path) else {
					try? store.completeWork(id: work.id)
					continue
				}
				do {
					try await manager.evictItem(identifier: NSFileProviderItemIdentifier(ItemIdentifiers.string(for: item.id)))
					try? store.completeWork(id: work.id)
				}
				catch let error as NSError where error.domain == NSPOSIXErrorDomain && error.code == Int(EBUSY) {
					if Date().timeIntervalSince(work.createdAt) > Self.evictionLifetime {
						try? store.completeWork(id: work.id)
					}
					else {
						try? store.failWork(id: work.id, error: error.localizedDescription)
						done = false
					}
				}
				catch {
					Log.warn("Could not evict \(work.path) from the File Provider: \(error.localizedDescription)")
					try? store.completeWork(id: work.id)
				}
			}
			return done
		}

		/// Lists the files that were requested while the app was not running, so the user can open them in Files now.
		/// They are not handed to the system to download (`requestDownloadForItem`): it schedules such downloads for
		/// later, usually when the app is suspended again, and while they wait or fail Files shows the location as
		/// paused. Requests that were not followed up on expire.
		private func updateRequestedDownloads(_ fetches: [WorkItem], store: IndexStore) throws {
			var current: [RequestedDownload] = []
			for work in fetches {
				if Date().timeIntervalSince(work.createdAt) > Self.requestedDownloadLifetime {
					try store.completeWork(id: work.id)
				}
				else {
					current.append(RequestedDownload(work: work))
				}
			}
			requestedDownloads = current.sorted { $0.requestedAt > $1.requestedAt }
		}

		/// Opens a requested file in the Files app; the app stays awake long enough for the system to download it
		func showInFiles(_ download: RequestedDownload) {
			guard let container else {
				return
			}
			openURLInSystemFilesApp(url: container.fileURL(folderID: download.folderID, path: download.path))
		}
	}

	struct RequestedDownload: Identifiable, Hashable {
		let id: Int64
		let folderID: String
		let path: String
		let requestedAt: Date

		init(work: WorkItem) {
			id = work.id
			folderID = work.folderID
			path = work.path
			requestedAt = work.createdAt
		}

		var name: String {
			FileNames.lastComponent(path)
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
