// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// In a folder's settings: whether the folder is accessed through a gateway (or being switched), its edits waiting for
	/// the gateway, and switching it between Syncthing on this device and the gateway
	struct RemoteFolderAccessSection: View {
		let folder: SushitrainFolder
		private var coordinator = RemoteCoordinator.shared
		@State private var confirmSwitchBack = false
		@State private var confirmSwitch = false
		@State private var isWorking = false
		/// The outcome of the last switch, shown until dismissed
		@State private var message: Message? = nil

		struct Message: Equatable {
			var text: String
			var isError: Bool
		}

		init(folder: SushitrainFolder) {
			self.folder = folder
		}

		private var folderID: String { folder.folderID }

		var body: some View {
			if let binding = coordinator.binding(folderID: folderID), let serviceID = binding.serviceID {
				remoteSection(binding: binding, serviceID: serviceID)
				RemoteOperationsSection(folderID: folderID)
			}
			else if let migration = coordinator.migrations[folderID] {
				Section("Remote access") {
					RemoteMigrationRow(migration: migration)
					messageRows
				}
			}
			else if coordinator.remoteManagedFolderIDs.contains(folderID) {
				Section("Remote access") {
					Text(
						"Syncthing on this device leaves this folder to a gateway, but the folder is not opened through one."
					)
					switchBackButton
					messageRows
				}
			}
			else if let serving = coordinator.gatewayServing(folder: folder) {
				switchSection(gateway: serving.status)
			}
			else if message != nil {
				Section("Remote access") {
					messageRows
				}
			}
		}

		@ViewBuilder private var messageRows: some View {
			if let message {
				Text(message.text).foregroundStyle(message.isError ? Color.red : Color.primary)
				Button("OK") {
					self.message = nil
				}
			}
		}

		@ViewBuilder private func remoteSection(binding: RemoteFolderBinding, serviceID: String) -> some View {
			let status = coordinator.status(serviceID: serviceID)
			let migration = coordinator.migrations[folderID]
			Section {
				LabeledContent("Accessed through") {
					Text(status?.displayName ?? serviceID)
				}
				LabeledContent("Access") {
					Text(binding.permissions.write ? "Read and write" : (binding.permissions.read ? "Read only" : "None"))
				}
				if let migration {
					RemoteMigrationRow(migration: migration)
				}
				if let problem = status?.problem ?? status?.folderProblems[folderID] {
					Text(problem).foregroundStyle(.red)
				}
				if migration == nil {
					switchBackButton
				}
				messageRows
			} header: {
				Text("Remote access")
			} footer: {
				Text(
					"Syncthing on this device does not synchronize this folder. The Files app opens and saves its files through the gateway. The files that were synchronized on this device before are kept."
				)
			}
		}

		@ViewBuilder private var switchBackButton: some View {
			if isWorking {
				ProgressView()
			}
			else {
				Button("Switch Back to Syncthing…") {
					confirmSwitchBack = true
				}
				.confirmationDialog(
					"Switch back to Syncthing?", isPresented: $confirmSwitchBack, titleVisibility: .visible
				) {
					Button("Switch Back") {
						switchBack()
					}
				} message: {
					Text(
						"The folder is then synchronized by Syncthing on this device again, starting from the files on this device. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
			}
		}

		@ViewBuilder private func switchSection(gateway: RemoteGatewayStatus) -> some View {
			Section {
				if isWorking {
					ProgressView()
				}
				else {
					Button("Switch to Remote Access…") {
						confirmSwitch = true
					}
					.disabled(!gateway.isUsable)
					.confirmationDialog(
						"Access this folder through \(gateway.displayName)?", isPresented: $confirmSwitch,
						titleVisibility: .visible
					) {
						Button("Switch") {
							switchToGateway(serviceID: gateway.id)
						}
					} message: {
						Text(
							"Syncthing on this device stops synchronizing the folder, and the Files app opens and saves its files through the gateway instead. The files on this device are kept. Switching is refused while this device has changes the gateway does not have yet."
						)
					}
				}
				messageRows
			} header: {
				Text("Remote access")
			} footer: {
				Text(
					"\(gateway.displayName) serves this folder. Opening it through the gateway keeps it available in the Files app without synchronizing it on this device."
				)
			}
		}

		private func switchToGateway(serviceID: String) {
			isWorking = true
			message = nil
			Task {
				defer { isWorking = false }
				do throws(RemoteSwitchError) {
					try await coordinator.switchToGateway(folderID: folderID, serviceID: serviceID)
				}
				catch {
					message = Message(text: error.localizedDescription, isError: true)
				}
			}
		}

		private func switchBack() {
			isWorking = true
			message = nil
			Task {
				defer { isWorking = false }
				do throws(RemoteSwitchError) {
					if try await coordinator.switchToSyncthing(folderID: folderID) {
						message = Message(
							text: String(
								localized:
									"Switched back to Syncthing. The folder stays paused until you resume it; when it resumes, Syncthing scans the files on this device first."
							), isError: false)
					}
				}
				catch {
					message = Message(text: error.localizedDescription, isError: true)
				}
			}
		}
	}
#endif
