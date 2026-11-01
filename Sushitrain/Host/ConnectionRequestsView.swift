// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// Devices that tried to connect without being known here, which the engine turned away for now
	struct ConnectionRequestsView: View {
		@Environment(AppState.self) private var appState

		@State private var addingDeviceID = ""
		@State private var showingAddDevice = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Group {
				if appState.pendingDevices.isEmpty {
					ContentUnavailableView(
						"No connection requests", systemImage: "person.crop.circle.badge.checkmark",
						description: Text("A device that tries to connect from elsewhere shows here, for you to add or ignore.")
					)
				}
				else {
					Form {
						Section {
							ForEach(appState.pendingDevices, id: \.deviceID) { device in
								self.row(device)
							}
						} footer: {
							Text(
								"A device names itself, so check its ID before you add it. Dismissing forgets the request, until the device tries again. Ignoring turns it away from now on, which you can undo in the advanced settings."
							)
						}
					}
					.formStyle(.grouped)
				}
			}
			.frame(minWidth: 520, minHeight: 280)
			.navigationTitle("Connection requests")
			.sheet(isPresented: $showingAddDevice) {
				AddDeviceView(suggestedDeviceID: $addingDeviceID, shown: $showingAddDevice)
			}
			.errorAlert($errorMessage)
		}

		private func row(_ device: SushitrainObservedDevice) -> some View {
			VStack(alignment: .leading, spacing: 6) {
				Text(device.name.isEmpty ? String(localized: "Unnamed device") : device.name)
					.bold()
				Text(verbatim: device.deviceID)
					.font(.caption.monospaced())
					.textSelection(.enabled)
				if !device.address.isEmpty {
					Text(verbatim: device.address)
						.font(.caption)
						.foregroundStyle(.secondary)
				}
				if let time = device.time()?.date() {
					Text(time.formatted())
						.font(.caption)
						.foregroundStyle(.secondary)
				}
				HStack {
					Button("Add device...") {
						addingDeviceID = device.deviceID
						showingAddDevice = true
					}
					Button("Dismiss") {
						self.act(on: device) { client, deviceID in try client.dismissPendingDevice(deviceID) }
					}
					Button("Ignore", role: .destructive) {
						self.act(on: device) { client, deviceID in try client.ignoreDevice(deviceID) }
					}
				}
			}
		}

		private func act(
			on device: SushitrainObservedDevice, action: @Sendable @escaping (SushitrainClient, String) throws -> Void
		) {
			let client = appState.client
			let deviceID = device.deviceID
			Task {
				do {
					try await goBlocking { try action(client, deviceID) }
					await appState.updatePendingDevices()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
		}
	}

	/// Devices that are turned away, which can be allowed to ask again
	struct IgnoredDevicesView: View {
		@Environment(AppState.self) private var appState

		@State private var devices: [SushitrainObservedDevice] = []
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Group {
				if devices.isEmpty {
					ContentUnavailableView(
						"No ignored devices", systemImage: "person.crop.circle.badge.xmark",
						description: Text("Devices whose connection requests you ignore show here.")
					)
				}
				else {
					Form {
						Section {
							ForEach(devices, id: \.deviceID) { device in
								LabeledContent {
									Button("Stop ignoring") {
										self.unignore(device)
									}
								} label: {
									Text(device.name.isEmpty ? String(localized: "Unnamed device") : device.name)
									Text(verbatim: device.deviceID).font(.caption.monospaced())
								}
							}
						} footer: {
							Text("Once you stop ignoring a device, it shows as a connection request the next time it tries.")
						}
					}
					.formStyle(.grouped)
				}
			}
			.navigationTitle("Ignored devices")
			.task(id: appState.eventCounter) {
				await self.update()
			}
			.errorAlert($errorMessage)
		}

		/// Copies the whole configuration, so not on the main thread, at every change while this page is open
		private func update() async {
			let client = appState.client
			let devices: [SushitrainObservedDevice] = await Task.detached {
				let list = client.ignoredDevices()
				return (0..<(list?.count() ?? 0)).compactMap { list?.item(at: $0) }
			}.value
			if !Task.isCancelled {
				self.devices = devices
			}
		}

		private func unignore(_ device: SushitrainObservedDevice) {
			let client = appState.client
			let deviceID = device.deviceID
			Task {
				do {
					try await goBlocking { try client.unignoreDevice(deviceID) }
					await self.update()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
		}
	}
#endif
