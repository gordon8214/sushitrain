// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// How the engine connects to other devices, for what has no setting of its own in the app
	struct ConnectionOptionsView: View {
		private typealias Draft = SettingsDraft<SushitrainConnectionOptions>

		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "Connection options", load: { client.connectionOptions() }, isEqual: { $0.equal($1) },
				save: { try client.setConnectionOptions($0) },
				content: { draft in
					AdvancedRestartSection()
					self.limitsSection(draft)
					self.prioritySection(draft)
					self.reconnectionSection(draft)
					self.localDiscoverySection(draft)
					self.natSection(draft)
					self.networksSection(draft)
					self.namesSection(draft)
				}
			)
		}

		private func limitsSection(_ draft: Binding<Draft>) -> some View {
			Section {
				SettingsNumberField(title: "Maximum connections", unit: "in total", text: draft.connectionLimitMax).settingsAnchor(
					"connectionLimitMax")
			} header: {
				Text("Limits")
			} footer: {
				Text("Neither accepts nor makes connections beyond this number; 0 means no limit.")
			}
		}

		private func prioritySection(_ draft: Binding<Draft>) -> some View {
			Section {
				TextField("TCP on the local network", text: draft.priorityTCPLAN).settingsAnchor("priorityTCPLAN")
				TextField("QUIC on the local network", text: draft.priorityQUICLAN).settingsAnchor("priorityQUICLAN")
				TextField("TCP over the internet", text: draft.priorityTCPWAN).settingsAnchor("priorityTCPWAN")
				TextField("QUIC over the internet", text: draft.priorityQUICWAN).settingsAnchor("priorityQUICWAN")
				TextField("Relays", text: draft.priorityRelay).settingsAnchor("priorityRelay")
				TextField("Upgrade threshold", text: draft.priorityUpgradeThreshold).settingsAnchor("priorityUpgradeThreshold")
			} header: {
				Text("Priorities")
			} footer: {
				Text(
					"Lower numbers win, and a device is only connected in the best way available. Over the internet has to stay above the local network, for TCP and QUIC each. A connection is replaced by a better one if that is more than the threshold better."
				)
			}
		}

		private func reconnectionSection(_ draft: Binding<Draft>) -> some View {
			Section {
				SettingsNumberField(title: "Reconnection interval", unit: "seconds", text: draft.reconnectionIntervalS)
					.settingsAnchor("reconnectionIntervalS")
				SettingsNumberField(title: "Relay reconnection interval", unit: "minutes", text: draft.relayReconnectIntervalM)
					.settingsAnchor("relayReconnectIntervalM")
			} header: {
				Text("Reconnecting")
			} footer: {
				Text(
					"How often to try devices that aren't connected, no more often than every 5 seconds, and to look for a better relay."
				)
			}
		}

		private func localDiscoverySection(_ draft: Binding<Draft>) -> some View {
			Section {
				TextField("Local announcement port", text: draft.localAnnouncePort).settingsAnchor("localAnnouncePort")
				TextField("Local announcement address", text: draft.localAnnounceMCAddr).settingsAnchor("localAnnounceMCAddr")
			} header: {
				Text("Local discovery")
			} footer: {
				Text(
					"Devices find each other on the local network with a broadcast to this port over IPv4, and to this IPv6 multicast address. Every device has to use the same ones."
				)
			}
		}

		private func natSection(_ draft: Binding<Draft>) -> some View {
			Section {
				SettingsNumberField(title: "Port mapping lease", unit: "minutes", text: draft.natLeaseMinutes).settingsAnchor(
					"natLeaseMinutes")
				SettingsNumberField(title: "Port mapping renewal", unit: "minutes", text: draft.natRenewalMinutes).settingsAnchor(
					"natRenewalMinutes")
				SettingsNumberField(title: "Port mapping timeout", unit: "seconds", text: draft.natTimeoutSeconds).settingsAnchor(
					"natTimeoutSeconds")
				SettingsNumberField(title: "STUN keepalive", unit: "seconds", text: draft.stunKeepaliveStartS).settingsAnchor(
					"stunKeepaliveStartS")
			} header: {
				Text("Port mapping and STUN")
			} footer: {
				Text(
					"How long a port mapping on the router lasts, when it is renewed, and how long the router gets to answer. How often to start refreshing what STUN learned. STUN itself is turned on and off in Settings › Network."
				)
			}
		}

		private func networksSection(_ draft: Binding<Draft>) -> some View {
			Section {
				SettingsLinesEditor(title: "Always local networks", text: draft.alwaysLocalNets).settingsAnchor("alwaysLocalNets")
			} header: {
				Text("Always local networks")
			} footer: {
				Text(
					"Connections from these networks count as local, for limits and priorities, even when the addresses aren't. One per line, such as 100.64.0.0/10."
				)
			}
		}

		private func namesSection(_ draft: Binding<Draft>) -> some View {
			Section {
				Toggle("Use the names devices give themselves", isOn: draft.overwriteRemoteDeviceNames).settingsAnchor(
					"overwriteRemoteDeviceNames")
			} header: {
				Text("Device names")
			} footer: {
				Text("Replaces the name of a device here with its own each time it connects.")
			}
		}
	}
#endif
