// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Why a folder cannot be added or switched now
	enum RemoteSwitchError: LocalizedError, Sendable {
		case notEligible(String)
		case localChangesWaiting(LocalChangeSummary)
		case folderPaused
		case folderBusy(state: String)
		case gatewayDeviceUnknown(gateway: String)
		case notSharedWithGateway(gateway: String)
		case gatewayDeviceNotConnected(gateway: String)
		case gatewayCatchingUp(count: Int, gateway: String)
		case gatewayLacksChanges(count: Int, gateway: String)
		case unresolvedOperations
		case gatewayUnavailable(String)
		case engine(String)
		case store(IndexError)
		case remote(RemoteError)

		var errorDescription: String? {
			switch self {
			case .notEligible(let reason):
				return reason
			case .localChangesWaiting(let changes):
				return String(
					localized:
						"Changes to \(changes.pendingItems + changes.queuedWork) items made on this device have not been picked up by Syncthing yet. Keep Synctrain open until they are synchronized, then try again."
				)
			case .folderPaused:
				return String(
					localized:
						"The folder is paused. Resume it and wait until it is up to date with the gateway, so that no change made on this device is left behind."
				)
			case .folderBusy(let state):
				return String(
					localized:
						"The folder is busy (\(state)). Wait until it is up to date, then try again."
				)
			case .gatewayDeviceUnknown(let gateway):
				return String(
					localized:
						"\(gateway) does not tell which Syncthing device it is, so Synctrain cannot check that it has every change made on this device."
				)
			case .notSharedWithGateway(let gateway):
				return String(
					localized:
						"This folder is not shared with the Syncthing device of \(gateway). Only folders that are synchronized with the gateway can be switched to remote access."
				)
			case .gatewayDeviceNotConnected(let gateway):
				return String(
					localized:
						"Synctrain is not connected to the Syncthing device of \(gateway). It needs to be, to check that the gateway has every change made on this device."
				)
			case .gatewayCatchingUp(let count, let gateway):
				return String(
					localized:
						"\(gateway) still needs \(count) items of this folder. Wait until it is up to date, then try again."
				)
			case .gatewayLacksChanges(let count, let gateway):
				return String(
					localized:
						"\(gateway) does not have \(count) changes made on this device yet. Keep Synctrain open until they are synchronized, then try again."
				)
			case .unresolvedOperations:
				return String(
					localized:
						"Edits made in this folder have not been confirmed by the gateway. Wait until they are sent, or export or discard them, then try again."
				)
			case .gatewayUnavailable(let problem):
				return problem
			case .engine(let message):
				return message
			case .store(let error):
				return error.userDescription
			case .remote(let error):
				return error.userDescription
			}
		}
	}

	/// Moves a folder between Syncthing on this device and a gateway, explicitly and one folder at a time. Every step
	/// is recorded in the folder's journal (`remote_migrations`) before the next one starts, so that a switch that was
	/// interrupted resumes from where it stopped. A switch to a gateway is refused whenever this device might have a
	/// change the gateway lacks; nothing is ever overwritten, and the folder's data on this device is kept.
	struct RemoteFolderSwitcher: Sendable {
		let environment: RemoteEnvironment
		let client: SushitrainClient

		private var store: IndexStore { environment.store }

		/// More items than this missing on the gateway are not checked one by one
		static let neededItemsLimit = 20_000

		private func stored<T>(_ body: () throws(IndexError) -> T) throws(RemoteSwitchError) -> T {
			do {
				return try body()
			}
			catch {
				throw .store(error)
			}
		}

		// MARK: - To the gateway

		/// Switches the Syncthing folder of this device to access through the gateway, which serves the same folder
		/// (with the same ID) and synchronizes it with this device
		func switchToGateway(
			folderID: String, endpoint: RemoteEndpoint, capabilities: Capabilities, remoteFolder: RemoteFolder
		) async throws(RemoteSwitchError) {
			let gateway = endpoint.displayName
			guard remoteFolder.id == folderID else {
				throw .notEligible(String(localized: "The folder on the gateway has another folder ID."))
			}
			guard remoteFolder.permissions.read else {
				throw .notEligible(String(localized: "The gateway does not let this device read the folder."))
			}
			guard let folder = client.folder(withID: folderID), folder.exists() else {
				throw .notEligible(String(localized: "The folder does not exist on this device."))
			}
			guard folder.isInManagedBase(), folder.isRegularFolder, !folder.isPhotoFolder, folder.isExternal == false
			else {
				throw .notEligible(
					String(localized: "Only folders that Synctrain stores itself can be accessed through a gateway."))
			}
			guard !client.isFolderRemoteManaged(folderID) else {
				throw .notEligible(String(localized: "The folder is accessed through a gateway already."))
			}
			guard let deviceID = capabilities.syncthingDeviceID, !deviceID.isEmpty else {
				throw .gatewayDeviceUnknown(gateway: gateway)
			}
			guard folder.isShared(withDeviceID: deviceID) else {
				throw .notSharedWithGateway(gateway: gateway)
			}

			// Fence new edits, in one transaction with the check that Files has no change waiting for Syncthing
			let journal = RemoteMigration.Journal(
				direction: .toGateway, serviceID: endpoint.serviceID, remoteFolderID: remoteFolder.id,
				rootGatewayID: remoteFolder.rootItemID, permissions: remoteFolder.permissions, label: folder.displayName)
			let migration: RemoteMigration
			do throws(IndexError) {
				migration = try store.beginRemoteMigration(folderID: folderID, journal: journal)
			}
			catch .notAllowed(let reason) {
				let changes = try stored { () throws(IndexError) in try store.localChanges(folderID: folderID) }
				throw changes.isEmpty ? .notEligible(reason) : .localChangesWaiting(changes)
			}
			catch {
				throw .store(error)
			}
			if migration.state == .fenced {
				do throws(RemoteSwitchError) {
					try checkEngineIsSettled(folder: folder, deviceID: deviceID, gateway: gateway)
					try checkGatewayHasLocalChanges(folder: folder, deviceID: deviceID, gateway: gateway)
					try stopEngine(folder: folder, deviceID: deviceID, gateway: gateway)
				}
				catch {
					throw abandonSwitch(folderID: folderID, folder: folder, because: error)
				}
			}
			try await continueToGateway(folderID: folderID)
		}

		/// Gives up a switch to a gateway that a check refused: Syncthing on this device takes the folder back, and the
		/// fence is lifted. When Syncthing could not take the folder back, the fence stays (edits would otherwise reach a
		/// folder that nothing synchronizes) with the problem in the journal, and resuming the switch later checks
		/// again. Returns the error to report: the reason the switch was refused.
		private func abandonSwitch(folderID: String, folder: SushitrainFolder?, because reason: RemoteSwitchError)
			-> RemoteSwitchError
		{
			if client.isFolderRemoteManaged(folderID) {
				do {
					try client.setFolderRemoteManaged(folderID, managed: false)
				}
				catch {
					Log.warn(
						"Could not give folder \(folderID) back to Syncthing after the switch was refused: \(error.localizedDescription)"
					)
					_ = try? store.updateRemoteMigration(folderID: folderID, lastError: error.localizedDescription)
					return reason
				}
				do {
					try folder?.setPaused(false)
				}
				catch {
					// Syncthing has the folder again; the user can resume it
					Log.warn("Could not resume folder \(folderID): \(error.localizedDescription)")
				}
			}
			do throws(IndexError) {
				try store.finishRemoteMigration(folderID: folderID)
			}
			catch {
				Log.warn("Could not lift the fence of folder \(folderID): \(error.userDescription)")
			}
			return reason
		}

		/// Refuses the switch unless Syncthing has scanned what is on disk and knows what the gateway has: the folder must
		/// be running and idle, and connected to the gateway's device
		private func checkEngineIsSettled(folder: SushitrainFolder, deviceID: String, gateway: String)
			throws(RemoteSwitchError)
		{
			guard !folder.isPaused() else {
				throw .folderPaused
			}
			var stateError: NSError? = nil
			let state = folder.state(&stateError)
			guard state == "idle" else {
				throw .folderBusy(state: state.isEmpty ? String(localized: "unknown") : state)
			}
			guard let peer = client.peer(withID: deviceID), peer.isConnected() else {
				throw .gatewayDeviceNotConnected(gateway: gateway)
			}
		}

		/// Refuses the switch while the gateway might lack a version that only this device has: none of the items the
		/// gateway still needs may be a version (or deletion) this device made
		private func checkGatewayHasLocalChanges(folder: SushitrainFolder, deviceID: String, gateway: String)
			throws(RemoteSwitchError)
		{
			let needed: [String]
			do {
				needed = try folder.filesNeeded(by: deviceID).asArray()
			}
			catch {
				throw .engine(error.localizedDescription)
			}
			guard needed.count <= Self.neededItemsLimit else {
				throw .gatewayCatchingUp(count: needed.count, gateway: gateway)
			}
			let me = client.shortDeviceID()
			var mine = 0
			for path in needed {
				// A version (or deletion) whose latest change came from this device exists only here until the gateway
				// has it
				if let entry = try? folder.getFileInformation(path), entry.modifiedByShortDeviceID() == me {
					mine += 1
				}
			}
			if mine > 0 {
				throw .gatewayLacksChanges(count: mine, gateway: gateway)
			}
		}

		/// Makes Syncthing on this device stop scanning, pulling and announcing the folder, then checks once more that
		/// no edit reached the folder (or was scanned) in the meantime, which would otherwise only be on this device.
		/// When one did, the caller gives up the switch (and Syncthing takes the folder back).
		private func stopEngine(folder: SushitrainFolder, deviceID: String, gateway: String) throws(RemoteSwitchError) {
			let folderID = folder.folderID
			do {
				try client.setFolderRemoteManaged(folderID, managed: true)
			}
			catch {
				throw .engine(error.localizedDescription)
			}
			try checkStoppedEngine(folder: folder, deviceID: deviceID, gateway: gateway)
			_ = try stored { () throws(IndexError) in
				try store.updateRemoteMigration(folderID: folderID, state: .engineStopped, lastError: nil)
			}
			Log.info("Folder \(folderID) is now left to its gateway by Syncthing on this device")
		}

		/// Once Syncthing on this device has stopped with the folder (nothing changes what it knows anymore): refuses the
		/// switch when an edit reached the folder or was scanned before it stopped, or the gateway lacks a version that
		/// only this device has
		private func checkStoppedEngine(folder: SushitrainFolder, deviceID: String, gateway: String)
			throws(RemoteSwitchError)
		{
			let changes = try stored { () throws(IndexError) in try store.localChanges(folderID: folder.folderID) }
			guard changes.isEmpty else {
				throw .localChangesWaiting(changes)
			}
			try checkGatewayHasLocalChanges(folder: folder, deviceID: deviceID, gateway: gateway)
		}

		/// For a switch interrupted after Syncthing stopped with the folder but before that was recorded: the checks made
		/// after stopping may not have run. They run again (asking the gateway which Syncthing device it is); when they
		/// refuse the switch, it is given up.
		private func recheckStoppedEngine(folderID: String, journal: RemoteMigration.Journal)
			async
			throws(RemoteSwitchError)
		{
			guard let folder = client.folder(withID: folderID), folder.exists() else {
				throw abandonSwitch(
					folderID: folderID, folder: nil,
					because: .notEligible(String(localized: "The folder does not exist on this device.")))
			}
			guard let serviceID = journal.serviceID,
				let endpoint = try stored({ () throws(IndexError) in try store.remoteEndpoint(serviceID: serviceID) })
			else {
				throw abandonSwitch(
					folderID: folderID, folder: folder,
					because: .notEligible(String(localized: "The gateway is not paired anymore.")))
			}
			let capabilities: Capabilities
			do throws(RemoteError) {
				capabilities = try await environment.sync(for: endpoint).refreshCapabilities()
			}
			catch {
				// The check waits until the gateway can be reached (the folder stays fenced until then)
				_ = try? store.updateRemoteMigration(folderID: folderID, lastError: error.userDescription)
				throw .remote(error)
			}
			guard let deviceID = capabilities.syncthingDeviceID, !deviceID.isEmpty else {
				throw abandonSwitch(
					folderID: folderID, folder: folder, because: .gatewayDeviceUnknown(gateway: endpoint.displayName))
			}
			do throws(RemoteSwitchError) {
				try checkStoppedEngine(folder: folder, deviceID: deviceID, gateway: endpoint.displayName)
			}
			catch {
				throw abandonSwitch(folderID: folderID, folder: folder, because: error)
			}
		}

		/// Continues a switch to a gateway whose engine step is done: binds the folder (its existing rows are adopted by
		/// path, keeping their Files identifiers, tags and favorites) and takes the first snapshot. Until the snapshot is
		/// complete the switch stays in its journal, and is continued by later refreshes.
		func continueToGateway(folderID: String) async throws(RemoteSwitchError) {
			guard var migration = try stored({ () throws(IndexError) in try store.remoteMigration(folderID: folderID) }),
				migration.journal.direction == .toGateway
			else {
				return
			}
			if migration.state == .fenced {
				// Interrupted between the checks and recording that Syncthing stopped
				guard client.isFolderRemoteManaged(folderID) else {
					try? store.finishRemoteMigration(folderID: folderID)
					throw .notEligible(String(localized: "Switching the folder was interrupted. Try again."))
				}
				try await recheckStoppedEngine(folderID: folderID, journal: migration.journal)
				migration = try stored { () throws(IndexError) in
					try store.updateRemoteMigration(folderID: folderID, state: .engineStopped, lastError: nil)
				}
			}
			let journal = migration.journal
			guard let serviceID = journal.serviceID, let remoteFolderID = journal.remoteFolderID,
				let permissions = journal.permissions
			else {
				throw .notEligible(String(localized: "The record of the switch is incomplete."))
			}
			if migration.state == .engineStopped {
				let binding = RemoteFolderBinding(
					folderID: folderID, accessMode: .remoteAPI, serviceID: serviceID, remoteFolderID: remoteFolderID,
					permissions: permissions)
				let description = FolderDescription(
					folderID: folderID, label: journal.label ?? folderID,
					folderType: RemoteWork.folderType(for: permissions), isSelective: true)
				migration = try stored { () throws(IndexError) in
					try store.bindRemoteFolder(binding, folder: description, rootGatewayID: journal.rootGatewayID)
					return try store.updateRemoteMigration(folderID: folderID, state: .bound, lastError: nil)
				}
			}
			guard migration.state == .bound else {
				return
			}
			try await completeSnapshot(folderID: folderID, serviceID: serviceID)
		}

		/// Takes the first snapshot of a newly bound folder, and ends its switch once that is complete
		private func completeSnapshot(folderID: String, serviceID: String) async throws(RemoteSwitchError) {
			guard let endpoint = try stored({ () throws(IndexError) in try store.remoteEndpoint(serviceID: serviceID) })
			else {
				throw .notEligible(String(localized: "The gateway is not paired anymore."))
			}
			do throws(RemoteError) {
				try await environment.sync(for: endpoint).refresh(folderID: folderID)
			}
			catch {
				_ = try? store.updateRemoteMigration(folderID: folderID, lastError: error.userDescription)
				throw .remote(error)
			}
			let state = try stored { () throws(IndexError) in try store.remoteSyncState(folderID: folderID) }
			guard !state.needsSnapshot else {
				return
			}
			try stored { () throws(IndexError) in try store.finishRemoteMigration(folderID: folderID) }
			Log.info("Folder \(folderID) is now accessed through its gateway")
		}

		// MARK: - Folders only on the gateway

		/// The local folder ID for a folder of the gateway: its own, unless that is taken by another folder on this
		/// device (a Syncthing folder that is not the same folder, or a folder of another gateway)
		func localFolderID(for remoteFolder: RemoteFolder, serviceID: String) throws(RemoteSwitchError) -> String {
			func isTaken(_ id: String) throws(IndexError) -> Bool {
				if client.folder(withID: id)?.exists() == true {
					return true
				}
				if let binding = try store.remoteBinding(folderID: id), binding.isRemote {
					return binding.serviceID != serviceID || binding.remoteFolderID != remoteFolder.id
				}
				return try store.folder(id: id) != nil
			}
			return try stored { () throws(IndexError) -> String in
				if try !isTaken(remoteFolder.id) {
					return remoteFolder.id
				}
				let base = "\(remoteFolder.id)~\(serviceID.suffix(8))"
				var candidate = base
				var counter = 1
				while try isTaken(candidate) {
					counter += 1
					candidate = "\(base)-\(counter)"
				}
				return candidate
			}
		}

		/// Adds a folder of the gateway that this device does not synchronize to the Files app
		func addGatewayFolder(_ remoteFolder: RemoteFolder, endpoint: RemoteEndpoint) async throws(RemoteSwitchError)
			-> String
		{
			guard remoteFolder.permissions.read else {
				throw .notEligible(String(localized: "The gateway does not let this device read the folder."))
			}
			let existing = try stored { () throws(IndexError) in try store.remoteBindings() }.first {
				$0.isRemote && $0.serviceID == endpoint.serviceID && $0.remoteFolderID == remoteFolder.id
			}
			let folderID: String
			if let existing {
				folderID = existing.folderID
			}
			else {
				folderID = try localFolderID(for: remoteFolder, serviceID: endpoint.serviceID)
				let binding = RemoteFolderBinding(
					folderID: folderID, accessMode: .remoteAPI, serviceID: endpoint.serviceID,
					remoteFolderID: remoteFolder.id, permissions: remoteFolder.permissions)
				let description = FolderDescription(
					folderID: folderID, label: remoteFolder.label.isEmpty ? remoteFolder.id : remoteFolder.label,
					folderType: RemoteWork.folderType(for: remoteFolder.permissions), isSelective: true)
				try stored { () throws(IndexError) in
					try store.bindRemoteFolder(binding, folder: description, rootGatewayID: remoteFolder.rootItemID)
				}
				Log.info("Added folder \(remoteFolder.id) of gateway \(endpoint.serviceID) to Files as \(folderID)")
			}
			do throws(RemoteError) {
				try await environment.sync(for: endpoint).refresh(folderID: folderID)
			}
			catch {
				// The folder is added; its listing follows with the next refresh
				throw .remote(error)
			}
			return folderID
		}

		// MARK: - Back to Syncthing

		/// Ends access through the gateway: the folder is synchronized by Syncthing on this device again (and stays
		/// paused until the user resumes it), or, for a folder that is only on the gateway, removed from the Files app.
		/// Refused while edits of the folder are not resolved with the gateway. Returns whether a Syncthing folder
		/// takes over.
		@discardableResult
		func switchToSyncthing(folderID: String) throws(RemoteSwitchError) -> Bool {
			let binding = try stored { () throws(IndexError) in try store.remoteBinding(folderID: folderID) }
			let journal = RemoteMigration.Journal(
				direction: .toSyncthing, serviceID: binding?.serviceID, remoteFolderID: binding?.remoteFolderID)
			do throws(IndexError) {
				try store.beginRemoteMigration(folderID: folderID, journal: journal)
			}
			catch .notAllowed(let reason) {
				if try stored({ () throws(IndexError) in try store.hasUnresolvedMutations(folderID: folderID) }) {
					throw .unresolvedOperations
				}
				throw .notEligible(reason)
			}
			catch {
				throw .store(error)
			}
			return try continueToSyncthing(folderID: folderID)
		}

		/// Continues a switch back to Syncthing: unbinds the folder (its metadata from the gateway is forgotten; its
		/// rows stay until Syncthing's index replaces them), then lets Syncthing on this device take the folder again
		@discardableResult
		func continueToSyncthing(folderID: String) throws(RemoteSwitchError) -> Bool {
			let migration = try stored { () throws(IndexError) in try store.remoteMigration(folderID: folderID) }
			guard migration?.journal.direction == .toSyncthing else {
				return false
			}
			if try stored({ () throws(IndexError) in try store.remoteBinding(folderID: folderID)?.isRemote }) == true {
				let binding = RemoteFolderBinding(
					folderID: folderID, accessMode: .syncthing, serviceID: nil, remoteFolderID: nil,
					permissions: RemotePermissions(read: false, write: false))
				do throws(IndexError) {
					try store.bindRemoteFolder(binding)
				}
				catch .notAllowed {
					try? store.finishRemoteMigration(folderID: folderID)
					throw .unresolvedOperations
				}
				catch {
					throw .store(error)
				}
			}
			if client.folder(withID: folderID)?.exists() == true {
				// The rows show the gateway's versions now: Syncthing's index replaces them with full records (keeping
				// their identifiers), rather than skipping the entries whose sequence it has seen before
				_ = try stored { () throws(IndexError) in
					try store.write { writer throws(IndexError) in try writer.setCursor(0, folderID: folderID) }
				}
			}
			if client.isFolderRemoteManaged(folderID) {
				do {
					try client.setFolderRemoteManaged(folderID, managed: false)
				}
				catch {
					throw .engine(error.localizedDescription)
				}
			}
			try stored { () throws(IndexError) in try store.finishRemoteMigration(folderID: folderID) }
			let hasSyncthingFolder = client.folder(withID: folderID)?.exists() == true
			Log.info("Folder \(folderID) is no longer accessed through a gateway (Syncthing folder: \(hasSyncthingFolder))")
			return hasSyncthingFolder
		}

		// MARK: - Resuming

		/// Continues a switch that was interrupted (at launch, or when a refresh finds it unfinished)
		func resume(_ migration: RemoteMigration) async throws(RemoteSwitchError) {
			switch migration.journal.direction {
			case .toGateway:
				try await continueToGateway(folderID: migration.folderID)
			case .toSyncthing:
				try continueToSyncthing(folderID: migration.folderID)
			}
		}
	}
#endif
