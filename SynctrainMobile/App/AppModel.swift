// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Observation
import SynctrainFileProviderKit

/// Starts the app's work with gateways and decides what the app shows first
@Observable @MainActor final class AppModel {
	enum Phase: Equatable {
		/// The app group is not available, so nothing can be stored
		case unavailable
		case loading
		/// The stored state could not be read (for example before the first unlock). This never counts as having no
		/// gateways: nothing may be set up again on the strength of it.
		case loadFailed(String)
		case needsGateway
		case needsFolders
		case ready
	}

	private enum Load: Equatable {
		case notDone
		case failed(String)
		case done
	}

	private var load = Load.notDone
	private let isAvailable = RemoteEnvironment.shared != nil

	var phase: Phase {
		guard isAvailable else {
			return .unavailable
		}
		switch load {
		case .notDone:
			return .loading
		case .failed(let message):
			return .loadFailed(message)
		case .done:
			let coordinator = RemoteCoordinator.shared
			if coordinator.gateways.isEmpty {
				return .needsGateway
			}
			return coordinator.bindings.isEmpty ? .needsFolders : .ready
		}
	}

	/// Reads the stored state and starts the work with gateways. Call again after a failure.
	func start() async {
		guard let environment = RemoteEnvironment.shared else {
			return
		}
		load = .notDone
		let result = await Task.detached(priority: .userInitiated) { () -> Result<[RemoteEndpoint], any Error> in
			Result {
				try environment.container.createDirectories()
				return try environment.store.remoteEndpoints()
			}
		}.value
		switch result {
		case .success(let endpoints):
			ContentOpener.removeViewingCopies()
			Task.detached(priority: .utility) { environment.storage.trimContent(toBytes: CacheSettings.limit) }
			if !endpoints.isEmpty {
				await FilesDomain.shared.register()
			}
			await RemoteCoordinator.shared.reloadState()
			RemoteCoordinator.shared.start()
			load = .done
		case .failure(let error):
			Log.warn("Could not read the state of gateways: \(error.localizedDescription)")
			load = .failed(error.localizedDescription)
		}
	}
}
