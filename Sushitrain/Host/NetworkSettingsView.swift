// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit

struct NetworkSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings
	@State private var showListeningAddresses = false
	@State private var showDiscoveryAddresses = false
	@State private var showSTUNAddresses = false
	@State private var updateCounter = 0

	var body: some View {
		let _ = updateCounter
		Form {
			Section {
				Toggle(
					"Listen for incoming connections",
					isOn: Binding(
						get: {
							return appState.client.isListening()
						},
						set: { listening in
							try? appState.client.setListening(listening)
							self.updateCounter += 1
						}))

				// Listening addresses popup sheet
				Button("Listening addresses...", systemImage: "envelope.front") {
					showListeningAddresses = true
				}
				.disabled(!appState.client.isListening())
				.buttonStyle(.link)
				.sheet(isPresented: $showListeningAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .listening)
							.navigationTitle("Listening addresses")
							.toolbar {
								SheetButton(role: .save) {
									showListeningAddresses = false
								}
							}
					}
				}
			} header: {
				Text("Connectivity")
			} footer: {
				if appState.client.isListening() {
					Text(
						"Added devices can connect to this device on their initiative, while the app is running. This may cause additional battery drain. It is advisable to enable this only if you experience difficulty connecting to other devices."
					)
				}
				else {
					Text(
						"Connections to other devices can only be initiated by this device, not by the other added devices."
					)
				}
			}

			Section {
				Toggle(
					"One connection is enough",
					isOn: Binding(
						get: {
							return appState.client.getEnoughConnections() == 1
						},
						set: { enough in
							// Fails while the engine restarts
							do {
								try appState.client.setEnoughConnections(enough ? 1 : 0)
							}
							catch {
								Log.warn("Could not change the connection setting: \(error.localizedDescription)")
							}
						}))
			} footer: {
				Text(
					"When this setting is enabled, the app will not attempt to connect to more devices after one connection has been established."
				)
			}

			Section("Discovery") {
				Toggle(
					"Announce on local networks",
					isOn: Binding(
						get: {
							return appState.client.isLocalAnnounceEnabled()
						},
						set: { nv in
							try? appState.client.setLocalAnnounceEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Announce LAN addresses",
					isOn: Binding(
						get: {
							return appState.client.isAnnounceLANAddressesEnabled()
						},
						set: { nv in
							try? appState.client.setAnnounceLANAddresses(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Announce globally",
					isOn: Binding(
						get: {
							return appState.client.isGlobalAnnounceEnabled()
						},
						set: { nv in
							try? appState.client.setGlobalAnnounceEnabled(nv)
							self.updateCounter += 1
						}))

				// Global announce addresses popup sheet
				Button("Global announce servers...", systemImage: "megaphone.fill") {
					showDiscoveryAddresses = true
				}
				.sheet(isPresented: $showDiscoveryAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .discovery)
							.navigationTitle("Global announce servers")
							.toolbar {
								SheetButton(role: .save) {
									showDiscoveryAddresses = false
								}
							}
					}
				}
				.disabled(!appState.client.isGlobalAnnounceEnabled())
				.buttonStyle(.link)
			}

			Section("Network traversal") {
				Toggle(
					"Enable relaying",
					isOn: Binding(
						get: {
							return appState.client.isRelaysEnabled()
						},
						set: { nv in
							try? appState.client.setRelaysEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Enable NAT-PMP / UPnP",
					isOn: Binding(
						get: {
							return appState.client.isNATEnabled()
						},
						set: { nv in
							try? appState.client.setNATEnabled(nv)
							self.updateCounter += 1
						}))

				Toggle(
					"Enable STUN",
					isOn: Binding(
						get: {
							return appState.client.isSTUNEnabled()
						},
						set: { nv in
							try? appState.client.setSTUNEnabled(nv)
							self.updateCounter += 1
						}))

				// STUN server addresses popup sheet
				Button("STUN Servers...", systemImage: "arrow.trianglehead.swap") {
					showSTUNAddresses = true
				}
				.sheet(isPresented: $showSTUNAddresses) {
					NavigationStack {
						AsyncAddressesView(addressType: .stun)
							.navigationTitle("STUN servers")
							.toolbar {
								SheetButton(role: .save) {
									showSTUNAddresses = false
								}
							}
					}
				}
				.disabled(!appState.client.isSTUNEnabled())
				.buttonStyle(.link)
			}

			Section {
				NavigationLink("Connection options") {
					ConnectionOptionsView()
				}
			} footer: {
				Text("Limits, priorities, reconnecting, local discovery and port mapping.")
			}

			Section {
				NavigationLink("Proxy") {
					ProxySettingsView()
				}
			} footer: {
				Text("A SOCKS5 or HTTP proxy for the engine's outgoing connections.")
			}

			Section {
				NavigationLink("Ignored devices") {
					IgnoredDevicesView()
				}
			} footer: {
				Text("Devices whose requests to connect you turned away.")
			}

			Section {
				if deviceHasCellularCapability() {
					Toggle("On cellular networks", isOn: userSettings.$disableDevicesOnCellular)
				}

				Toggle("On metered networks", isOn: userSettings.$disableDevicesOnMetered)
				Toggle("In low power mode", isOn: userSettings.$disableDevicesInLowPowerMode)
			} header: {
				Text("Limit connectivity")
			} footer: {
				Text(
					"When the app detects that the system's internet connection is of the selected types, it will temporarily disable network connections to other devices. This will cause synchronization to be paused, and files that are not on this device yet cannot be streamed."
				)
			}
		}
		.formStyle(.grouped)
	}
}
