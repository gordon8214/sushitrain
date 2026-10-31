// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
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
						"Forgetting a gateway removes its credential from this device. Folders accessed through it have to be removed first."
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
					do throws(GatewayFolderError) {
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

/// The folders a gateway lets this device access: adding them to this device, showing them in Files, and removing them
struct RemoteGatewayFoldersSection: View {
	let serviceID: String
	private var coordinator = RemoteCoordinator.shared
	@State private var confirmRemoval: Removal? = nil
	@State private var errorMessage: String? = nil
	@State private var working: Set<String> = []

	/// Removing a folder from this device, with how many changes in Files would be lost
	struct Removal: Identifiable {
		var folder: RemoteFolder
		var localID: String
		var pendingItemCount: Int
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
			Text(status?.displayName ?? String(localized: "Folders"))
		} footer: {
			Text(
				"Folders are not synchronized to this device: files are downloaded from the gateway when they are opened, and edits are saved through it."
			)
		}
		.confirmationDialog(
			"Remove the folder from this device?", isPresented: Binding.isNotNil($confirmRemoval),
			titleVisibility: .visible, presenting: confirmRemoval
		) { removal in
			Button(removal.pendingItemCount > 0 ? "Remove Anyway" : "Remove", role: .destructive) {
				remove(removal)
			}
		} message: { removal in
			#if os(macOS)
				if removal.pendingItemCount > 0 {
					Text(
						"Changes to \(removal.pendingItemCount) items made in Finder have not reached Synctrain yet. Removing the folder discards them."
					)
				}
				else {
					Text(
						"The folder leaves this Mac and Finder; it stays on the gateway. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
			#else
				if removal.pendingItemCount > 0 {
					Text(
						"Changes to \(removal.pendingItemCount) items made in the Files app have not reached Synctrain yet. Removing the folder discards them."
					)
				}
				else {
					Text(
						"The folder leaves this device and the Files app; it stays on the gateway. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
			#endif
		}
		.alert(
			"Could not change the folder", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage
		) { _ in
			Button("OK") {}
		} message: { message in
			Text(message)
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
		if working.contains(folder.id) {
			ProgressView()
		}
		else if let localID = coordinator.localFolderID(serviceID: serviceID, remoteFolderID: folder.id) {
			Menu {
				#if os(macOS)
					Button("Show in Finder", systemImage: "folder") {
						showInFiles(folderID: localID)
					}
				#else
					Button("Show in Files", systemImage: "folder") {
						showInFiles(folderID: localID)
					}
				#endif
				Button("Remove from This Device…", systemImage: "minus.circle", role: .destructive) {
					askToRemove(folder, localID: localID)
				}
			} label: {
				Label("Added", systemImage: "checkmark")
			}
		}
		else if !folder.permissions.read {
			Text("No access").foregroundStyle(.secondary)
		}
		else {
			Button("Add") {
				add(folder)
			}
		}
	}

	private func add(_ folder: RemoteFolder) {
		working.insert(folder.id)
		Task {
			defer { working.remove(folder.id) }
			do throws(GatewayFolderError) {
				try await coordinator.addFolder(folder, serviceID: serviceID)
			}
			catch {
				errorMessage = error.localizedDescription
			}
		}
	}

	private func showInFiles(folderID: String) {
		Task {
			if let url = await FilesDomain.shared.userVisibleURL(folderID: folderID, path: "") {
				ClientLifecycle.show(url)
			}
			else {
				#if os(macOS)
					errorMessage = String(
						localized: "The folder is not shown in Finder yet. Is the Synctrain location turned on?")
				#else
					errorMessage = String(
						localized: "The folder is not shown in the Files app. Is the Synctrain location turned on?")
				#endif
			}
		}
	}

	private func askToRemove(_ folder: RemoteFolder, localID: String) {
		Task {
			let pending = await FilesDomain.shared.pendingItemCount()
			confirmRemoval = Removal(folder: folder, localID: localID, pendingItemCount: pending)
		}
	}

	private func remove(_ removal: Removal) {
		working.insert(removal.folder.id)
		Task {
			defer { working.remove(removal.folder.id) }
			do throws(GatewayFolderError) {
				try await coordinator.removeFolder(folderID: removal.localID)
			}
			catch {
				errorMessage = error.localizedDescription
			}
		}
	}
}
