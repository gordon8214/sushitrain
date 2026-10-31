// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainFileProviderKit

/// The app's requests to gateways and its reads of the shared index, off the main actor. Nothing here holds a
/// database transaction open while waiting for the network.
enum RemoteWork {
	/// What the app shows about remote folders, read from the index in one go
	struct State: Sendable {
		var endpoints: [RemoteEndpoint] = []
		var bindings: [String: RemoteFolderBinding] = [:]
		var folders: [IndexFolder] = []
		var operations: [RemoteOperationSummary] = []
		var hasUnresolvedOperations = false
	}

	static func loadState(environment: RemoteEnvironment) throws(IndexError) -> State {
		let store = environment.store
		var state = State()
		state.endpoints = try store.remoteEndpoints()
		state.bindings = Dictionary(
			uniqueKeysWithValues: try store.remoteBindings().map { ($0.folderID, $0) })
		state.folders = try store.folders()
		state.operations = try RemoteOperationSummary.load(store: store, folderID: nil)
		state.hasUnresolvedOperations = try store.hasUnresolvedMutations()
		return state
	}

	// MARK: - Refreshing

	/// The outcome of checking a gateway and refreshing the metadata of its folders
	struct GatewayRefresh: Sendable {
		var serviceID: String
		/// The endpoint as stored afterwards (the gateway may have announced a certificate rotation)
		var endpoint: RemoteEndpoint
		var connection: RemoteGatewayStatus.Connection
		var capabilities: Capabilities?
		var folders: [RemoteFolder]?
		var authorityChanged = false
		/// Local folders whose items shown in Files changed
		var changedFolders: [String] = []
		/// Problems of single folders, by local folder ID
		var folderProblems: [String: String] = [:]
	}

	/// Checks the gateway (capabilities, authority generation and folder permissions) and brings the metadata of
	/// its bound folders up to date, without long polling
	static func refresh(_ endpoint: RemoteEndpoint, environment: RemoteEnvironment) async -> GatewayRefresh {
		let store = environment.store
		// As stored now: an announced certificate rotation may have added a pin
		let endpoint = (try? store.remoteEndpoint(serviceID: endpoint.serviceID)) ?? endpoint
		let sync = environment.sync(for: endpoint)
		var result = GatewayRefresh(serviceID: endpoint.serviceID, endpoint: endpoint, connection: .unknown)
		let previousGeneration = try? store.remoteAuthorityGeneration(serviceID: endpoint.serviceID)
		do throws(RemoteError) {
			let capabilities = try await sync.refreshCapabilities()
			result.capabilities = capabilities
			result.authorityChanged =
				previousGeneration != nil && previousGeneration != capabilities.authorityGeneration
			if var stored = try? store.remoteEndpoint(serviceID: endpoint.serviceID) {
				if !capabilities.serviceName.isEmpty, stored.displayName != capabilities.serviceName {
					stored.displayName = capabilities.serviceName
					try? store.saveRemoteEndpoint(stored)
				}
				result.endpoint = stored
			}
			guard capabilities.apiVersion == RemoteGatewayStatus.apiVersion else {
				result.connection = .incompatible(apiVersion: capabilities.apiVersion)
				return result
			}
			result.connection = .reachable
			let folders = try await sync.refreshFolders()
			result.folders = folders
			let bindings = (try? store.remoteBindings()) ?? []
			for binding in bindings where binding.serviceID == endpoint.serviceID && binding.permissions.read {
				do throws(RemoteError) {
					if try await sync.refresh(folderID: binding.folderID) || result.authorityChanged {
						result.changedFolders.append(binding.folderID)
					}
				}
				catch .canceled {
					return result
				}
				catch {
					// What concerns the whole gateway is its status, not a problem of the folder
					if case .unreachable = error {
						throw error
					}
					if error == .unauthenticated {
						throw error
					}
					result.folderProblems[binding.folderID] = error.userDescription
				}
			}
			for binding in bindings where binding.serviceID == endpoint.serviceID && !binding.permissions.read {
				result.folderProblems[binding.folderID] = String(
					localized: "This device may no longer read the folder on the gateway.")
			}
		}
		catch .canceled {
			return result
		}
		catch {
			result.connection = RemoteGatewayStatus.connection(for: error)
		}
		return result
	}

	/// A folder to long-poll, with the folders of all gateways that are only refreshed
	struct PollTarget: Sendable, Hashable {
		var folderID: String
		var endpoint: RemoteEndpoint
		/// Seconds to hold the request for (nil when the gateway does not long-poll)
		var wait: Int?
	}

