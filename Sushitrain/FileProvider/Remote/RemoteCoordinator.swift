// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	@preconcurrency import FileProvider
	import Foundation
	import os
	import UIKit
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Runs the app's side of folders accessed through gateways. It refreshes their metadata (at start, on foreground,
	/// after pairing and in background runs, and by long polling while the app is active), sends the edits waiting in
	/// the shared outbox, finishes the app's background transfers, resumes folder switches that were interrupted, and
	/// keeps what the settings show about every gateway.
	@Observable @MainActor final class RemoteCoordinator {
		static let shared = RemoteCoordinator()

		/// The paired gateways, by name
		private(set) var gateways: [RemoteGatewayStatus] = []
		/// Folders accessed through a gateway, by local folder ID
		private(set) var bindings: [String: RemoteFolderBinding] = [:]
		/// Folder switches in progress, by local folder ID
		private(set) var migrations: [String: RemoteMigration] = [:]
		/// Folders that Syncthing on this device leaves to a gateway
		private(set) var remoteManagedFolderIDs: Set<String> = []
		/// Edits being sent or needing review, newest first
		private(set) var operations: [RemoteOperationSummary] = []
		/// Whether some edits are not resolved with their gateways (which rules out rebuilding the Files location)
		private(set) var hasUnresolvedOperations = false
		/// Folders being switched by the user right now
		private(set) var switchingFolders: Set<String> = []
		private(set) var isRefreshing = false
		/// A problem the user should know about (such as a switch that could not be finished)
		private(set) var notice: String? = nil

		@ObservationIgnored private var client: SushitrainClient? = nil
		@ObservationIgnored private var isStarted = false
		@ObservationIgnored private var appTransfers: BackgroundTransfers? = nil
		/// The extension's background session, recreated only to finish its events
		@ObservationIgnored private var extensionTransfers: BackgroundTransfers? = nil
		@ObservationIgnored private var observers: [any NSObjectProtocol] = []
		@ObservationIgnored private var workObserver: SharedNotificationObserver? = nil
		@ObservationIgnored private var refreshTask: Task<Void, Never>? = nil
		@ObservationIgnored private var pollTask: Task<Void, Never>? = nil
		@ObservationIgnored private var outboxTask: Task<Void, Never>? = nil
		@ObservationIgnored private var outboxSleep: Task<Void, Never>? = nil
		@ObservationIgnored private var outboxPoked = false
		/// Hands the rest of the upload in progress to the app's background session (for the pass running now)
		@ObservationIgnored private var outboxHandoff: BackgroundHandoff? = nil
		@ObservationIgnored private var lastRefresh: Date? = nil
		/// When Files was last told that what it was told about a gateway is resolved, by service ID
		@ObservationIgnored private var lastResolvedSignal: [String: Date] = [:]
		/// Folders whose interrupted switch is being continued now
		@ObservationIgnored private var resumingFolders: Set<String> = []

		/// How long the outbox sleeps when nothing is waiting (pokes wake it earlier)
		private static let idleOutboxInterval: Duration = .seconds(300)
		/// How long gateways that could not be used wait between checks while the app is active
		private static let recheckInterval: TimeInterval = 60
		/// The longest long poll (shorter ones let several folders take turns)
		private static let longPollLimit = 30
		/// Background time kept in reserve when refreshing from a background run
		private static let backgroundReserve: TimeInterval = 10
		/// How often Files is told at most that errors it was given about a gateway that answers are resolved
		private static let resolvedSignalInterval: TimeInterval = 60
		/// Background time left when the rest of an upload goes to the app's background session: enough to ask the
		/// gateway for the offset, copy the rest and start the task before iOS suspends the app
		static let handoffReserve: TimeInterval = 20
		/// How long the app works on what a background session's tasks finished before it tells the system that their
		/// events are handled (the system then suspends it)
		private static let eventsBudget: Duration = .seconds(20)
		/// The operations whose background upload task ended, as the session delegates reported them
		private nonisolated static let finishedUploads = OSAllocatedUnfairLock<Set<String>>(initialState: [])

		var environment: RemoteEnvironment? { RemoteEnvironment.shared }

		// MARK: - Starting

		/// Starts the work with gateways. Call once the client has started, after the File Provider coordinator.
		func start(client: SushitrainClient) {
			guard !isStarted, let environment else {
				return
			}
			isStarted = true
			self.client = client
			_ = backgroundTransfers(for: environment.appSessionIdentifier)

			let center = NotificationCenter.default
			observers.append(
				center.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) {
					_ in
					MainActor.assumeIsolated { RemoteCoordinator.shared.didEnterForeground() }
				})
			observers.append(
				center.addObserver(forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main) { _ in
					MainActor.assumeIsolated { RemoteCoordinator.shared.startLongPolling() }
				})
			observers.append(
				center.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) {
					_ in
					MainActor.assumeIsolated { RemoteCoordinator.shared.stopLongPolling() }
				})
			// The extension posts this after recording work, which includes edits of remote folders
			workObserver = SharedNotificationObserver(.workQueued) {
				Task { @MainActor in RemoteCoordinator.shared.pokeOutbox() }
			}

			Task {
				do {
					try await Task.detached(priority: .utility) { try environment.storage.createDirectories() }.value
				}
				catch {
					Log.warn("Could not create the directories for remote folders: \(error.localizedDescription)")
				}
				await reloadState()
				restoreFences()
				// Switches that were interrupted continue before anything is sent for their folders
				await resumeMigrations()
				await reconcileTransfers()
				startOutbox()
				await refresh()
				if UIApplication.shared.applicationState == .active {
					startLongPolling()
				}
			}
		}

		private func didEnterForeground() {
			pokeOutbox()
			Task {
				await refresh()
			}
		}

		/// For background runs: refreshes the gateways and sends waiting edits, within the run's time
		func backgroundRefresh() {
			guard isStarted else {
				return
			}
			let remaining = UIApplication.shared.backgroundTimeRemaining
			let budget = min(max(remaining - Self.backgroundReserve, 0), 60)
			guard budget > 1 else {
				return
			}
			pokeOutbox()
			let wasRefreshing = refreshTask != nil
			let started = startRefresh()
			guard !wasRefreshing else {
				// Not started for this run: it is not canceled when the run's time is up
				return
			}
			Task {
				try? await Task.sleep(for: .seconds(budget))
				// Requests still waiting give up rather than outlast the run
				started.cancel()
			}
		}

		/// The Syncthing name of this device, which gateways show when it asks to pair
		var deviceName: String {
			var error: NSError? = nil
			let name = client?.getName(&error) ?? ""
			return name.isEmpty ? UIDevice.current.name : name
		}

		/// Folders bound to a gateway, or switched part of the way, stay fenced from Syncthing on this device. The index
		/// is the record of that; this restores Syncthing's own list of them if it was lost or set aside as damaged.
		private func restoreFences() {
			guard let client else {
				return
			}
			let fencedStates: Set<RemoteMigrationState> = [.engineStopped, .bound, .returning]
			let folderIDs = Set(bindings.keys).union(
				migrations.values.filter { fencedStates.contains($0.state) }.map(\.folderID))
			for folderID in folderIDs.sorted() where !client.isFolderRemoteManaged(folderID) {
				// Folders added from a gateway have no Syncthing folder on this device
				guard let folder = client.folder(withID: folderID), folder.exists() else {
					continue
				}
				do {
					try client.setFolderRemoteManaged(folderID, managed: true)
					Log.warn("Restored the gateway fence of folder \(folderID)")
				}
				catch {
					Log.warn("Could not restore the gateway fence of folder \(folderID): \(error.localizedDescription)")
				}
			}
			update(\.remoteManagedFolderIDs, to: Set(client.remoteManagedFolderIDs()?.asArray() ?? []))
		}

		// MARK: - State

		/// Reads bindings, switches, gateways and operations from the index again
		func reloadState() async {
			guard let environment else {
				return
			}
			let client = self.client
			let state: RemoteWork.State
			do {
				state = try await Task.detached(priority: .utility) {
					try RemoteWork.loadState(environment: environment, client: client)
				}.value
			}
			catch {
				Log.warn("Could not read the state of remote folders: \(error)")
				return
			}
			// Only what changed, so that views showing the rest are not updated
			update(\.bindings, to: state.bindings)
			update(\.migrations, to: state.migrations)
			update(\.operations, to: state.operations)
			update(\.hasUnresolvedOperations, to: state.hasUnresolvedOperations)
			update(\.remoteManagedFolderIDs, to: state.remoteManaged)
			let known = Dictionary(uniqueKeysWithValues: gateways.map { ($0.id, $0) })
			update(
				\.gateways,
				to: state.endpoints.map { endpoint in
					var status = known[endpoint.serviceID] ?? RemoteGatewayStatus(endpoint: endpoint)
					status.endpoint = endpoint
					return status
				})
		}

		/// Assigns an observed property only when the value changes (an assignment notifies its observers either way)
		private func update<Value: Equatable>(
			_ property: ReferenceWritableKeyPath<RemoteCoordinator, Value>, to value: Value
		) {
			if self[keyPath: property] != value {
				self[keyPath: property] = value
			}
		}

		func status(serviceID: String) -> RemoteGatewayStatus? {
			gateways.first { $0.id == serviceID }
		}

		/// The binding of a folder accessed through a gateway
		func binding(folderID: String) -> RemoteFolderBinding? {
			bindings[folderID]
		}

		/// The local folder bound to the folder of the gateway, if it is in Files
		func localFolderID(serviceID: String, remoteFolderID: String) -> String? {
			bindings.values.first { $0.serviceID == serviceID && $0.remoteFolderID == remoteFolderID }?.folderID
		}

		/// The Syncthing folder of this device with the ID, if there is one
		func syncthingFolder(id: String) -> SushitrainFolder? {
			guard let folder = client?.folder(withID: id), folder.exists() else {
				return nil
			}
			return folder
		}

		func isRemoteManaged(folderID: String) -> Bool {
			remoteManagedFolderIDs.contains(folderID) || bindings[folderID] != nil
		}

		/// A paired gateway that serves the Syncthing folder of this device (the same folder ID, shared with the
		/// gateway's Syncthing device), so that the folder can be switched to access through it
		func gatewayServing(folder: SushitrainFolder, serviceID: String? = nil)
			-> (status: RemoteGatewayStatus, folder: RemoteFolder)?
		{
			guard bindings[folder.folderID] == nil, !remoteManagedFolderIDs.contains(folder.folderID),
				folder.isInManagedBase(), folder.isRegularFolder, !folder.isPhotoFolder
			else {
				return nil
			}
			for status in gateways where serviceID == nil || status.id == serviceID {
				guard let deviceID = status.capabilities?.syncthingDeviceID, folder.isShared(withDeviceID: deviceID),
					let remote = status.folders?.first(where: { $0.id == folder.folderID })
				else {
					continue
				}
				return (status, remote)
			}
			return nil
		}

		func operations(folderID: String) -> [RemoteOperationSummary] {
			operations.filter { $0.folderID == folderID }
		}

		func dismissNotice() {
			notice = nil
		}

		// MARK: - Refreshing

		/// Checks every gateway and brings the metadata of their folders up to date. Concurrent calls wait for the same
		/// refresh.
		func refresh() async {
			await startRefresh().value
		}

		/// The refresh in progress, or a new one
		private func startRefresh() -> Task<Void, Never> {
			if let refreshTask {
				return refreshTask
			}
			let task = Task {
				await performRefresh()
				refreshTask = nil
			}
			refreshTask = task
			return task
		}

		private func performRefresh() async {
			guard let environment, isStarted else {
				return
			}
			isRefreshing = true
			defer {
				isRefreshing = false
				lastRefresh = Date()
			}
			await reloadState()
			let endpoints = gateways.map(\.endpoint)
			await withTaskGroup(of: RemoteWork.GatewayRefresh.self) { group in
				for endpoint in endpoints {
					group.addTask { await RemoteWork.refresh(endpoint, environment: environment) }
				}
				for await result in group {
					apply(result)
				}
			}
			await resumeMigrations()
			await reloadState()
			pokeOutbox()
		}

		/// Refreshes one gateway now (after pairing, or when the user asks)
		func refresh(serviceID: String) async {
			guard let environment, let endpoint = status(serviceID: serviceID)?.endpoint else {
				return
			}
			apply(await RemoteWork.refresh(endpoint, environment: environment))
			await reloadState()
			pokeOutbox()
		}

		private func apply(_ result: RemoteWork.GatewayRefresh) {
			guard let index = gateways.firstIndex(where: { $0.id == result.serviceID }) else {
				return
			}
			let previous = gateways[index]
			var status = previous
			status.endpoint = result.endpoint
			if result.connection != .unknown {
				status.connection = result.connection
				status.lastChecked = Date()
			}
			if let capabilities = result.capabilities {
				status.capabilities = capabilities
			}
			if let folders = result.folders {
				status.folders = folders
				status.folderProblems = result.folderProblems
			}
			if result.authorityChanged {
				status.authorityChangedAt = Date()
				Log.warn("Gateway \(result.serviceID) started a new authority generation")
			}
			if status != previous {
				gateways[index] = status
			}

			let files = FileProviderCoordinator.shared
			if !result.changedFolders.isEmpty || result.authorityChanged {
				files.signalWorkingSet()
			}
			if status.isUsable {
				signalReached(serviceID: result.serviceID, force: previous.connection != .reachable)
			}
		}

		/// Files may have been told the gateway was unreachable or did not accept this device, by this app or by the
		/// extension (which signals that resolved only once its own requests succeed, and only while it runs): it tries
		/// the affected items again. At most once a minute per gateway, unless forced (it just became usable, or was
		/// just paired).
		private func signalReached(serviceID: String, force: Bool) {
			let now = Date()
			if !force, let last = lastResolvedSignal[serviceID],
				now.timeIntervalSince(last) < Self.resolvedSignalInterval
			{
				return
			}
			lastResolvedSignal[serviceID] = now
			let files = FileProviderCoordinator.shared
			files.signalResolved(.serverUnreachable)
			files.signalResolved(.notAuthenticated)
			files.signalResolved(.cannotSynchronize)
			pokeOutbox()
		}

		/// Acknowledges that the user saw that a gateway may have lost state
		func acknowledgeAuthorityChange(serviceID: String) {
			if let index = gateways.firstIndex(where: { $0.id == serviceID }) {
				gateways[index].authorityChangedAt = nil
			}
		}

		// MARK: - Long polling

		/// Long-polls folders for changes, one at a time (a long poll occupies one of the metadata requests the app and
		/// the extension share), while the app is active
		func startLongPolling() {
			guard isStarted, pollTask == nil, let environment else {
				return
			}
			pollTask = Task { [weak self] in
				var turn = 0
				while !Task.isCancelled {
					guard let self else {
						return
					}
					if gateways.contains(where: { !$0.isUsable }),
						Date().timeIntervalSince(lastRefresh ?? .distantPast) > Self.recheckInterval
					{
						await refresh()
					}
					let targets = pollTargets()
					guard !targets.isEmpty else {
						try? await Task.sleep(for: .seconds(Self.recheckInterval))
						continue
					}
					let target = targets[turn % targets.count]
					turn &+= 1
					let others = targets.filter { $0 != target }
					let started = Date()
					let (result, changedOthers) = await RemoteWork.poll(
						target, others: others, environment: environment)
					if Task.isCancelled {
						return
					}
					await pollFinished(target, result: result, changedOthers: changedOthers)
					switch result {
					case .failure:
						try? await Task.sleep(for: .seconds(15))
					case .success where target.wait == nil:
						try? await Task.sleep(for: .seconds(Self.longPollLimit))
					case .success(let changed):
						// A gateway that answers right away without changes does not hold the request
						if !changed, Date().timeIntervalSince(started) < 1 {
							try? await Task.sleep(for: .seconds(5))
						}
					}
				}
			}
		}

		func stopLongPolling() {
			pollTask?.cancel()
			pollTask = nil
		}

		private func pollTargets() -> [RemoteWork.PollTarget] {
			let usable = Dictionary(uniqueKeysWithValues: gateways.filter(\.isUsable).map { ($0.id, $0) })
			return bindings.values.sorted { $0.folderID < $1.folderID }.compactMap { binding in
				guard binding.permissions.read, let serviceID = binding.serviceID, let status = usable[serviceID],
					let capabilities = status.capabilities
				else {
					return nil
				}
				let wait =
					capabilities.supports(.longPoll)
					? min(max(capabilities.limits.longPollSeconds, 1), Self.longPollLimit) : nil
				return RemoteWork.PollTarget(folderID: binding.folderID, endpoint: status.endpoint, wait: wait)
			}
		}

		private func pollFinished(
			_ target: RemoteWork.PollTarget, result: Result<Bool, RemoteError>, changedOthers: [String]
		) async {
			switch result {
			case .success(let changed):
				if changed || !changedOthers.isEmpty {
					FileProviderCoordinator.shared.signalWorkingSet()
				}
				signalReached(serviceID: target.endpoint.serviceID, force: false)
				if migrations[target.folderID] != nil {
					await resumeMigrations()
					await reloadState()
				}
			case .failure(let error):
				guard error != .canceled, let index = gateways.firstIndex(where: { $0.id == target.endpoint.serviceID })
				else {
					return
				}
				switch error {
				case .unreachable, .unauthenticated:
					gateways[index].connection = RemoteGatewayStatus.connection(for: error)
					gateways[index].lastChecked = Date()
				default:
					gateways[index].folderProblems[target.folderID] = error.userDescription
					// Permissions may have changed; a refresh of the gateway finds out
					await refresh(serviceID: target.endpoint.serviceID)
				}
			}
		}

		// MARK: - Outbox

		/// Wakes the outbox to look for work now
		func pokeOutbox() {
			outboxPoked = true
			outboxSleep?.cancel()
		}

		private func startOutbox() {
			guard outboxTask == nil, let environment else {
				return
			}
			outboxTask = Task { [weak self] in
				while !Task.isCancelled {
					guard let self else {
						return
					}
					outboxPoked = false
					// Nothing is sent to gateways that speak another version of the API
					let incompatible = Set(gateways.filter { $0.isCompatible == false }.map(\.id))
					let pass = await runOutboxPass(environment, incompatible: incompatible)
					await afterOutboxPass(pass)
					if outboxPoked {
						continue
					}
					// Operations that wait for others (or for the extension's claims) are not claimable: no busy loop
					let shortest: TimeInterval = pass.claimed > 0 ? 1 : 15
					let delay = pass.nextAttempt.map {
						Duration.seconds(min(max($0.timeIntervalSinceNow, shortest), 60))
					}
					let sleeper = Task {
						_ = try? await Task.sleep(for: delay ?? Self.idleOutboxInterval)
					}
					outboxSleep = sleeper
					await sleeper.value
					outboxSleep = nil
				}
			}
		}

		/// One pass over the outbox, with background time to finish it when the app leaves the foreground. When that time
		/// runs short (or a background run ends, see `prepareForSuspension`), the rest of a long upload goes to the app's
		/// background session, which sends it while the app is suspended.
		private func runOutboxPass(_ environment: RemoteEnvironment, incompatible: Set<String>) async
			-> RemoteWork.OutboxPass
		{
			let handoff = appTransfers.map { BackgroundHandoff(transfers: $0) }
			outboxHandoff = handoff
			defer {
				if outboxHandoff === handoff {
					outboxHandoff = nil
				}
			}
			let work = Task.detached(priority: .utility) {
				await RemoteWork.runOutbox(environment: environment, incompatible: incompatible, handoff: handoff)
			}
			let activity = BackgroundActivity(name: "Sending edits to gateways") {
				// The watch below hands over earlier; this is the last chance
				handoff?.request()
				work.cancel()
			}
			defer { activity.end() }
			let watch = Task { @MainActor in
				while !Task.isCancelled {
					if Self.isRunningOutOfTime {
						Log.info("Little background time is left: handing uploads to the background session")
						handoff?.request()
						return
					}
					try? await Task.sleep(for: .seconds(1))
				}
			}
			defer { watch.cancel() }
			let pass = await work.value
			if pass.handedOver > 0 {
				Log.info("Handed \(pass.handedOver) uploads to the background session")
			}
			return pass
		}

		/// Whether the app runs in the background with less time left than handing an upload over needs
		private static var isRunningOutOfTime: Bool {
			UIApplication.shared.applicationState == .background
				&& UIApplication.shared.backgroundTimeRemaining < handoffReserve
		}

		/// Before the app loses its execution time (a background run ends): the rest of the upload in progress goes to the
		/// app's background session, and nothing more is started. Waits for the handoff, at most `limit`.
		func prepareForSuspension(within limit: Duration = .seconds(3)) async {
			guard let handoff = outboxHandoff else {
				return
			}
			handoff.request()
			let clock = ContinuousClock()
			let deadline = clock.now.advanced(by: limit)
			while outboxHandoff === handoff, clock.now < deadline {
				try? await Task.sleep(for: .milliseconds(100))
			}
		}

		private func afterOutboxPass(_ pass: RemoteWork.OutboxPass, signal: Bool = true) async {
			for (serviceID, error) in pass.gatewayErrors {
				guard let index = gateways.firstIndex(where: { $0.id == serviceID }) else {
					continue
				}
				switch error {
				case .unauthenticated, .unreachable:
					gateways[index].connection = RemoteGatewayStatus.connection(for: error)
					gateways[index].lastChecked = Date()
				default:
					break
				}
			}
			if pass.resolved > 0, signal {
				FileProviderCoordinator.shared.signalWorkingSet()
				FileProviderCoordinator.shared.signalResolved(.cannotSynchronize)
			}
			if pass.claimed > 0 {
				await reloadState()
			}
		}

		// MARK: - Background transfers

		/// The background transfers of the app's session, or of the extension's (recreated only to finish its events)
		private func backgroundTransfers(for identifier: String) -> BackgroundTransfers? {
			guard let environment else {
				return nil
			}
			if identifier == environment.appSessionIdentifier, let appTransfers {
				return appTransfers
			}
			if identifier == environment.extensionSessionIdentifier, let extensionTransfers {
				return extensionTransfers
			}
			guard identifier == environment.appSessionIdentifier || identifier == environment.extensionSessionIdentifier
			else {
				return nil
			}
			let transfers = BackgroundTransfers(
				identifier: identifier, appGroupIdentifier: environment.appGroupIdentifier, store: environment.store,
				storage: environment.storage, credentials: environment.credentials
			) { record in
				if record.kind == .upload, let operationID = record.operationID {
					// Remembered right away, for the end of the session's events
					_ = RemoteCoordinator.finishedUploads.withLock { $0.insert(operationID) }
				}
				Task { @MainActor in RemoteCoordinator.shared.transferFinished(record) }
			}
			if identifier == environment.appSessionIdentifier {
				appTransfers = transfers
			}
			else {
				extensionTransfers = transfers
			}
			return transfers
		}

		/// For `application(_:handleEventsForBackgroundURLSession:completionHandler:)`: the app's session, or the
		/// extension's (events of the extension's tasks reach the app when the extension does not run). Once the events
		/// are delivered, the operations whose uploads ended are submitted and the downloads verified, Files is told what
		/// was resolved, and then the completion is called, exactly once. Returns false for sessions that are not about
		/// remote folders.
		func handleBackgroundEvents(identifier: String, completion: @escaping @MainActor @Sendable () -> Void) -> Bool {
			guard let environment, let transfers = backgroundTransfers(for: identifier) else {
				return false
			}
			Log.info("Finishing the events of background session \(identifier)")
			let isExtensionSession = identifier == environment.extensionSessionIdentifier
			transfers.handleEvents {
				Task { @MainActor in
					await RemoteCoordinator.shared.finishBackgroundEvents(of: transfers)
					completion()
					if isExtensionSession {
						// Only the extension uses its session otherwise: let go of it once its tasks are done (never
						// cancel them)
						transfers.disconnect()
						if RemoteCoordinator.shared.extensionTransfers === transfers {
							RemoteCoordinator.shared.extensionTransfers = nil
						}
					}
				}
			}
			return true
		}

		/// Goes on with what a session's tasks finished, within the time the system gives the app for its events
		private func finishBackgroundEvents(of transfers: BackgroundTransfers) async {
			guard let environment else {
				return
			}
			let operationIDs = Self.finishedUploads.withLock { finished in
				defer { finished = [] }
				return finished
			}
			let incompatible = Set(gateways.filter { $0.isCompatible == false }.map(\.id))
			// In the background, the app has only the events' time: what a task did not send goes to a new one
			let handoff = backgroundTransfers(for: environment.appSessionIdentifier).map { BackgroundHandoff(transfers: $0) }
			if UIApplication.shared.applicationState == .background {
				handoff?.request()
			}
			let work = Task.detached(priority: .utility) {
				// Tasks of uploads that newer edits replaced, or whose operations were resolved, stop
				await transfers.cancelObsoleteUploads()
				return await RemoteWork.finishBackgroundTransfers(
					operationIDs, environment: environment, incompatible: incompatible, handoff: handoff)
			}
			let activity = BackgroundActivity(name: "Finishing background transfers") {
				work.cancel()
			}
			defer { activity.end() }
			let timeout = Task {
				try? await Task.sleep(for: Self.eventsBudget)
				work.cancel()
			}
			defer { timeout.cancel() }
			let pass = await work.value
			if pass.resolved > 0 {
				// Awaited, since the app may be suspended as soon as the events are handled
				await Self.signalResolvedChanges()
			}
			await afterOutboxPass(pass, signal: false)
		}

		/// Tells Files that changes it was told are waiting were resolved, so that it asks for them again
		private static func signalResolvedChanges() async {
			guard let manager = NSFileProviderManager(for: FileProviderCoordinator.domain) else {
				return
			}
			do {
				try await manager.signalEnumerator(for: .workingSet)
				try await manager.signalErrorResolved(NSFileProviderError(.cannotSynchronize))
			}
			catch {
				Log.warn("Could not tell the File Provider about resolved changes: \(error.localizedDescription)")
			}
		}

		/// At start: reconnects to the app's session (transfers whose task it no longer has are interrupted, and their
		/// operations go on), and removes what finished transfers left
		private func reconcileTransfers() async {
			guard let environment, let appTransfers else {
				return
			}
			do {
				let lost = try await appTransfers.reconcile()
				if !lost.isEmpty {
					Log.info("\(lost.count) background transfers were interrupted while the app did not run")
				}
			}
			catch {
				Log.warn("Could not reconcile background transfers: \(error.userDescription)")
			}
			await Task.detached(priority: .utility) {
				let outbox = environment.outbox()
				outbox.removeIdleTransfers()
				outbox.removeReleasedTokens()
			}.value
		}

		private func transferFinished(_ record: TransferRecord) {
			guard let environment else {
				return
			}
			switch record.kind {
			case .upload:
				// The operation continues from the offset the gateway confirms (the delegate expedited it)
				pokeOutbox()
			case .download:
				Task.detached(priority: .utility) { RemoteWork.finishDownload(record, environment: environment) }
			case .thumbnail:
				break
			}
		}

		// MARK: - Switching folders

		private func switcher() -> RemoteFolderSwitcher? {
			guard let environment, let client else {
				return nil
			}
			return RemoteFolderSwitcher(environment: environment, client: client)
		}

		/// Continues switches that were interrupted, except the ones the user is running now (and the ones being
		/// continued already: refreshes and polls call this as well)
		private func resumeMigrations() async {
			guard let switcher = switcher() else {
				return
			}
			for migration in migrations.values
			where !switchingFolders.contains(migration.folderID) && !resumingFolders.contains(migration.folderID) {
				resumingFolders.insert(migration.folderID)
				defer { resumingFolders.remove(migration.folderID) }
				do {
					try await Task.detached(priority: .utility) { try await switcher.resume(migration) }.value
					folderAccessChanged(migration.folderID)
				}
				catch let error as RemoteSwitchError {
					if case .remote = error {
						// The gateway is not reachable: the switch continues with a later refresh
						continue
					}
					Log.warn("Could not continue switching folder \(migration.folderID): \(error.localizedDescription)")
					notice = error.localizedDescription
					folderAccessChanged(migration.folderID)
				}
				catch {
					Log.warn("Could not continue switching folder \(migration.folderID): \(error.localizedDescription)")
				}
			}
		}

		/// Runs a switch the user asked for (`key` names it while it runs), then updates what Files and the settings show
		/// for the folder whose access it changes
		private func runSwitch<T: Sendable>(
			key: String, affecting folderID: String?,
			_ body: @escaping @Sendable (RemoteFolderSwitcher) async throws(RemoteSwitchError) -> T
		) async throws(RemoteSwitchError) -> T {
			guard let switcher = switcher() else {
				throw .notEligible(String(localized: "Synctrain has not started yet."))
			}
			guard !switchingFolders.contains(key), !resumingFolders.contains(key) else {
				throw .notEligible(String(localized: "The folder is being switched already."))
			}
			switchingFolders.insert(key)
			let activity = BackgroundActivity(name: "Switching a folder")
			defer {
				activity.end()
				switchingFolders.remove(key)
			}
			let result = await Task.detached(priority: .userInitiated) {
				() async -> Result<T, RemoteSwitchError> in
				do throws(RemoteSwitchError) {
					return .success(try await body(switcher))
				}
				catch {
					return .failure(error)
				}
			}.value
			if let folderID {
				folderAccessChanged(folderID)
			}
			else {
				FileProviderCoordinator.shared.signalWorkingSet()
			}
			await reloadState()
			pokeOutbox()
			return try result.get()
		}

		private func folderAccessChanged(_ folderID: String) {
			FileProviderCoordinator.shared.folderAccessChanged(folderID: folderID)
		}

		/// Switches a Syncthing folder of this device to access through the gateway that serves it
		func switchToGateway(folderID: String, serviceID: String) async throws(RemoteSwitchError) {
			guard let status = status(serviceID: serviceID), let capabilities = status.capabilities,
				let remote = status.folders?.first(where: { $0.id == folderID })
			else {
				throw .notEligible(String(localized: "The gateway does not serve this folder."))
			}
			guard status.isUsable else {
				throw .gatewayUnavailable(status.problem ?? status.summary)
			}
			let endpoint = status.endpoint
			try await runSwitch(key: folderID, affecting: folderID) {
				(switcher: RemoteFolderSwitcher) async throws(RemoteSwitchError) -> Void in
				try await switcher.switchToGateway(
					folderID: folderID, endpoint: endpoint, capabilities: capabilities, remoteFolder: remote)
			}
		}

		/// Adds a folder of the gateway that this device does not synchronize to the Files app. Returns its local ID.
		@discardableResult
		func addGatewayFolder(_ remote: RemoteFolder, serviceID: String) async throws(RemoteSwitchError) -> String {
			guard let status = status(serviceID: serviceID) else {
				throw .notEligible(String(localized: "The gateway is not paired."))
			}
			let endpoint = status.endpoint
			return try await runSwitch(key: "\(serviceID)/\(remote.id)", affecting: nil) {
				(switcher: RemoteFolderSwitcher) async throws(RemoteSwitchError) -> String in
				try await switcher.addGatewayFolder(remote, endpoint: endpoint)
			}
		}

		/// Ends access through the gateway. Returns whether a Syncthing folder of this device takes over (it stays
		/// paused until the user resumes it).
		@discardableResult
		func switchToSyncthing(folderID: String) async throws(RemoteSwitchError) -> Bool {
			try await runSwitch(key: folderID, affecting: folderID) {
				(switcher: RemoteFolderSwitcher) throws(RemoteSwitchError) -> Bool in
				try switcher.switchToSyncthing(folderID: folderID)
			}
		}

		// MARK: - Pairing and forgetting

		/// After pairing: shows the gateway and lists its folders. Files may have been told that the gateway did not
		/// accept this device (before pairing again), so it tries those items again.
		func didPair(serviceID: String) async {
			await reloadState()
			let signaled = lastResolvedSignal[serviceID]
			await refresh(serviceID: serviceID)
			if lastResolvedSignal[serviceID] == signaled {
				// Not signaled by the refresh (the gateway did not answer it): pairing itself shows it accepts this device
				signalReached(serviceID: serviceID, force: true)
			}
		}

		/// Forgets a paired gateway and its credential. Refused while folders are accessed through it (their edits and
		/// cached files are never deleted this way).
		func forgetGateway(serviceID: String) async throws(RemoteSwitchError) {
			guard let environment else {
				return
			}
			let result = await Task.detached(priority: .userInitiated) { () -> Result<Void, RemoteSwitchError> in
				do throws(IndexError) {
					try environment.store.removeRemoteEndpoint(serviceID: serviceID)
				}
				catch .notAllowed {
					return .failure(
						.notEligible(
							String(
								localized:
									"Folders are still accessed through this gateway. Switch them back to Syncthing or remove them from Files first."
							)))
				}
				catch {
					return .failure(.store(error))
				}
				try? environment.credentials.removeCredential(serviceID: serviceID)
				environment.forgetClient(serviceID: serviceID)
				return .success(())
			}.value
			try result.get()
			gateways.removeAll { $0.id == serviceID }
			await reloadState()
		}

		// MARK: - Reviewing operations

		/// Gives up an edit after the user reviewed it: its item shows the gateway's state again
		func discard(operationID: String) async throws(RemoteSwitchError) {
			guard let environment else {
				return
			}
			let result = await Task.detached(priority: .userInitiated) { () -> Result<Void, RemoteSwitchError> in
				do throws(IndexError) {
					try RemoteWork.discard(operationID: operationID, environment: environment)
					return .success(())
				}
				catch {
					return .failure(.store(error))
				}
			}.value
			try result.get()
			FileProviderCoordinator.shared.signalWorkingSet()
			await reloadState()
		}

		/// A copy of an edit's contents, named like the item, to export
		func exportCopy(of summary: RemoteOperationSummary) async throws -> URL {
			guard let environment else {
				throw CocoaError(.fileNoSuchFile)
			}
			return try await Task.detached(priority: .userInitiated) {
				try RemoteWork.exportCopy(of: summary, environment: environment)
			}.value
		}
	}
#endif
