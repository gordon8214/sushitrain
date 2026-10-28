// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	import os
	import SynctrainFileProviderKit

	/// What the app's work with gateways shares: the index, the remote storage, the Keychain (in the app group, which the
	/// extension shares) and one client per paired gateway, so that parallel requests to a gateway share connections.
	/// Safe to use from any thread.
	final class RemoteEnvironment: Sendable {
		let store: IndexStore
		let storage: RemoteStorage
		let credentials: KeychainCredentialStore
		let appGroupIdentifier: String?
		/// Who claims operations, transfers and slots in the shared registry: this launch of the app
		let owner: String
		let scheduler: TransferScheduler
		private let clients = OSAllocatedUnfairLock<[String: RemoteFileClient]>(initialState: [:])

		/// The session identifiers of the app and the extension
		let appSessionIdentifier: String
		let extensionSessionIdentifier: String

		init(container: SharedContainer, bundle: Bundle = .main) {
			store = IndexStore(container: container)
			storage = RemoteStorage(container: container)
			let group = bundle.object(forInfoDictionaryKey: SharedContainer.appGroupInfoKey) as? String
			appGroupIdentifier = group?.isEmpty == false ? group : nil
			credentials = KeychainCredentialStore(accessGroup: appGroupIdentifier)
			owner = "app-\(UUID().uuidString.prefix(8).lowercased())"
			scheduler = TransferScheduler(store: store, owner: owner)
			let bundleID = RemoteSessionIdentifier.appBundleIdentifier(of: bundle) ?? "Synctrain"
			appSessionIdentifier = RemoteSessionIdentifier.app(appBundleIdentifier: bundleID)
			extensionSessionIdentifier = RemoteSessionIdentifier.fileProviderExtension(appBundleIdentifier: bundleID)
		}

		/// The environment of the shared container, or nil when the app group is not available
		static let shared: RemoteEnvironment? = FileProviderStorage.container.map { RemoteEnvironment(container: $0) }

		/// The client of a paired gateway, or nil when it is not paired
		func client(serviceID: String) -> RemoteFileClient? {
			guard let endpoint = try? store.remoteEndpoint(serviceID: serviceID) else {
				return nil
			}
			return client(for: endpoint)
		}

		/// The client of the endpoint (a new one when its address or pins changed)
		func client(for endpoint: RemoteEndpoint) -> RemoteFileClient {
			clients.withLock { clients in
				if let existing = clients[endpoint.serviceID], existing.endpoint == endpoint {
					return existing
				}
				let client = RemoteFileClient(endpoint: endpoint, credentials: credentials)
				clients[endpoint.serviceID] = client
				return client
			}
		}

		func forgetClient(serviceID: String) {
			clients.withLock { $0[serviceID] = nil }
		}

		func sync(for endpoint: RemoteEndpoint) -> RemoteSync {
			RemoteSync(store: store, client: client(for: endpoint), scheduler: scheduler)
		}

		/// The outbox, in the app's name. With a handoff, the rest of a long upload goes to the app's background session
		/// when the handoff is requested.
		func outbox(handoff: BackgroundHandoff? = nil) -> Outbox {
			var outbox = Outbox(
				store: store, storage: storage, owner: owner, scheduler: scheduler, credentials: credentials
			) { [weak self] serviceID in
				self?.client(serviceID: serviceID)
			}
			outbox.handoff = handoff
			return outbox
		}

		/// Removes staged files that no operation needs anymore
		func removeStagedFiles(_ paths: [String]) {
			RemoteUploader.removeReleasedStagedFiles(paths, store: store, storage: storage)
		}
	}
#endif
