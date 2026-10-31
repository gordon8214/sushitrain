// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import os
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
	/// The folders on this device, by label
	private(set) var folders: [IndexFolder] = []
	/// Edits being sent or needing review, newest first
	private(set) var operations: [RemoteOperationSummary] = []
	/// Whether some edits are not resolved with their gateways (which rules out rebuilding the Files location)
	private(set) var hasUnresolvedOperations = false
	private(set) var isRefreshing = false
	#if os(macOS)
		/// What the Synctrain location kept on this Mac when a folder left it or came back, oldest first, for the
		/// settings to tell the user once (`filesLeftoversAlert`, which takes each one as it is shown)
		var leftovers: [FilesLeftovers.Report] = []
	#endif

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

	private static let tombstonePruneInterval: TimeInterval = 24 * 3600
	private static let tombstoneRetention: TimeInterval = 30 * 24 * 3600
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
	#if os(iOS)
		/// The operations whose background upload task ended, as the session delegates reported them
		private nonisolated static let finishedUploads = OSAllocatedUnfairLock<Set<String>>(initialState: [])
	#endif

	var environment: RemoteEnvironment? { RemoteEnvironment.shared }

	// MARK: - Starting

	/// Starts the work with gateways
	func start() {
		guard !isStarted, let environment else {
			return
		}
		isStarted = true
		#if os(iOS)
			_ = backgroundTransfers(for: environment.appSessionIdentifier)
		#endif

		if observers.isEmpty {
			observers.append(ClientLifecycle.observe(.willResume) { RemoteCoordinator.shared.didEnterForeground() })
			observers.append(ClientLifecycle.observe(.didBecomeActive) { RemoteCoordinator.shared.startLongPolling() })
			observers.append(ClientLifecycle.observe(.didStop) { RemoteCoordinator.shared.stopLongPolling() })
		}
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
			await pruneTombstones()
			await reconcileTransfers()
			startOutbox()
			await refresh()
			if ClientLifecycle.isActive {
				startLongPolling()
			}
		}
	}

	/// Stops the work with gateways, which `start` starts again (on macOS, where it is a setting)
	func stop() {
		isStarted = false
		stopLongPolling()
		outboxTask?.cancel()
		outboxTask = nil
		outboxSleep?.cancel()
		workObserver = nil
	}

	/// Items that are gone stay in the index for a while, for what Files has not asked about yet
	private func pruneTombstones() async {
		let key = "lastTombstonePrune"
		let last = UserDefaults.standard.double(forKey: key)
		guard let environment, Date().timeIntervalSince1970 - last > Self.tombstonePruneInterval else {
			return
		}
		let store = environment.store
		let cutoff = Date().addingTimeInterval(-Self.tombstoneRetention)
		let pruned = await Task.detached(priority: .utility) { (try? store.pruneTombstones(before: cutoff)) != nil }.value
		if pruned {
			UserDefaults.standard.set(Date().timeIntervalSince1970, forKey: key)
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
		let remaining = ClientLifecycle.backgroundTimeRemaining
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

	// MARK: - State

	/// Reads bindings, gateways and operations from the index again
	func reloadState() async {
		guard let environment else {
			return
		}
		let state: RemoteWork.State
		do {
			state = try await Task.detached(priority: .utility) {
				try RemoteWork.loadState(environment: environment)
			}.value
		}
		catch {
			Log.warn("Could not read the state of remote folders: \(error)")
			return
		}
		// Only what changed, so that views showing the rest are not updated
		update(\.bindings, to: state.bindings)
		update(\.folders, to: state.folders)
		update(\.operations, to: state.operations)
		update(\.hasUnresolvedOperations, to: state.hasUnresolvedOperations)
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

	func operations(folderID: String) -> [RemoteOperationSummary] {
		operations.filter { $0.folderID == folderID }
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

		let files = FilesDomain.shared
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
		let files = FilesDomain.shared
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
			guard binding.permissions.read, let status = usable[binding.serviceID],
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
				FilesDomain.shared.signalWorkingSet()
			}
			signalReached(serviceID: target.endpoint.serviceID, force: false)
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
		guard isStarted, outboxTask == nil, let environment else {
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
				if Task.isCancelled {
					return
				}
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
		ClientLifecycle.isInBackground && ClientLifecycle.backgroundTimeRemaining < handoffReserve
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
			FilesDomain.shared.signalWorkingSet()
			FilesDomain.shared.signalResolved(.cannotSynchronize)
		}
		if pass.claimed > 0 {
			await reloadState()
		}
	}

	#if os(iOS)
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
			if ClientLifecycle.isInBackground {
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
			guard let manager = NSFileProviderManager(for: FilesDomain.domain) else {
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
	#endif

	/// At start: reconnects to the app's session (transfers whose task it no longer has are interrupted, and their
	/// operations go on), and removes what finished transfers left
	private func reconcileTransfers() async {
		guard let environment else {
			return
		}
		if let appTransfers {
			do {
				let lost = try await appTransfers.reconcile()
				if !lost.isEmpty {
					Log.info("\(lost.count) background transfers were interrupted while the app did not run")
				}
			}
			catch {
				Log.warn("Could not reconcile background transfers: \(error.userDescription)")
			}
		}
		await Task.detached(priority: .utility) {
			let outbox = environment.outbox()
			outbox.removeIdleTransfers()
			outbox.removeReleasedTokens()
		}.value
	}

	#if os(iOS)
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
	#endif

	// MARK: - Folders

	/// Adds a folder of the gateway to this device, and returns its local ID. A folder whose first listing fails is
	/// added all the same: its items follow with the next refresh.
	@discardableResult
	func addFolder(_ remote: RemoteFolder, serviceID: String) async throws(GatewayFolderError) -> String {
		guard let environment, let status = status(serviceID: serviceID) else {
			throw .notEligible(String(localized: "The gateway is not paired."))
		}
		guard remote.permissions.read else {
			throw .notEligible(String(localized: "The gateway does not let this device read the folder."))
		}
		let endpoint = status.endpoint
		let activity = BackgroundActivity(name: "Adding a folder")
		defer { activity.end() }
		#if os(macOS)
			// Before the folder is added, as its own root takes the name a directory left by an earlier round holds.
			// Reported right away: it has happened whether or not the folder is added after it.
			if localFolderID(serviceID: serviceID, remoteFolderID: remote.id) == nil {
				let label = remote.label.isEmpty ? remote.id : remote.label
				switch await moveLeftoversAside(label: label, environment: environment) {
				case .moved(let report):
					leftovers.append(report)
				case .failed:
					throw .notEligible(
						String(
							localized:
								"Finder's Synctrain location still holds files of \(label) that Synctrain cannot synchronize, and could not move them aside. Move or remove that folder in Finder, then add it again."
						))
				case .nothingInTheWay:
					break
				}
			}
		#endif
		let result = await Task.detached(priority: .userInitiated) {
			await RemoteWork.addFolder(remote, endpoint: endpoint, environment: environment)
		}.value
		FilesDomain.shared.signalWorkingSet()
		await reloadState()
		pokeOutbox()
		return try result.get()
	}

	/// Removes a folder from this device; it stays on its gateway. Refused while its edits are unresolved. On macOS,
	/// what the location cannot synchronize is taken out of it first, into `keeping` (the folder's own directory, for a
	/// folder of this Mac) or else a new directory of this Mac.
	func removeFolder(folderID: String, keeping destination: URL? = nil) async throws(GatewayFolderError) {
		guard let environment else {
			return
		}
		let activity = BackgroundActivity(name: "Removing a folder")
		defer { activity.end() }
		#if os(macOS)
			// While the folder is still in the index: its items are what tells what the location cannot synchronize
			let report = await takeFilesOutOfTheLocation(
				folderID: folderID, environment: environment, keeping: destination)
		#endif
		let result = await Task.detached(priority: .userInitiated) {
			RemoteWork.removeFolder(folderID: folderID, environment: environment)
		}.value
		FilesDomain.shared.signalWorkingSet()
		await reloadState()
		try result.get()
		#if os(macOS)
			if let report {
				leftovers.append(report)
			}
		#endif
	}

	#if os(macOS)
		/// What the location cannot synchronize in the folder, taken out of it before it leaves Finder: macOS removes
		/// its copy of a folder with everything in it, these files included, and they are nowhere else. They go to the
		/// folder's own directory where it has one, and otherwise to a new directory of this Mac.
		private func takeFilesOutOfTheLocation(
			folderID: String, environment: RemoteEnvironment, keeping destination: URL?
		) async -> FilesLeftovers.Report? {
			let folder = try? environment.store.folder(id: folderID)
			guard let label = folder?.label,
				let root = await FilesDomain.shared.userVisibleURL(folderID: folderID, path: "")
			else {
				Log.warn("Could not look at what the Synctrain location keeps of \(folderID)")
				return nil
			}
			let store = environment.store
			let found = await Task.detached(priority: .userInitiated) { () -> SystemCopy.Unknown? in
				do {
					return try SystemCopy.unknownPaths(
						folderID: folderID, root: root, store: store, limit: FilesLeftovers.limit)
				}
				catch {
					Log.warn("Could not read the index to see what the Synctrain location keeps: \(error)")
					return nil
				}
			}.value
			guard let found, let example = found.paths.first else {
				return nil
			}
			Log.info(
				"The Synctrain location cannot synchronize \(found.paths.count) items of \(folderID)"
					+ (found.isComplete ? "" : " (at least)"))
			let place = destination ?? ClientLifecycle.directoryForFilesOutsideTheLocation(named: label)
			let paths = found.paths
			let moved: Int
			if let place {
				moved = await Task.detached(priority: .userInitiated) {
					FilesLeftovers.moveOut(paths, from: root, to: place)
				}.value
			}
			else {
				moved = 0
			}
			guard moved == paths.count, let place else {
				// What stays is the system's to keep or to remove with the folder
				return FilesLeftovers.Report(
					folder: label, url: root,
					kind: .staying(count: paths.count, example: example, isComplete: found.isComplete))
			}
			return FilesLeftovers.Report(
				folder: label, url: place,
				kind: .movedOut(
					count: moved, example: example, isComplete: found.isComplete,
					place: (place.path(percentEncoded: false) as NSString).abbreviatingWithTildeInPath))
		}

		/// Renames the directory an earlier round of a folder of this label left at the top level of the location, which
		/// would otherwise keep the folder from showing anything of its own
		private func moveLeftoversAside(label: String, environment: RemoteEnvironment) async
			-> FilesLeftovers.Aside<FilesLeftovers.Report>
		{
			guard let location = await FilesDomain.shared.locationURL() else {
				return .nothingInTheWay
			}
			let store = environment.store
			let aside = await Task.detached(priority: .userInitiated) {
				FilesLeftovers.moveAside(label: label, in: location, store: store)
			}.value
			switch aside {
			case .nothingInTheWay:
				return .nothingInTheWay
			case .failed:
				return .failed
			case .moved(let name):
				return .moved(
					FilesLeftovers.Report(
						folder: label, url: location.appending(path: name, directoryHint: .isDirectory),
						kind: .movedAside(name)))
			}
		}
	#endif

	// MARK: - Pairing and forgetting

	/// After pairing: shows the gateway and lists its folders. Files may have been told that the gateway did not
	/// accept this device (before pairing again), so it tries those items again.
	func didPair(serviceID: String) async {
		await FilesDomain.shared.register()
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
	func forgetGateway(serviceID: String) async throws(GatewayFolderError) {
		guard let environment else {
			return
		}
		let result = await Task.detached(priority: .userInitiated) { () -> Result<Void, GatewayFolderError> in
			do throws(IndexError) {
				try environment.store.removeRemoteEndpoint(serviceID: serviceID)
			}
			catch .notAllowed {
				return .failure(
					.notEligible(
						String(
							localized:
								"Folders are still accessed through this gateway. Remove them first."
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
		if gateways.isEmpty {
			await FilesDomain.shared.remove()
		}
	}

	// MARK: - Reviewing operations

	/// Gives up an edit after the user reviewed it: its item shows the gateway's state again
	func discard(operationID: String) async throws(GatewayFolderError) {
		guard let environment else {
			return
		}
		let result = await Task.detached(priority: .userInitiated) { () -> Result<Void, GatewayFolderError> in
			do throws(IndexError) {
				try RemoteWork.discard(operationID: operationID, environment: environment)
				return .success(())
			}
			catch {
				return .failure(.store(error))
			}
		}.value
		try result.get()
		FilesDomain.shared.signalWorkingSet()
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
