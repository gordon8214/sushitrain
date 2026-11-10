// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Observation
import os
import SynctrainFileProviderKit

/// The Synctrain location in the Files app. It exists while a gateway is paired.
@Observable @MainActor final class FilesDomain {
	static let shared = FilesDomain()

	/// Whether the user enabled the Synctrain location in the Files app (nil while unknown, or without a location)
	private(set) var isEnabled: Bool? = nil
	private(set) var lastError: String? = nil
	private(set) var isRebuilding = false
	/// While a rebuild lists the folders again from their gateways
	private(set) var isRelisting = false
	/// Counts the changes of the index, so that views listing the index can follow them
	private(set) var indexRevision = 0

	@ObservationIgnored private var signalTask: Task<Void, Never>? = nil
	@ObservationIgnored private var observers: [any NSObjectProtocol] = []
	@ObservationIgnored private var indexObserver: SharedNotificationObserver? = nil

	private init() {
		// Only the app's own views need to know: the extension tells the system itself
		indexObserver = SharedNotificationObserver(.indexChanged) {
			Task { @MainActor in FilesDomain.shared.indexRevision &+= 1 }
		}
		let center = NotificationCenter.default
		observers.append(
			center.addObserver(forName: .fileProviderDomainDidChange, object: nil, queue: .main) { _ in
				Task { @MainActor in await FilesDomain.shared.refreshState() }
			})
		observers.append(
			ClientLifecycle.observe(.willResume) {
				Task { @MainActor in await FilesDomain.shared.refreshState() }
			})
	}

	nonisolated static var domain: NSFileProviderDomain {
		let domain = NSFileProviderDomain(
			identifier: NSFileProviderDomainIdentifier(rawValue: SharedContainer.domainIdentifier),
			displayName: "Synctrain")
		// Deleting through the Files app deletes on the gateway; Syncthing versioning there keeps old copies if enabled
		domain.supportsSyncingTrash = false
		return domain
	}

	private var manager: NSFileProviderManager? {
		ClientLifecycle.allowsSharedState ? NSFileProviderManager(for: Self.domain) : nil
	}

	/// Whether the system answered that the location does not exist: nothing to tell it about yet
	nonisolated private static func isDomainMissing(_ error: any Error) -> Bool {
		(error as? NSFileProviderError)?.code == .providerDomainNotFound
	}

	// MARK: - The location

	/// Adds the location, which is harmless when it exists already
	func register() async {
		guard ClientLifecycle.allowsSharedState else {
			return
		}
		do {
			try await NSFileProviderManager.add(Self.domain)
			lastError = nil
		}
		catch {
			Log.warn("Could not register the File Provider domain: \(error.localizedDescription)")
			lastError = error.localizedDescription
		}
		await refreshState()
	}

	/// Removes the location, and with it the system's copy of its files. On a Mac the system keeps changes it hasn't
	/// handed to the extension, and this returns where, if it kept any. It does not keep what the extension refused, which
	/// has to be taken out first (`RemoteCoordinator.takeFilesOut`). This also leaves a failure in `lastError`. A location
	/// that doesn't exist counts as removed.
	@discardableResult func remove() async throws -> URL? {
		guard ClientLifecycle.allowsSharedState else {
			return nil
		}
		do {
			let kept = try await removeFromSystem()
			lastError = nil
			await refreshState()
			return kept
		}
		catch {
			if Self.isDomainMissing(error) {
				await refreshState()
				return nil
			}
			Log.warn("Could not remove the File Provider domain: \(error.localizedDescription)")
			lastError = error.localizedDescription
			await refreshState()
			throw error
		}
	}

	/// The system names a place also when it had nothing to keep, which isn't there then. One it can't read may well hold
	/// something.
	private func removeFromSystem() async throws -> URL? {
		#if os(macOS)
			return try await NSFileProviderManager.remove(Self.domain, mode: .preserveDirtyUserData).flatMap { url in
				Log.info("The system kept what the Synctrain location held in \(url.path(percentEncoded: false))")
				return SystemCopy.holdsNothing(url) ? nil : url
			}
		#else
			try await NSFileProviderManager.remove(Self.domain)
			return nil
		#endif
	}

	func refreshState() async {
		do {
			let domains = try await NSFileProviderManager.domains()
			isEnabled = domains.first { $0.identifier.rawValue == SharedContainer.domainIdentifier }?.userEnabled
		}
		catch {
			Log.warn("Could not query File Provider domains: \(error.localizedDescription)")
		}
	}