	/// Refreshes the other folders without waiting, then long-polls the target. Returns whether the target's shown
	/// items changed, and the other folders whose items changed.
	static func poll(_ target: PollTarget, others: [PollTarget], environment: RemoteEnvironment) async
		-> (result: Result<Bool, RemoteError>, changedOthers: [String])
	{
		var changedOthers: [String] = []
		for other in others {
			if Task.isCancelled {
				break
			}
			if (try? await environment.sync(for: other.endpoint).refresh(folderID: other.folderID)) == true {
				changedOthers.append(other.folderID)
			}
		}
		do throws(RemoteError) {
			let changed = try await environment.sync(for: target.endpoint).refresh(
				folderID: target.folderID, wait: target.wait)
			return (.success(changed), changedOthers)
		}
		catch {
			return (.failure(error), changedOthers)
		}
	}

	// MARK: - Folders

	/// Adds a folder of the gateway and lists it
	static func addFolder(_ remote: RemoteFolder, endpoint: RemoteEndpoint, environment: RemoteEnvironment) async
		-> Result<String, GatewayFolderError>
	{
		let folderID: String
		do throws(IndexError) {
			folderID = try environment.store.addRemoteFolder(remote, serviceID: endpoint.serviceID)
		}
		catch {
			return .failure(.store(error))
		}
		Log.info("Added folder \(remote.id) of gateway \(endpoint.serviceID) as \(folderID)")
		do throws(RemoteError) {
			try await environment.sync(for: endpoint).refresh(folderID: folderID)
			return .success(folderID)
		}
		catch {
			return .failure(.remote(error))
		}
	}

	/// Removes a folder from this device, with the files it leaves behind
	static func removeFolder(folderID: String, environment: RemoteEnvironment) -> Result<Void, GatewayFolderError> {
		do throws(IndexError) {
			environment.removeStagedFiles(try environment.store.removeRemoteFolder(folderID: folderID))
			Log.info("Removed folder \(folderID)")
			return .success(())
		}
		catch .notAllowed {
			return .failure(.unresolvedOperations)
		}
		catch {
			return .failure(.store(error))
		}
	}

	// MARK: - Outbox

	/// What a pass over the outbox did
	struct OutboxPass: Sendable {
		/// Operations worked on
		var claimed = 0
		/// Operations that reached a final state
		var resolved = 0
		/// Errors that concern a whole gateway, by service ID
		var gatewayErrors: [String: RemoteError] = [:]
		/// When an operation can make progress next (nil: nothing is waiting)
		var nextAttempt: Date?
		/// Uploads whose rest went to the app's background session
		var handedOver = 0
	}

	/// How long operations for a gateway that speaks another version of the API wait before they are claimed again
	static let incompatibleRetryInterval: TimeInterval = 300

	/// Works on the operations that can make progress now, one claim at a time, in the app's name. Nothing is sent to
	/// the gateways that speak another version of the API (`incompatible`, by service ID): their operations wait.
	/// Once the handoff is requested, the rest of the upload in progress goes to the app's background session, and
	/// nothing more is started.
	static func runOutbox(
		environment: RemoteEnvironment, incompatible: Set<String> = [], limit: Int = 50,
		handoff: BackgroundHandoff? = nil
	) async -> OutboxPass {
		let store = environment.store
		let outbox = environment.outbox(handoff: handoff)
		var pass = OutboxPass()
		for _ in 0..<limit {
			if Task.isCancelled || handoff?.isRequested == true {
				break
			}
			guard let claim = try? store.claimMutation(owner: environment.owner, duration: outbox.claimDuration) else {
				break
			}
			if let serviceID = claim.record.serviceID, incompatible.contains(serviceID) {
				// Released with a delay, so that the next claim of this pass is another operation
				_ = try? store.recordRetry(
					claim, error: "the gateway speaks another version of the remote file API",
					notBefore: Date().addingTimeInterval(incompatibleRetryInterval))
				continue
			}
			pass.claimed += 1
			do throws(RemoteError) {
				if case .resolved = try await outbox.process(claim) {
					pass.resolved += 1
				}
			}
			catch {
				if let serviceID = claim.record.serviceID, error != .canceled {
					pass.gatewayErrors[serviceID] = error
				}
			}
		}
		outbox.removeReleasedFiles()
		outbox.removeIdleTransfers()
		pass.handedOver = handoff?.handedOver.count ?? 0
		pass.nextAttempt = nextAttempt(store: store)
		return pass
	}

	/// When an unfinished operation can be worked on next (by this process or the extension)
	static func nextAttempt(store: IndexStore) -> Date? {
		let waiting =
			(try? store.mutations(states: [.staged, .uploading, .uploaded, .submitted, .pending], limit: 500))
			?? []
		let now = Date()
		return waiting.filter { $0.kind != .crossHostMove }.map { record in
			max(record.notBefore ?? now, record.claimExpires ?? now)
		}.min()
	}

