// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	@preconcurrency import FileProvider
	import os
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
		private(set) var isRebuilding = false

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
		/// Since when devices have been able to connect (the app started or became active)
		@ObservationIgnored private var devicesSince = Date()

		static let heartbeatInterval: TimeInterval = 2
		static let heartbeatValidity: TimeInterval = 6
		private static let workRetryInterval: TimeInterval = 15
		nonisolated private static let requestedDownloadLifetime: TimeInterval = 24 * 3600
		nonisolated private static let evictionLifetime: TimeInterval = 24 * 3600
		private static let fullPassInterval: TimeInterval = 6 * 3600
		private static let lastFullPassKey = "fileProviderLastFullPass"
		/// How much queued work is handled at a time
		nonisolated private static let workBatchSize = 500
		/// When set, the File Provider domain is rebuilt on the next launch
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
			let indexer = FileProviderIndexer(client: client, store: store) { [weak self] in
				self?.signalWorkingSet()
			}
			indexer.onFullPassCompleted = {
				UserDefaults.standard.set(Date().timeIntervalSince1970, forKey: Self.lastFullPassKey)
			}
			self.indexer = indexer
			devicesSince = Date()

			startServer()
			// In the common modes, so that it also fires while the user scrolls
			let timer = Timer(timeInterval: Self.heartbeatInterval, repeats: true) { _ in
				Task { @MainActor in FileProviderCoordinator.shared.heartbeat() }
			}
			timer.tolerance = 0.5
			RunLoop.main.add(timer, forMode: .common)
			heartbeatTimer = timer

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
			indexer.scheduleFolderReconciliation(fullPass: needsFullPass)
			for folderID in client.folders()?.asArray() ?? [] {
				indexer.schedule(folderID: folderID, fullPass: false)
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
			let activity = BackgroundActivity(name: "Refreshing Files items")
			defer { activity.end() }
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

		/// Removes the Synctrain location (and the system's copy of its files) and sets it up again from a new index. The
		/// files in the Syncthing folders are not affected, but changes the system has not handed to the extension yet
		/// are lost (see `pendingItemCount`). The user has to enable the location again in Files.
		func rebuild() async {
			guard let indexer, !isRebuilding else {
				return
			}
			isRebuilding = true
			let activity = BackgroundActivity(name: "Rebuilding the Files location")
			defer {
				activity.end()
				isRebuilding = false
			}
			Log.info("Rebuilding the File Provider domain")
			do {
				try await NSFileProviderManager.remove(Self.domain)
			}
			catch {
				Log.warn("Could not remove the File Provider domain: \(error.localizedDescription)")
				lastError = error.localizedDescription
				await refreshDomainState()
				return
			}
			do {
				try await indexer.reset()
				UserDefaults.standard.set(Self.itemFormat, forKey: Self.itemFormatKey)
				lastError = nil
			}
			catch {
				Log.warn("Could not rebuild the Files index: \(error.localizedDescription)")
				lastError = error.localizedDescription
			}
			// Whatever happened to the index, the location has to come back
			do {
				try await NSFileProviderManager.add(Self.domain)
			}
			catch {
				Log.warn("Could not add the File Provider domain: \(error.localizedDescription)")
				lastError = error.localizedDescription
			}
			await refreshDomainState()
		}

		/// How many items have changes in the system's copy that the extension has not accepted yet (such as edits of
		/// files that were still being downloaded). Only the first page is counted.
		func pendingItemCount() async -> Int {
			guard let manager else {
				return 0
			}
			let enumerator = manager.enumeratorForPendingItems()
			defer { enumerator.invalidate() }
			return await withCheckedContinuation { continuation in
				let counter = PendingItemCounter { continuation.resume(returning: $0) }
				enumerator.enumerateItems(
					for: counter, startingAt: NSFileProviderPage(NSFileProviderPage.initialPageSortedByName as Data))
			}
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
			devicesSince = Date()
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
			devicesSince = Date()
			// Restarts the server only if the app was suspended, so downloads that continued in the background go on
			heartbeat()
			// The extension may have found the app suspended before the heartbeat went stale
			signalResolved(.serverUnreachable)
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
			let runner = FileProviderWorkRunner(
				client: client, store: store, container: container, devicesSince: devicesSince)
			// The work cannot be interrupted, but none of it keeps the index locked between steps
			let activity = BackgroundActivity(name: "Applying changes from Files")

			Task {
				let pass = await Task.detached(priority: .utility) { await Self.performWork(runner, store: store) }.value
				activity.end()
				isProcessingWork = false
				if !pass.refusals.isEmpty {
					keepNotices = Array((pass.refusals + keepNotices).prefix(10))
				}
				if let downloads = pass.requestedDownloads {
					requestedDownloads = downloads
				}
				retryRejectedChanges()

				if hasMoreWork || pass.hasMore {
					processWork()
				}
				else if pass.shouldRetry {
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

		private struct WorkPass: Sendable {
			var shouldRetry = false
			/// More work is waiting than was handled
			var hasMore = false
			var refusals: [String] = []
			var requestedDownloads: [RequestedDownload]? = nil
		}

		/// One pass over the queued work, off the main actor (it waits for Syncthing and for the index)
		nonisolated private static func performWork(_ runner: FileProviderWorkRunner, store: IndexStore) async -> WorkPass {
			var pass = WorkPass()
			do {
				let changes = try store.pendingWork(
					limit: workBatchSize, kinds: [.select, .deselectSafely, .rescan, .finishRename])
				if !changes.isEmpty {
					let outcome = runner.run(changes)
					pass.shouldRetry = outcome.shouldRetry
					// Work that keeps failing would come first again, so only continue when this batch got somewhere
					pass.hasMore = changes.count == workBatchSize && outcome.completed > 0
					for item in outcome.deselected {
						try store.enqueue(.evict, folderID: item.folderID, path: item.path)
					}
					pass.refusals = outcome.refusals
				}
				let evictions = try store.pendingWork(limit: workBatchSize, kinds: [.evict])
				if await !evict(evictions, store: store) {
					pass.shouldRetry = true
				}
				pass.requestedDownloads = try requestedDownloads(store: store)
			}
			catch {
				Log.warn("Could not process File Provider work: \(error)")
				pass.shouldRetry = true
			}
			return pass
		}

		/// Items that are no longer kept on this device do not need to take up space in the system's copy either. A file
		/// that was open recently (or is shown in Files) stays in use for a while; that is tried again on later passes,
		/// for a day. Returns false when some evictions have to be tried again.
		nonisolated private static func evict(_ evictions: [WorkItem], store: IndexStore) async -> Bool {
			guard !evictions.isEmpty, let manager = NSFileProviderManager(for: domain) else {
				return true
			}
			var done = true
			for work in evictions {
				// Kept on this device again in the meantime
				guard let item = try? store.item(folderID: work.folderID, path: work.path), !item.isSelected else {
					try? store.completeWork(id: work.id)
					continue
				}
				do {
					try await manager.evictItem(identifier: NSFileProviderItemIdentifier(ItemIdentifiers.string(for: item.id)))
					try? store.completeWork(id: work.id)
				}
				catch let error as NSError where error.domain == NSPOSIXErrorDomain && error.code == Int(EBUSY) {
					if Date().timeIntervalSince(work.createdAt) > evictionLifetime {
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

		/// The files that were requested while the app was not running, so the user can open them in Files now. They
		/// are not handed to the system to download (`requestDownloadForItem`): it schedules such downloads for later,
		/// usually when the app is suspended again, and while they wait or fail Files shows the location as paused.
		/// Requests that were not followed up on expire.
		nonisolated private static func requestedDownloads(store: IndexStore) throws -> [RequestedDownload] {
			var current: [RequestedDownload] = []
			for work in try store.pendingWork(limit: workBatchSize, kinds: [.fetch]) {
				if Date().timeIntervalSince(work.createdAt) > requestedDownloadLifetime {
					try store.completeWork(id: work.id)
				}
				else {
					current.append(RequestedDownload(work: work))
				}
			}
			return current.sorted { $0.requestedAt > $1.requestedAt }
		}

		/// Opens a requested file in the Files app; the app stays awake long enough for the system to download it
		func showInFiles(_ download: RequestedDownload) {
			Task {
				if let url = await FileProviderStorage.userVisibleURL(folderID: download.folderID, path: download.path) {
					openURLInSystemFilesApp(url: url)
				}
				else {
					lastError = String(localized: "This file is not shown in the Files app. Is the Synctrain location turned on?")
				}
			}
		}
	}

	/// Background time for work that should finish when the app leaves the foreground. Ends when the work is done or
	/// when the time is up, whichever comes first (iOS terminates apps that keep it longer).
	@MainActor final class BackgroundActivity {
		private var identifier = UIBackgroundTaskIdentifier.invalid

		init(name: String, onExpiration: (@MainActor @Sendable () -> Void)? = nil) {
			identifier = UIApplication.shared.beginBackgroundTask(withName: name) { [weak self] in
				MainActor.assumeIsolated {
					onExpiration?()
					self?.end()
				}
			}
		}

		func end() {
			guard identifier != .invalid else {
				return
			}
			UIApplication.shared.endBackgroundTask(identifier)
			identifier = .invalid
		}
	}

	/// Counts the items of the first page of an enumeration
	private final class PendingItemCounter: NSObject, NSFileProviderEnumerationObserver, @unchecked Sendable {
		private let lock = OSAllocatedUnfairLock(initialState: (count: 0, finished: false))
		private let completion: @Sendable (Int) -> Void

		init(completion: @escaping @Sendable (Int) -> Void) {
			self.completion = completion
		}

		func didEnumerate(_ updatedItems: [any NSFileProviderItemProtocol]) {
			lock.withLock { $0.count += updatedItems.count }
		}

		func finishEnumerating(upTo nextPage: NSFileProviderPage?) {
			finish()
		}

		func finishEnumeratingWithError(_ error: any Error) {
			finish()
		}

		private func finish() {
			let count: Int? = lock.withLock { state in
				guard !state.finished else {
					return nil
				}
				state.finished = true
				return state.count
			}
			if let count {
				completion(count)
			}
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
