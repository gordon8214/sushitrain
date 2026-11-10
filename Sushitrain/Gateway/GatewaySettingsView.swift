// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// Settings › Remote file service: turn the service on, export folders, pair devices and manage them
	struct GatewaySettingsView: View {
		private var controller = GatewayController.shared
		@State private var showPairing = false

		var body: some View {
			Form {
				GatewayStatusSection(showPairing: $showPairing)

				if controller.isEnabled {
					GatewayExportsSection().settingsAnchor("gateway.exports")
					GatewayClientsSection().settingsAnchor("gateway.clients")
					GatewayPendingOperationsSection()
				}

				GatewayConnectionSection()

				Section {
					AutoStartToggleView()
				} footer: {
					Text(
						"Paired devices can only reach this Mac while Synctrain is running and the Mac is awake. Launch Synctrain at login so that the service is available after a restart; hide the dock icon to keep it running without a window."
					)
				}
			}
			.formStyle(.grouped)
			.task {
				await controller.refreshWhileVisible()
			}
			.sheet(isPresented: $showPairing) {
				GatewayPairingView()
			}
		}
	}

	/// The on/off switch and what the service is doing
	private struct GatewayStatusSection: View {
		private var controller = GatewayController.shared
		@Binding var showPairing: Bool

		init(showPairing: Binding<Bool>) {
			self._showPairing = showPairing
		}

		var body: some View {
			Section {
				Toggle(
					"Remote file service",
					isOn: Binding(get: { controller.isEnabled }, set: { controller.setEnabled($0) })
				).settingsAnchor("gateway.enabled")

				if controller.isEnabled {
					LabeledContent("Status") {
						stateView
					}
				}

				GatewayAddressField()

				if controller.isEnabled {
					if controller.state == .running, let status = controller.status {
						if controller.settings.certificateMode == .selfSigned, let spki = status.spki, !spki.isEmpty {
							LabeledContent("Certificate key (SHA-256)") {
								Text(verbatim: spki)
									.monospaced()
									.textSelection(.enabled)
									.multilineTextAlignment(.trailing)
							}
						}
						LabeledContent("Paired devices", value: controller.clients.count, format: .number)
						LabeledContent("Changes being saved", value: status.pendingOperations, format: .number)
						LabeledContent("Content cache") {
							Text("\(bytes(status.contentCacheBytes)) of \(bytes(status.contentCacheQuota))")
						}
						if status.pendingPairings > 0 {
							LabeledContent {
								Button("Review…") {
									showPairing = true
								}
							} label: {
								Text("Devices waiting for approval: \(status.pendingPairings)")
							}
						}
					}

					if let error = controller.refreshError {
						Text(error).foregroundStyle(.red)
					}

					if controller.state == .running {
						Button("Pair a Device…") {
							showPairing = true
						}
					}
					else if case .failed = controller.state {
						Button("Try Again") {
							controller.restart()
						}
					}
				}
			} footer: {
				VStack(alignment: .leading, spacing: 6) {
					Text(
						"Lets Synctrain on your iPhone or iPad browse, open and edit folders of this Mac in the Files app, without keeping copies of the folders on the device."
					)
					Text(
						"Devices connect to the address in their pairing code. This Mac's local name only works on the same network: to connect from elsewhere, enter a name that reaches this Mac from there, through a VPN or a port your router forwards. Devices paired already keep their address; change it on the gateway's page in their settings."
					)
				}
			}
		}

		private func bytes(_ count: Int64) -> String {
			count.formatted(.byteCount(style: .memory))
		}

		@ViewBuilder private var stateView: some View {
			switch controller.state {
			case .stopped:
				Text("Stopped")
			case .starting:
				HStack {
					ProgressView().controlSize(.small)
					Text("Starting…")
				}
			case .running:
				Label("Running", systemImage: "checkmark.circle.fill")
					.foregroundStyle(.green)
			case .failed(let message):
				Text("Could not start: \(message)")
					.foregroundStyle(.red)
					.multilineTextAlignment(.trailing)
			}
		}
	}

	/// Where devices connect, for pairing codes: this Mac's local name and the listening port, unless the user enters
	/// an address that reaches this Mac from elsewhere (`host` or `host:port`). A running service takes it at once.
	private struct GatewayAddressField: View {
		private var controller = GatewayController.shared
		@State private var text = GatewayController.shared.settings.customAddress

		/// The settings with the address entered, nil when it is not an address
		private var edited: GatewaySettings? {
			controller.settings.withAddress(text)
		}

		private var hasChanges: Bool {
			edited != controller.settings
		}

		var body: some View {
			LabeledContent("Address") {
				HStack {
					TextField("Address", text: $text, prompt: Text(verbatim: defaultAddress))
						.labelsHidden()
						.multilineTextAlignment(.trailing)
						.autocorrectionDisabled()
						.onSubmit(save)
						.onExitCommand {
							text = controller.settings.customAddress
						}
					if hasChanges {
						Button("Save", action: save)
							.disabled(edited == nil)
					}
				}
			}
			.settingsAnchor("gateway.host")
			.settingsEditor(hasChanges: hasChanges)
			.onChange(of: controller.settings.customAddress) { previous, address in
				// Saved elsewhere (the port, or an automation): follow it unless the user is editing
				if text == previous {
					text = address
				}
			}
			if edited == nil {
				Text("Enter a host name or IP address, optionally followed by a colon and a port.")
					.foregroundStyle(.red)
			}
		}

		/// This Mac's local name and the listening port
		private var defaultAddress: String {
			RemoteAddress.format(host: GatewaySettings.localHostName, port: controller.settings.port)
		}

		private func save() {
			guard let edited, edited != controller.settings else {
				return
			}
			controller.apply(edited)
			text = controller.settings.customAddress
		}
	}
#endif