	// MARK: - Background transfers

	/// Goes on with what background tasks (of either session) finished: the operations whose upload task ended are
	/// submitted, and followed while the gateway applies them (briefly), and downloads are verified. `operationIDs`
	/// are those the session's events named; transfers whose task ended are looked up as well. An upload a task did
	/// not finish goes on through the handoff (in the background, a task of the app's session sends its rest).
	static func finishBackgroundTransfers(
		_ operationIDs: Set<String>, environment: RemoteEnvironment, incompatible: Set<String> = [],
		handoff: BackgroundHandoff? = nil
	) async -> OutboxPass {
		let store = environment.store
		var pass = OutboxPass()
		var pending = operationIDs
		for transfer in (try? store.transfers(kind: .upload, limit: 500)) ?? []
		where transfer.taskDescription == nil {
			if let operationID = transfer.operationID {
				pending.insert(operationID)
			}
		}
		for transfer in (try? store.transfers(kind: .download, limit: 500)) ?? []
		where transfer.taskDescription == nil && transfer.state == .downloaded {
			finishDownload(transfer, environment: environment)
		}
		let outbox = environment.outbox(handoff: handoff)
		for operationID in pending.sorted() {
			if Task.isCancelled {
				break
			}
			guard let record = try? store.mutation(operationID: operationID), !record.state.isTerminal,
				!(record.serviceID.map(incompatible.contains) ?? false)
			else {
				continue
			}
			try? store.expediteMutation(operationID: operationID)
			pass.claimed += 1
			// Committed, or pending on the gateway for a few seconds at most: the outbox follows it up later
			for _ in 0..<10 {
				let outcome: Outbox.Outcome
				do throws(RemoteError) {
					outcome = try await outbox.process(operationID: operationID)
				}
				catch {
					if let serviceID = record.serviceID, error != .canceled {
						pass.gatewayErrors[serviceID] = error
					}
					break
				}
				guard case .waiting(let waiting, let notBefore) = outcome, waiting.state == .pending else {
					if case .resolved = outcome {
						pass.resolved += 1
					}
					break
				}
				try? await Task.sleep(for: .seconds(min(max((notBefore ?? Date()).timeIntervalSinceNow, 0.05), 2)))
			}
		}
		outbox.removeIdleTransfers()
		outbox.removeReleasedFiles()
		outbox.removeReleasedTokens()
		pass.handedOver = handoff?.handedOver.count ?? 0
		pass.nextAttempt = nextAttempt(store: store)
		return pass
	}

	/// Verifies a download a background task finished (and publishes it to the content cache when it is complete).
	/// Downloads that still miss blocks are left for whoever needs the file next.
	static func finishDownload(_ transfer: TransferRecord, environment: RemoteEnvironment) {
		let store = environment.store
		guard let itemID = transfer.itemID, let item = try? store.item(id: itemID),
			let binding = try? store.remoteBinding(folderID: item.folderID),
			let client = environment.client(serviceID: binding.serviceID)
		else {
			return
		}
		let downloader = RemoteDownloader(
			store: store, client: client, storage: environment.storage, scheduler: environment.scheduler,
			credentials: environment.credentials, owner: environment.owner)
		do throws(RemoteError) {
			if try downloader.finishBackgroundDownload(transferID: transfer.transferID) != nil {
				Log.info("Finished a background download of a remote file")
			}
		}
		catch {
			Log.warn("Could not finish a background download: \(error.userDescription)")
		}
	}

	// MARK: - Operations

	/// Gives up an operation (after the user reviewed it) and removes the staged files it released
	static func discard(operationID: String, environment: RemoteEnvironment) throws(IndexError) {
		let result = try environment.store.discardMutation(operationID: operationID)
		environment.removeStagedFiles(result.releasedStagedFiles)
	}

	/// A copy of an operation's staged contents named like the item, in a temporary directory, to export
	static func exportCopy(of summary: RemoteOperationSummary, environment: RemoteEnvironment) throws -> URL {
		guard let stagedPath = summary.stagedPath else {
			throw CocoaError(.fileNoSuchFile)
		}
		let source = environment.storage.url(relativePath: stagedPath)
		let directory = URL.temporaryDirectory.appending(path: "Export-\(UUID().uuidString)", directoryHint: .isDirectory)
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		let name = FileNames.isValid(summary.name) ? summary.name : String(localized: "Edited file")
		let destination = directory.appending(path: name, directoryHint: .notDirectory)
		// FileManager clones where the volume allows it, so that exporting a large edit takes no extra space
		try FileManager.default.copyItem(at: source, to: destination)
		try FileManager.default.setAttributes(
			[.posixPermissions: 0o644], ofItemAtPath: destination.path(percentEncoded: false))
		return destination
	}
}
