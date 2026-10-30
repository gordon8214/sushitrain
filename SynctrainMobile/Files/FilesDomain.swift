// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Observation
import UIKit
import os
import SynctrainFileProviderKit

/// The Synctrain location in the Files app. It exists while a gateway is paired.
@Observable @MainActor final class FilesDomain {
	static let shared = FilesDomain()

	/// Whether the user enabled the Synctrain location in the Files app (nil while unknown, or without a location)
	private(set) var isEnabled: Bool? = nil
	private(set) var lastError: String? = nil
	private(set) var isRebuilding = false
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
			center.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) { _ in
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
		NSFileProviderManager(for: Self.domain)
	}

	/// Whether the system answered that the location does not exist: nothing to tell it about yet
	nonisolated private static func isDomainMissing(_ error: any Error) -> Bool {
		(error as? NSFileProviderError)?.code == .providerDomainNotFound
	}

	// MARK: - The location

	/// Adds the location, which is harmless when it exists already
	func register() async {
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

	/// Removes the location, and with it the system's copy of its files. Only for when no gateway is left.
	func remove() async {
		do {
			try await NSFileProviderManager.remove(Self.domain)
			lastError = nil
		}
		catch {
			Log.warn("Could not remove the File Provider domain: \(error.localizedDescription)")
			lastError = error.localizedDescription
		}
		await refreshState()
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
	/// affected, but changes the system has not handed to the extension yet are lost (see `pendingItemCount`). The user
	/// has to enable the location again in Files.
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
		do {
			try await NSFileProviderManager.remove(Self.domain)
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
		// Whatever happened to the index, the location has to come back
		await register()
	}

	static var unresolvedEditsMessage: String {
		String(
			localized:
				"The Synctrain location cannot be rebuilt while edits of folders on gateways are being sent or wait for review. Wait until they are sent, or export or discard them, then try again."
		)
	}

	/// How many items have changes in the system's copy that the extension has not accepted yet. Only the first page is
	/// counted.
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

	/// Where an item of the index appears in the Files app, if it does
	func userVisibleURL(folderID: String, path: String) async -> URL? {
		guard let store = RemoteEnvironment.shared?.store, let manager else {
			return nil
		}
		do {
			guard let item = try store.item(folderID: folderID, path: path) else {
				return nil
			}
			return try await manager.getUserVisibleURL(for: NSFileProviderItemIdentifier(ItemIdentifiers.string(for: item.id)))
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
