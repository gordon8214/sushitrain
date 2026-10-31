// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	/// Settings › Remote file service: turn the service on, export folders, pair devices and manage them
	struct GatewaySettingsView: View {
		private var controller = GatewayController.shared
		@State private var showPairing = false

		var body: some View {
			Form {
				GatewayStatusSection(showPairing: $showPairing)

				if controller.isEnabled {
					GatewayExportsSection()
					GatewayClientsSection()
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
					isOn: Binding(get: { controller.isEnabled }, set: { controller.setEnabled($0) }))

				if controller.isEnabled {
					LabeledContent("Status") {
						stateView
					}

					if controller.state == .running, let status = controller.status {
						LabeledContent("Address") {
							Text(verbatim: "\(controller.settings.effectivePublicHost):\(controller.settings.port)")
								.textSelection(.enabled)
						}
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
				Text(
					"Lets Synctrain on your iPhone or iPad browse, open and edit folders of this Mac in the Files app, without keeping copies of the folders on the device."
				)
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
#endif
