// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	/// Paired devices, with their access to the exported folders
	struct GatewayClientsSection: View {
		private var controller = GatewayController.shared

		var body: some View {
			Section {
				if controller.clients.isEmpty {
					Text("No devices are paired yet.")
						.foregroundStyle(.secondary)
				}
				ForEach(controller.clients) { client in
					GatewayClientRow(client: client)
				}
			} header: {
				Text("Paired devices")
			} footer: {
				if !controller.clients.isEmpty {
					Text(GatewayAccess.historyNote)
				}
			}
			.disabled(controller.state != .running)
		}
	}

	private struct GatewayClientRow: View {
		private var controller = GatewayController.shared
		let client: GatewayClient
		@State private var showRevokeConfirmation = false
		@State private var errorMessage: String? = nil

		init(client: GatewayClient) {
			self.client = client
		}

		private var exportedFolders: [GatewayFolder] {
			controller.folders.filter { controller.exports[$0.id] != nil }
		}

		var body: some View {
			DisclosureGroup {
				if exportedFolders.isEmpty {
					Text("Export folders to give this device access to them.")
						.foregroundStyle(.secondary)
				}
				ForEach(exportedFolders) { folder in
					GatewayAccessPicker(
						title: folder.name,
						maximum: GatewayAccess(export: controller.exports[folder.id]),
						selection: Binding(
							get: { client.access(to: folder.id) },
							set: { access in setAccess(access, folderID: folder.id) }))
				}
				HStack {
					Spacer()
					Button("Revoke…", role: .destructive) {
						showRevokeConfirmation = true
					}
				}
			} label: {
				VStack(alignment: .leading) {
					Text(client.displayName)
					Text(subtitle)
						.font(.caption)
						.foregroundStyle(.secondary)
				}
			}
			.confirmationDialog(
				"Revoke \(client.displayName)?", isPresented: $showRevokeConfirmation, titleVisibility: .visible
			) {
				Button("Revoke", role: .destructive) {
					revoke()
				}
			} message: {
				Text(
					"The device can no longer use this Mac's folders. Changes it has not sent yet stay on the device. To use the folders again, pair the device again."
				)
			}
			.alert(
				"Could not change the device's access", isPresented: Binding.isNotNil($errorMessage),
				actions: {
					Button("OK") {}
				},
				message: {
					Text(errorMessage ?? "")
				})
		}

		private var subtitle: String {
			let seen =
				if let lastSeen = client.lastSeenDate {
					String(localized: "Last seen \(lastSeen.formatted(.relative(presentation: .named)))")
				}
				else {
					String(localized: "Never connected")
				}
			return client.platform.isEmpty ? seen : "\(client.platform) · \(seen)"
		}

		private func setAccess(_ access: GatewayAccess, folderID: String) {
			let clientID = client.clientID
			Task {
				do {
					try await controller.setAccess(clientID: clientID, folderID: folderID, access: access)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func revoke() {
			let clientID = client.clientID
			Task {
				do {
					try await controller.revoke(clientID: clientID)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}

	/// Chooses a device's access to a folder, up to what the folder's export allows
	struct GatewayAccessPicker: View {
		let title: String
		let maximum: GatewayAccess
		@Binding var selection: GatewayAccess

		var body: some View {
			Picker(selection: $selection) {
				Text(GatewayAccess.none.label).tag(GatewayAccess.none)
				Text(GatewayAccess.read.label).tag(GatewayAccess.read)
				// Keep a grant that is no longer allowed visible, so the picker shows what the device has
				if maximum == .write || selection == .write {
					Text(GatewayAccess.write.label).tag(GatewayAccess.write)
				}
			} label: {
				Text(title)
			}
		}
	}

	/// Changes from paired devices that are being saved
	struct GatewayPendingOperationsSection: View {
		private var controller = GatewayController.shared

		var body: some View {
			if !controller.pendingOperations.isEmpty {
				Section {
					ForEach(controller.pendingOperations) { operation in
						LabeledContent {
							Text(operation.created, format: .relative(presentation: .named))
						} label: {
							Text(operation.kindLabel)
							Text(detail(operation))
						}
					}
				} header: {
					Text("Changes being saved")
				} footer: {
					Text(
						"Changes from paired devices that are not complete yet. Changes interrupted by quitting Synctrain are completed when the service starts again."
					)
				}
			}
		}

		private func detail(_ operation: GatewayPendingOperation) -> String {
			let folder = controller.folders.first { $0.id == operation.folderID }?.name ?? operation.folderID
			let client = controller.clients.first { $0.clientID == operation.clientID }?.displayName
			guard let client else {
				return folder
			}
			return "\(folder) · \(client)"
		}
	}
#endif
