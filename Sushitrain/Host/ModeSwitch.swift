// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SynctrainFileProviderKit

	/// Changes what Synctrain does on this Mac (`AppMode`), which takes starting the app again. Each mode leaves what the
	/// other one needs as it is: the engine's configuration and folders, and the pairings with gateways.
	@MainActor enum ModeSwitch {
		enum Refusal: LocalizedError {
			case unresolvedEdits
			case filesLeftBehind

			var errorDescription: String? {
				switch self {
				case .unresolvedEdits:
					return String(
						localized:
							"Edits of folders are still being sent or wait for review. Wait until they are sent, or export or discard them, then try again."
					)
				case .filesLeftBehind:
					return String(
						localized:
							"Finder's Synctrain location still holds files that Synctrain could not move out of it. Move them somewhere else in Finder, then try again."
					)
				}
			}
		}

		/// Asks whether to stop synchronizing, and does. Nothing has to be undone: folders stay where they are, and pick
		/// up where they were when this Mac switches back.
		static func askToSwitchToFilesInFinder() {
			let alert = NSAlert()
			alert.messageText = String(localized: "Switch to Files in Finder?")
			var explanation = String(
				localized:
					"Synctrain restarts without its engine, so this Mac stops synchronizing. Your folders stay where they are, and synchronize again when you switch back. Finder shows the folders of a gateway on another system instead."
			)
			if !GatewayController.shared.clients.isEmpty {
				explanation +=
					"\n\n" + String(localized: "Devices paired with this Mac can't reach its folders until you switch back.")
			}
			alert.informativeText = explanation
			alert.addButton(withTitle: String(localized: "Switch"))
			alert.addButton(withTitle: String(localized: "Cancel"))
			if alert.runModal() == .alertFirstButtonReturn {
				AppMode.choose(.filesInFinder)
				AppRelauncher.relaunch()
			}
		}

		/// Takes every folder out of Finder and the location with them, and starts the app again with its engine. The mode
		/// changes last: stopping anywhere leaves this Mac in Files in Finder mode, which sets up the location again, and
		/// this can be repeated. What only the system's copy holds is the user's, and is moved out or left for the system
		/// to keep, never removed.
		static func leaveFilesInFinder() async throws {
			let coordinator = RemoteCoordinator.shared
			if let environment = RemoteEnvironment.shared {
				let store = environment.store
				let unresolved = await Task.detached(priority: .userInitiated) {
					(try? store.hasUnresolvedMutations()) ?? true
				}.value
				guard !unresolved else {
					throw Refusal.unresolvedEdits
				}
				await coordinator.reloadState()
				for folderID in coordinator.bindings.keys.sorted() {
					guard try await coordinator.removeFolder(folderID: folderID) else {
						throw Refusal.filesLeftBehind
					}
				}
				try await coordinator.removeLocation()
				await Task.detached(priority: .userInitiated) {
					try? store.reset()
					environment.outbox().removeReleasedTokens()
					environment.storage.trimContent(toBytes: 0)
				}.value
			}
			// These only exist in this process, and tell where files went
			while !coordinator.leftovers.isEmpty {
				try await Task.sleep(for: .milliseconds(200))
			}
			AppMode.choose(.synchronize)
			AppRelauncher.relaunch()
		}

		/// Looks for a location that outlived Files in Finder mode, as after a failed removal or with restored preferences:
		/// the engine doesn't serve it and nothing else would remove it. One that holds folders is left to the user, as what
		/// it holds may be nowhere else.
		static func removeLeftoverLocation(tell: @escaping @MainActor (String) -> Void) async {
			guard let environment = RemoteEnvironment.shared else {
				return
			}
			let domain = FilesDomain.shared
			await domain.refreshState()
			guard domain.isEnabled != nil else {
				return
			}
			let coordinator = RemoteCoordinator.shared
			await coordinator.reloadState()
			let store = environment.store
			let unresolved = await Task.detached(priority: .utility) {
				(try? store.hasUnresolvedMutations()) ?? true
			}.value
			guard coordinator.bindings.isEmpty, !unresolved else {
				tell(
					String(
						localized:
							"Finder still shows Synctrain folders of a gateway. Their changes wait until this Mac switches to Files in Finder, where you can remove them."
					))
				return
			}
			do {
				if let kept = try await domain.remove() {
					tell(
						String(
							localized:
								"macOS kept files from Finder's Synctrain location that never reached a gateway. They are in \(kept.path(percentEncoded: false)), where you can move or remove them."
						))
				}
			}
			catch {
				tell(error.localizedDescription)
			}
		}
	}
#endif
