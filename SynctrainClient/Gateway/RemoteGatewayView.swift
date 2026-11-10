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
		#if os(macOS)
			.formStyle(.grouped)
		#endif
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
			RemoteGatewayAddressField(status: status)
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
		} footer: {
			if status.endpoint.acceptedSPKIHashes.isEmpty {
				Text(
					"The address comes from the pairing code. This gateway is trusted by the name in its certificate, so only its port can be changed here: for another name, pair with it again from a pairing code with that name."
				)
			}
			else {
				Text(
					"The address comes from the pairing code. If another name reaches the gateway from elsewhere, such as one set up for its router or VPN, enter it here: the pairing stays as it is."
				)
			}
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

/// Where this device reaches the gateway (`host` or `host:port`). Saving keeps the pairing and checks the gateway again.
private struct RemoteGatewayAddressField: View {
	let status: RemoteGatewayStatus
	private var coordinator = RemoteCoordinator.shared
	@State private var text: String
	@State private var isSaving = false
	@State private var errorMessage: String? = nil

	init(status: RemoteGatewayStatus) {
		self.status = status
		_text = State(initialValue: status.address)
	}

	/// The address entered, nil when it is not one
	private var address: RemoteAddress? {
		RemoteAddress(parsing: text)
	}

	private var hasChanges: Bool {
		guard let address else {
			return true
		}
		return address.host != status.endpoint.host || (address.port ?? status.endpoint.port) != status.endpoint.port
	}

	var body: some View {
		LabeledContent("Address") {
			TextField("Address", text: $text, prompt: Text(verbatim: status.address))
				.labelsHidden()
				.multilineTextAlignment(.trailing)
				.autocorrectionDisabled()
				#if os(iOS)
					.textInputAutocapitalization(.never)
					.keyboardType(.URL)
				#else
					.onExitCommand {
						text = status.address
					}
				#endif
				.onSubmit(save)
		}
		#if os(macOS)
			.settingsEditor(hasChanges: hasChanges, isSaving: isSaving)
		#endif
		.onChange(of: status.address) { previous, address in
			if text == previous {
				text = address
			}
		}
		.alert(
			"Could not change the address", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage
		) { _ in
			Button("OK") {}
		} message: { message in
			Text(message)
		}

		// Not while the field is empty, as when its text was just selected and deleted to type another
		if address == nil, !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
			Text("Enter a host name or IP address, optionally followed by a colon and a port.")
				.foregroundStyle(.red)
		}
		if hasChanges {
			Button("Save Address", action: save)
				.disabled(address == nil || isSaving)
		}
	}

	private func save() {
		guard let address, hasChanges, !isSaving else {
			return
		}
		isSaving = true
		Task {
			defer { isSaving = false }
			do throws(GatewayFolderError) {
				try await coordinator.changeAddress(serviceID: status.id, to: address)
				text = coordinator.status(serviceID: status.id)?.address ?? text
			}
			catch {
				errorMessage = error.localizedDescription
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

	/// Removing a folder from this device, with how many changes in Files would be lost (nil if the system could not say)
	struct Removal: Identifiable {
		var remoteID: String
		var localID: String
		var pendingItemCount: Int?
		var id: String { localID }
	}

	init(serviceID: String) {
		self.serviceID = serviceID
	}

	var body: some View {
		let status = coordinator.status(serviceID: serviceID)
		let unlisted = unlistedFolders(status)
		Section {
			if let folders = status?.folders {
				if folders.isEmpty && unlisted.isEmpty {
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
			ForEach(unlisted, id: \.folderID) { binding in
				row(binding)
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
			Button(removal.pendingItemCount == 0 ? "Remove" : "Remove Anyway", role: .destructive) {
				remove(removal)
			}
		} message: { removal in
			#if os(macOS)
				if let count = removal.pendingItemCount, count > 0 {
					Text(
						"Changes to \(count) items made in Finder have not reached Synctrain yet. Removing the folder discards them."
					)
				}
				else if removal.pendingItemCount == nil {
					Text(
						"Synctrain could not find out whether changes made in Finder have reached it. Removing the folder discards any that have not."
					)
				}
				else {
					Text(
						"The folder leaves this Mac and Finder; it stays on the gateway. Edits that were not confirmed by the gateway have to be sent, exported or discarded first."
					)
				}
			#else
				if let count = removal.pendingItemCount, count > 0 {
					Text(
						"Changes to \(count) items made in the Files app have not reached Synctrain yet. Removing the folder discards them."
					)
				}
				else if removal.pendingItemCount == nil {
					Text(
						"Synctrain could not find out whether changes made in the Files app have reached it. Removing the folder discards any that have not."
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

	/// Folders on this device that the gateway doesn't list: it can't be reached, or no longer shares them with this
	/// device. Nothing else can take them away, and forgetting the gateway waits for them.
	private func unlistedFolders(_ status: RemoteGatewayStatus?) -> [RemoteFolderBinding] {
		let listed = Set(status?.folders?.map(\.id) ?? [])
		return coordinator.bindings.values.filter { $0.serviceID == serviceID && !listed.contains($0.remoteFolderID) }
			.sorted { $0.folderID < $1.folderID }
	}

	@ViewBuilder private func row(_ binding: RemoteFolderBinding) -> some View {
		LabeledContent {
			if working.contains(binding.remoteFolderID) {
				ProgressView()
			}
			else {
				addedMenu(remoteID: binding.remoteFolderID, localID: binding.folderID)
			}
		} label: {
			VStack(alignment: .leading) {
				Text(coordinator.folders.first { $0.folderID == binding.folderID }?.label ?? binding.remoteFolderID)
				if coordinator.status(serviceID: serviceID)?.folders != nil {
					Text("Not listed by the gateway").font(.caption).foregroundStyle(.secondary)
				}
			}
		}
	}

	@ViewBuilder private func addedMenu(remoteID: String, localID: String) -> some View {
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
				askToRemove(remoteID: remoteID, localID: localID)
			}
		} label: {
			Label("Added", systemImage: "checkmark")
		}
	}

	@ViewBuilder private func action(_ folder: RemoteFolder) -> some View {
		if working.contains(folder.id) {
			ProgressView()
		}
		else if let localID = coordinator.localFolderID(serviceID: serviceID, remoteFolderID: folder.id) {
			addedMenu(remoteID: folder.id, localID: localID)
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

	private func askToRemove(remoteID: String, localID: String) {
		Task {
			let pending = await FilesDomain.shared.pendingItemCount()
			confirmRemoval = Removal(remoteID: remoteID, localID: localID, pendingItemCount: pending)
		}
	}

	private func remove(_ removal: Removal) {
		working.insert(removal.remoteID)
		Task {
			defer { working.remove(removal.remoteID) }
			do throws(GatewayFolderError) {
				try await coordinator.removeFolder(folderID: removal.localID)
			}
			catch {
				errorMessage = error.localizedDescription
			}
		}
	}
}
