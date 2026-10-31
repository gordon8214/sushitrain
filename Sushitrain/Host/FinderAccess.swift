// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainFileProviderKit
	@preconcurrency import SushitrainCore

	/// Files in Finder: a folder can be on demand instead of synchronized in full. It shows in Finder's Synctrain location
	/// with all its files, and only what is opened comes to this Mac. This needs no gateway elsewhere: the File Provider
	/// extension is sandboxed and has no engine, but the app runs the remote file service that it speaks to, so this Mac
	/// is its own gateway, over the loopback interface. Folders of other gateways work the same way.
	@Observable @MainActor final class FinderAccess {
		static let shared = FinderAccess()

		/// A folder of this Mac, as far as this setting goes
		struct Folder: Identifiable, Equatable, Sendable {
			let id: String
			let name: String
			let isOnDemand: Bool
			/// Why the folder can't be on demand
			let restriction: String?
		}

		enum Failure: LocalizedError {
			case unavailable
			case notRunning
			case notListed
			case foldersInFinder
			case exportedReadOnly

			var errorDescription: String? {
				switch self {
				case .unavailable:
					return String(localized: "Files in Finder is not available in this build of Synctrain.")
				case .notRunning:
					return String(localized: "Synctrain could not start the service that Finder uses.")
				case .notListed:
					return String(localized: "The service that Finder uses does not list this folder. Try again.")
				case .foldersInFinder:
					return String(
						localized:
							"Finder still shows folders. Switch them to Synchronize All, or remove them from their gateways, first: their files could not be opened otherwise."
					)
				case .exportedReadOnly:
					return String(
						localized:
							"Other devices may only read this folder. Allow changes to it under Remote file service first, which applies to the devices you gave write access."
					)
				}
			}
		}

		private(set) var isEnabled: Bool
		/// This Mac as a gateway, once it is paired with itself
		private(set) var serviceID: String? = nil
		private(set) var folders: [Folder] = []
		private(set) var lastError: String? = nil
		/// The folder being switched
		private(set) var switching: String? = nil
		/// Whether something is being changed: changes run one after the other
		private(set) var isWorking = false

		@ObservationIgnored private(set) var client: SushitrainClient? = nil
		@ObservationIgnored private var queue: Task<Void, Never>? = nil

		private static let enabledKey = "finderAccessEnabled"
		/// The folders exported for this Mac alone, as opposed to what the user exported to other devices
		private static let ownExportsKey = "finderAccessOwnExports"

		/// A test run keeps out of the real system: the location and the app group container are not redirected by its
		/// scratch directory, so it only uses them when it is told to
		private static var isAllowed: Bool {
			guard TestRun.isActive else {
				return true
			}
			#if DEBUG
				return ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_FILE_PROVIDER"] != nil
			#else
				return false
			#endif
		}

		private init() {
			isEnabled = Self.isAllowed && UserDefaults.standard.bool(forKey: Self.enabledKey)
		}

		/// Runs a change once the ones asked for before it have finished, as they build on each other's results
		private func inOrder<T: Sendable>(_ work: @escaping @MainActor () async throws -> T) async throws -> T {
			let previous = queue
			let task = Task { @MainActor in
				await previous?.value
				isWorking = true
				defer { isWorking = false }
				return try await work()
			}
			queue = Task { _ = await task.result }
			return try await task.value
		}

		/// Starts what the setting needs. Call once the client has started.
		func start(client: SushitrainClient) {
			self.client = client
			if isEnabled {
				retry()
			}
		}

		/// Sets up what the setting needs, which starting it may not have got through
		func retry() {
			Task { try? await inOrder { await self.activate() } }
		}

		func setEnabled(_ enabled: Bool) async {
			guard enabled != isEnabled, Self.isAllowed else {
				return
			}
			do {
				try await inOrder {
					if enabled {
						self.isEnabled = true
						UserDefaults.standard.set(true, forKey: Self.enabledKey)
						await self.activate()
					}
					else {
						try await self.deactivate()
					}
				}
			}
			catch {
				lastError = error.localizedDescription
			}
		}

		private func activate() async {
			RemoteCoordinator.shared.start()
			await GatewayController.shared.setServesThisMac(true)
			do {
				try await pairThisMac()
				await FilesDomain.shared.register()
				lastError = nil
			}
			catch {
				Log.warn("Could not set up Files in Finder: \(error.localizedDescription)")
				lastError = error.localizedDescription
			}
			await refreshFolders()
		}

		/// Turns the setting off, unless Finder is the only way to folders: their files aren't on this Mac, and removing the
		/// location would also discard what the system's copy holds that didn't reach the service
		private func deactivate() async throws {
			let coordinator = RemoteCoordinator.shared
			await coordinator.reloadState()
			await refreshFolders()
			guard coordinator.bindings.isEmpty, !folders.contains(where: \.isOnDemand) else {
				throw Failure.foldersInFinder
			}
			await FilesDomain.shared.remove()
			if let error = FilesDomain.shared.lastError {
				throw LocalizedFailure(message: error)
			}
			coordinator.stop()
			await GatewayController.shared.setServesThisMac(false)
			isEnabled = false
			UserDefaults.standard.set(false, forKey: Self.enabledKey)
			serviceID = nil
			lastError = nil
		}

		/// Keeps this Mac's File Provider on the service's address and key, whenever those can have changed. The app
		/// pairs with the service itself in `activate`.
		func serviceDidChange() async {
			guard isEnabled, let environment = RemoteEnvironment.shared,
				let address = await GatewayController.shared.localAddress(),
				let serviceID = GatewayController.shared.status?.serviceID,
				let known = try? environment.store.remoteEndpoint(serviceID: serviceID)
			else {
				return
			}
			var endpoint = known
			endpoint.port = address.port
			endpoint.pinnedSPKIHash = address.spki
			endpoint.nextPinnedSPKIHash = nil
			if endpoint != known {
				do {
					try environment.store.saveRemoteEndpoint(endpoint)
					environment.forgetClient(serviceID: serviceID)
				}
				catch {
					Log.warn("Could not update this Mac as a gateway: \(error.localizedDescription)")
				}
			}
		}

		/// Makes this Mac a gateway of its own File Provider
		private func pairThisMac() async throws {
			guard let environment = RemoteEnvironment.shared else {
				throw Failure.unavailable
			}
			let controller = GatewayController.shared
			controller.thisMacClientID = try environment.store.remoteClientID()
			guard await controller.localAddress() != nil, let serviceID = controller.status?.serviceID else {
				throw Failure.notRunning
			}
			if try environment.store.remoteEndpoint(serviceID: serviceID) != nil,
				try environment.credentials.credential(serviceID: serviceID) != nil
			{
				await serviceDidChange()
				self.serviceID = serviceID
				await RemoteCoordinator.shared.refresh()
				return
			}
			let session = try await controller.startLocalPairing()
			let endpoint = try await RemotePairing.pairThisMac(link: session.url) { requestID in
				try await controller.approvePairing(requestID: requestID, grants: [:])
			}
			self.serviceID = endpoint.serviceID
			await RemoteCoordinator.shared.didPair(serviceID: endpoint.serviceID)
		}

		// MARK: - Folders

		func refreshFolders() async {
			guard let client else {
				return
			}
			folders = await Self.list(client)
		}

		@concurrent nonisolated private static func list(_ client: SushitrainClient) async -> [Folder] {
			let folders = (client.folders()?.asArray() ?? []).compactMap { client.folder(withID: $0) }
			return folders.filter { $0.exists() && !$0.isPhotoFolder && !$0.isReceiveEncryptedFolder }
				.sorted()
				.map { folder in
					let restriction =
						folder.folderType() == SushitrainFolderTypeSendReceive
						? nil : String(localized: "Only send-receive folders can be on demand.")
					return Folder(
						id: folder.folderID, name: folder.displayName,
						isOnDemand: (try? folder.checkedIsOnDemand()) == true, restriction: restriction)
				}
		}

		/// The folder as the Synctrain location shows it, if it does
		func localFolderID(_ folderID: String) -> String? {
			serviceID.flatMap { RemoteCoordinator.shared.localFolderID(serviceID: $0, remoteFolderID: folderID) }
		}

		/// Makes a folder on demand and shows it in Finder, and returns how many files stay on this Mac because no connected
		/// device has them. A folder that is on demand already finishes what an interrupted switch left to do.
		func switchToOnDemand(folderID: String) async throws -> Int {
			try await inOrder {
				self.switching = folderID
				defer { self.switching = nil }
				return try await self.bring(folderID: folderID)
			}
		}

		private func bring(folderID: String) async throws -> Int {
			guard let client, let folder = client.folder(withID: folderID), let serviceID else {
				throw Failure.unavailable
			}
			let controller = GatewayController.shared
			await controller.refresh()
			let export = controller.exports[folderID]
			if export?.allowWrite == false {
				// Allowing changes would also give write access to devices that have it for a folder that didn't allow it
				throw Failure.exportedReadOnly
			}
			let kept = try await Self.makeOnDemand(folder)
			await refreshFolders()
			if export == nil {
				try await controller.setExport(folderID: folderID, exported: true, allowWrite: true)
				remember(ownExport: folderID, true)
			}
			if let clientID = controller.thisMacClientID {
				try await controller.setAccess(clientID: clientID, folderID: folderID, access: .write)
			}
			let coordinator = RemoteCoordinator.shared
			await coordinator.refresh(serviceID: serviceID)
			guard let remote = coordinator.status(serviceID: serviceID)?.folders?.first(where: { $0.id == folderID }) else {
				throw Failure.notListed
			}
			try await coordinator.addFolder(remote, serviceID: serviceID)
			return kept
		}

		private func remember(ownExport folderID: String, _ own: Bool) {
			var exports = Set(UserDefaults.standard.stringArray(forKey: Self.ownExportsKey) ?? [])
			if own {
				exports.insert(folderID)
			}
			else {
				exports.remove(folderID)
			}
			UserDefaults.standard.set(exports.sorted(), forKey: Self.ownExportsKey)
		}

		@concurrent nonisolated private static func makeOnDemand(_ folder: SushitrainFolder) async throws -> Int {
			var kept = 0
			try folder.switchTo(onDemand: &kept)
			return kept
		}

		/// Takes a folder out of Finder and synchronizes all of it again. Refused while its edits are unresolved.
		func switchToSynchronizing(folderID: String) async throws {
			try await inOrder {
				self.switching = folderID
				defer { self.switching = nil }
				try await self.release(folderID: folderID)
			}
		}

		private func release(folderID: String) async throws {
			guard let client, let folder = client.folder(withID: folderID) else {
				throw Failure.unavailable
			}
			if let localID = localFolderID(folderID) {
				// What the location cannot synchronize goes into the folder's own directory, where it would have been
				// had the folder never been on demand, and is synchronized from now on like anything else in it
				try await RemoteCoordinator.shared.removeFolder(folderID: localID, keeping: folder.localNativeURL)
			}
			let controller = GatewayController.shared
			if let clientID = controller.thisMacClientID {
				try await controller.setAccess(clientID: clientID, folderID: folderID, access: .none)
			}
			let ownExports = UserDefaults.standard.stringArray(forKey: Self.ownExportsKey) ?? []
			if ownExports.contains(folderID), !controller.clients.contains(where: { $0.permissions?[folderID] != nil }) {
				try await controller.setExport(folderID: folderID, exported: false, allowWrite: false)
				remember(ownExport: folderID, false)
			}
			try await Self.makeSynchronized(folder)
			await refreshFolders()
		}

		@concurrent nonisolated private static func makeSynchronized(_ folder: SushitrainFolder) async throws {
			try folder.setSelective(false)
		}
	}

	/// A failure that only has a message, as the File Provider framework's do once they went through `FilesDomain`
	private struct LocalizedFailure: LocalizedError {
		let message: String
		var errorDescription: String? { message }
	}
#endif
