// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// A paired gateway: whether it can be used, the folders it serves, and forgetting it
	struct RemoteGatewayView: View {
		let serviceID: String
		private var coordinator = RemoteCoordinator.shared
		@Environment(\.dismiss) private var dismiss
		@State private var showForgetConfirmation = false
		@State private var showPairing = false
		@State private var errorMessage: String? = nil

		init(serviceID: String) {
			self.serviceID = serviceID
		}

		var body: some View {
			Form {
				if let status = coordinator.status(serviceID: serviceID) {
					RemoteGatewayStatusSection(status: status) {
						showPairing = true
					}
					RemoteGatewayFoldersSection(serviceID: serviceID)

					Section {
						Button("Forget This Gateway", role: .destructive) {
							showForgetConfirmation = true
						}
					} footer: {
						Text(
							"Forgetting a gateway removes its credential from this device. Folders accessed through it have to be switched back to Syncthing or removed from Files first."
						)
					}
				}
				else {
					ContentUnavailableView("Not paired", systemImage: "externaldrive.badge.xmark")
				}
			}
			.navigationTitle(coordinator.status(serviceID: serviceID)?.displayName ?? String(localized: "Gateway"))
			.refreshable {
				await coordinator.refresh(serviceID: serviceID)
			}
			.task {
				await coordinator.refresh(serviceID: serviceID)
			}
			.sheet(isPresented: $showPairing) {
				RemotePairingView()
			}
			.confirmationDialog(
				"Forget this gateway?", isPresented: $showForgetConfirmation, titleVisibility: .visible
			) {
				Button("Forget", role: .destructive) {
					Task {
						do throws(RemoteSwitchError) {
							try await coordinator.forgetGateway(serviceID: serviceID)
							dismiss()
						}
						catch {
							errorMessage = error.localizedDescription
						}
					}
				}
			}
			.alert(
				"Could not forget the gateway", isPresented: Binding.isNotNil($errorMessage),
				presenting: errorMessage
			) { _ in
				Button("OK") {}
			} message: { message in
				Text(message)
			}
		}
	}

	/// Whether the gateway can be reached, accepts this device and speaks this app's API version
	private struct RemoteGatewayStatusSection: View {
		let status: RemoteGatewayStatus
		let pairAgain: () -> Void
		private var coordinator = RemoteCoordinator.shared

		init(status: RemoteGatewayStatus, pairAgain: @escaping () -> Void) {
			self.status = status
			self.pairAgain = pairAgain
		}

		var body: some View {
			Section {
				LabeledContent("Address") {
					Text(verbatim: status.address).textSelection(.enabled)
				}
				LabeledContent("Certificate") {
					if let pin = status.endpoint.pinnedSPKIHash {
						Text("Pinned key \(String(pin.prefix(12)))…").monospaced()
					}
					else {
						Text("Trusted certificate")
					}
				}
				check("Reachable", status.isReachable)
				check("Authorized", status.isAuthorized)
				check("API compatible", status.isCompatible)
				if let generation = status.capabilities?.authorityGeneration {
					LabeledContent("Authority generation", value: generation, format: .number)
				}
				if let checked = status.lastChecked {
					LabeledContent("Last checked") {
						Text(checked, format: .relative(presentation: .named))
					}
				}
				if let problem = status.problem {
					Text(problem).foregroundStyle(.red)
				}
				if status.connection == .unauthorized || status.connection == .untrusted {
					Button("Pair Again…", action: pairAgain)
				}
			} header: {
				Text("Connection")
			}

			if status.authorityChangedAt != nil {
				Section {
					Label(
						"\(status.displayName) may have lost recent changes: its database was reset or restored, or another device became the folders' authority. Edits it could not confirm are listed for review.",
						systemImage: "exclamationmark.triangle"
					)
					.foregroundStyle(.orange)
					Button("OK") {
						coordinator.acknowledgeAuthorityChange(serviceID: status.id)
					}
				}
			}
		}

		@ViewBuilder private func check(_ title: LocalizedStringKey, _ value: Bool?) -> some View {
			LabeledContent(title) {
				switch value {
				case .some(true):
					Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
						.accessibilityLabel("Yes")
				case .some(false):
					Image(systemName: "xmark.circle.fill").foregroundStyle(.red)
						.accessibilityLabel("No")
				case .none:
					Text("Unknown").foregroundStyle(.secondary)
				}
			}
		}
	}

	/// The folders a gateway lets this device access, each with what can be done with it here: open it in Files, switch
	/// this device's Syncthing folder to it, or see that it is in Files already
	struct RemoteGatewayFoldersSection: View {
		let serviceID: String
		private var coordinator = RemoteCoordinator.shared
		@State private var confirmSwitch: RemoteFolder? = nil
		@State private var confirmRemoval: Removal? = nil
		@State private var errorMessage: String? = nil
		@State private var notice: String? = nil
		@State private var working: Set<String> = []

		/// Ending access to a folder through the gateway: a Syncthing folder of this device takes over, or (for a folder
		/// that is only on the gateway) it leaves the Files app
		struct Removal: Identifiable {
			var folder: RemoteFolder
			var localID: String
			var hasSyncthingFolder: Bool
			var id: String { localID }
		}

		init(serviceID: String) {
			self.serviceID = serviceID
		}

		var body: some View {
			let status = coordinator.status(serviceID: serviceID)
			Section {
				if let folders = status?.folders {
					if folders.isEmpty {
						Text("The gateway does not let this device access any folder. Grant access on the gateway.")
							.foregroundStyle(.secondary)
					}
					ForEach(folders, id: \.id) { folder in
						row(folder)
					}
				}
				else if status?.isUsable == false {
					Text("The folders are listed once the gateway can be reached.").foregroundStyle(.secondary)
				}
				else {
					ProgressView()
				}
			} header: {
				Text("Folders")
			} footer: {
				Text(
					"Folders opened through a gateway are not synchronized on this device: the Files app downloads files from the gateway when they are opened, and saves edits through it."
				)
			}
			.confirmationDialog(
				"Switch to remote access?", isPresented: Binding.isNotNil($confirmSwitch), titleVisibility: .visible,
				presenting: confirmSwitch
			) { folder in
				Button("Switch \(folder.label.isEmpty ? folder.id : folder.label)") {
					switchFolder(folder)
				}
			} message: { _ in
				Text(
					"Syncthing on this device stops synchronizing the folder, and the Files app opens and saves its files through the gateway instead. The files on this device are kept. Switching is refused while this device has changes the gateway does not have yet."
				)
			}
			.confirmationDialog(
				"Stop accessing the folder through the gateway?", isPresented: Binding.isNotNil($confirmRemoval),
				titleVisibility: .visible, presenting: confirmRemoval
			) { removal in
				if removal.hasSyncthingFolder {
					Button("Switch Back to Syncthing") {
						remove(removal)
					}
				}
				else {
					Button("Remove from Files", role: .destructive) {
						remove(removal)
					}
				}
			} message: { removal in
				if removal.hasSyncthingFolder {
					Text(
						"The folder is then synchronized by Syncthing on this device again, starting from the files on this device. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
				else {
					Text(
						"The folder leaves the Files app on this device; it stays on the gateway. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
			}
			.alert(
				"Could not change the folder", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage
			) { _ in
				Button("OK") {}
			} message: { message in
				Text(message)
			}
			.alert("Switched back to Syncthing", isPresented: Binding.isNotNil($notice), presenting: notice) { _ in
				Button("OK") {}
			} message: { notice in
				Text(notice)
			}
		}

		@ViewBuilder private func row(_ folder: RemoteFolder) -> some View {
			LabeledContent {
				action(folder)
			} label: {
				VStack(alignment: .leading) {
					Text(folder.label.isEmpty ? folder.id : folder.label)
					Text(folder.permissions.write ? "Read and write" : "Read only")
						.font(.caption)
						.foregroundStyle(.secondary)
					if let localID = coordinator.localFolderID(serviceID: serviceID, remoteFolderID: folder.id),
						let problem = coordinator.status(serviceID: serviceID)?.folderProblems[localID]
					{
						Text(problem).font(.caption).foregroundStyle(.red)
					}
				}
			}
		}

		@ViewBuilder private func action(_ folder: RemoteFolder) -> some View {
			let key = folder.id
			if working.contains(key) {
				ProgressView()
			}
			else if let localID = coordinator.localFolderID(serviceID: serviceID, remoteFolderID: folder.id) {
				if coordinator.migrations[localID] != nil {
					Text("Switching…").foregroundStyle(.secondary)
				}
				else {
					let hasSyncthingFolder = coordinator.syncthingFolder(id: localID) != nil
					Menu {
						Button(hasSyncthingFolder ? "Switch Back to Syncthing…" : "Remove from Files…") {
							confirmRemoval = Removal(
								folder: folder, localID: localID, hasSyncthingFolder: hasSyncthingFolder)
						}
					} label: {
						Label("In Files", systemImage: "checkmark")
					}
				}
			}
			else if coordinator.migrations[folder.id] != nil || coordinator.switchingFolders.contains(folder.id)
				|| (coordinator.remoteManagedFolderIDs.contains(folder.id) && coordinator.binding(folderID: folder.id) == nil)
			{
				// This device's folder of the same ID is being switched to the gateway: it is not bound yet, and adding the
				// folder now would add it a second time
				Text("Switching…").foregroundStyle(.secondary)
			}
			else if !folder.permissions.read {
				Text("No access").foregroundStyle(.secondary)
			}
			else if let local = coordinator.syncthingFolder(id: folder.id),
				coordinator.gatewayServing(folder: local, serviceID: serviceID) != nil
			{
				Button("Switch…") {
					confirmSwitch = folder
				}
			}
			else {
				Button("Add to Files") {
					add(folder)
				}
			}
		}

		private func add(_ folder: RemoteFolder) {
			working.insert(folder.id)
			Task {
				defer { working.remove(folder.id) }
				do throws(RemoteSwitchError) {
					try await coordinator.addGatewayFolder(folder, serviceID: serviceID)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func remove(_ removal: Removal) {
			working.insert(removal.folder.id)
			Task {
				defer { working.remove(removal.folder.id) }
				do throws(RemoteSwitchError) {
					if try await coordinator.switchToSyncthing(folderID: removal.localID) {
						notice = String(
							localized:
								"The folder stays paused until you resume it; when it resumes, Syncthing scans the files on this device first."
						)
					}
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func switchFolder(_ folder: RemoteFolder) {
			working.insert(folder.id)
			Task {
				defer { working.remove(folder.id) }
				do throws(RemoteSwitchError) {
					try await coordinator.switchToGateway(folderID: folder.id, serviceID: serviceID)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}
#endif