	/// Removes the location (and the system's copy of its files) and sets it up again. Files on the gateways are not
	/// affected. Changes the system has not handed to the extension yet are lost on iOS (see `pendingItemCount`), and
	/// kept by the system on a Mac. The user has to enable the location again in Files.
	func rebuild() async {
		guard !isRebuilding, let store = RemoteEnvironment.shared?.store else {
			return
		}
		// The index keeps what folders on gateways need, which it cannot while their edits are unresolved; check
		// before the system's copy is removed
		let hasUnresolvedEdits = await Task.detached(priority: .userInitiated) {
			(try? store.hasUnresolvedMutations()) ?? true
		}.value
		if hasUnresolvedEdits {
			lastError = Self.unresolvedEditsMessage
			return
		}
		isRebuilding = true
		let activity = BackgroundActivity(name: "Rebuilding the Files location")
		defer {
			activity.end()
			isRebuilding = false
		}
		Log.info("Rebuilding the File Provider domain")
		#if os(macOS)
			do throws(GatewayFolderError) {
				let coordinator = RemoteCoordinator.shared
				for folderID in coordinator.bindings.keys.sorted() {
					try await coordinator.takeFilesOut(folderID: folderID)
				}
			}
			catch {
				lastError = error.localizedDescription
				return
			}
		#endif
		do {
			let kept = try await removeFromSystem()
			#if os(macOS)
				if let kept {
					RemoteCoordinator.shared.leftovers.append(
						FilesLeftovers.Report(folder: "Synctrain", url: kept, kind: .keptBySystem))
				}
			#endif
		}
		catch {
			Log.warn("Could not remove the File Provider domain: \(error.localizedDescription)")
			lastError = error.localizedDescription
			await refreshState()
			return
		}
		do {
			try await Task.detached(priority: .userInitiated) { try store.reset() }.value
			lastError = nil
		}
		catch IndexError.notAllowed {
			Log.warn("Could not rebuild the Files index: edits are unresolved")
			lastError = Self.unresolvedEditsMessage
		}
		catch {
			Log.warn("Could not rebuild the Files index: \(error.localizedDescription)")
			lastError = error.localizedDescription
		}
		#if os(iOS)
			// iOS reads each directory's type (folder or package) only now, when it learns of the items again: list
			// the folders again first, for the package marks gateways on Macs give today
			isRelisting = true
			var relisted = true
			if let environment = RemoteEnvironment.shared {
				relisted = await RemoteWork.relistForRebuild(environment: environment, deadline: Self.relistDeadline)
			}
			isRelisting = false
		#endif
		// Whatever happened to the index, the location has to come back
		await register()
		#if os(iOS)
			// After registering, which clears the last error when it succeeds
			if !relisted, lastError == nil {
				Log.warn("Not every folder was listed again before the Files location came back")
				lastError = String(
					localized:
						"Some gateways did not answer in time, so packages in their folders may show as folders. Rebuild again when they can be reached."
				)
			}
		#endif
	}

	/// How long a rebuild waits for the gateways to list the folders again
	static let relistDeadline: Duration = .seconds(60)

	static var unresolvedEditsMessage: String {
		String(
			localized:
				"The Synctrain location cannot be rebuilt while edits of folders on gateways are being sent or wait for review. Wait until they are sent, or export or discard them, then try again."
		)
	}

	/// How many items have changes in the system's copy that the extension has not accepted yet, or nil if the system
	/// could not say, which callers must not take for none. Only the first page is counted.
	func pendingItemCount() async -> Int? {
		guard let manager else {
			return nil
		}
		let enumerator = manager.enumeratorForPendingItems()
		defer { enumerator.invalidate() }
		return await withCheckedContinuation { continuation in
			let counter = PendingItemCounter { continuation.resume(returning: $0) }
			enumerator.enumerateItems(
				for: counter, startingAt: NSFileProviderPage(NSFileProviderPage.initialPageSortedByName as Data))
		}
	}

	/// Where the system keeps the location's files, if it has it
	func locationURL() async -> URL? {
		guard let manager else {
			return nil
		}
		do {
			return try await manager.getUserVisibleURL(for: .rootContainer)
		}
		catch {
			Log.warn("Could not find the Synctrain location: \(error.localizedDescription)")
			return nil
		}
	}

	/// Where an item of the index appears in the Files app, if it does
	func userVisibleURL(folderID: String, path: String) async -> URL? {
		guard let store = RemoteEnvironment.shared?.store, let manager else {
			return nil
		}
		do {
			guard let item = try store.item(folderID: folderID, path: path) else {
				return nil
			}
			return try await manager.getUserVisibleURL(
				for: NSFileProviderItemIdentifier(item.fileProviderIdentifier ?? ItemIdentifiers.string(for: item.id)))
		}
		catch {
			Log.warn("Could not find \(folderID):\(path) in the Files app: \(error.localizedDescription)")
			return nil
		}
	}

	// MARK: - Signals

	/// Tells the system that errors with the code were resolved, so that it retries what failed with them
	func signalResolved(_ code: NSFileProviderError.Code) {
		manager?.signalErrorResolved(NSFileProviderError(code)) { error in
			if let error, !Self.isDomainMissing(error) {
				Log.warn("Could not signal the File Provider that an error was resolved: \(error.localizedDescription)")
			}
		}
	}

	/// Tells the system to fetch changes from the extension, after a short delay to coalesce updates
	func signalWorkingSet() {
		signalTask?.cancel()
		signalTask = Task {
			try? await Task.sleep(for: .milliseconds(500))
			if Task.isCancelled {
				return
			}
			indexRevision &+= 1
			do {
				try await manager?.signalEnumerator(for: .workingSet)
			}
			catch {
				if !Self.isDomainMissing(error) {
					Log.warn("Could not signal the File Provider: \(error.localizedDescription)")
				}
			}
		}
	}
}

/// Counts the items of the first page of an enumeration
private final class PendingItemCounter: NSObject, NSFileProviderEnumerationObserver, @unchecked Sendable {
	private let lock = OSAllocatedUnfairLock(initialState: (count: 0, finished: false))
	private let completion: @Sendable (Int?) -> Void

	init(completion: @escaping @Sendable (Int?) -> Void) {
		self.completion = completion
	}

	func didEnumerate(_ updatedItems: [any NSFileProviderItemProtocol]) {
		lock.withLock { $0.count += updatedItems.count }
	}

	func finishEnumerating(upTo nextPage: NSFileProviderPage?) {
		finish(failed: false)
	}

	func finishEnumeratingWithError(_ error: any Error) {
		finish(failed: true)
	}

	private func finish(failed: Bool) {
		let count: Int?? = lock.withLock { state in
			guard !state.finished else {
				return nil
			}
			state.finished = true
			return .some(failed ? nil : state.count)
		}
		if let count {
			completion(count)
		}
	}
}
