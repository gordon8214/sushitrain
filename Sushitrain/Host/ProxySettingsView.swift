// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// The proxy for the engine's outgoing connections, which it uses from when it next starts
	struct ProxySettingsView: View {
		@Environment(AppState.self) private var appState

		var body: some View {
			let client = appState.client
			SettingsPage(
				title: "Proxy", load: { client.proxySettings() }, isEqual: { $0.equal($1) },
				// Saving restarts the engine, so that what this page says is what the engine does: someone who turns the proxy
				// on must not go on connecting directly until they think of restarting it
				save: {
					try client.setProxySettings($0)
					try client.restartEngine()
				},
				confirmation: { _ in
					String(localized: "Saving restarts the engine, which interrupts its connections for a moment.")
				},
				content: { draft in
					let isOff = !draft.wrappedValue.enabled
					AdvancedRestartSection()
					Section {
						Toggle("Use a proxy", isOn: draft.enabled).settingsAnchor("enabled")
						Group {
							TextField("Proxy address", text: draft.url, prompt: Text(verbatim: "socks5://127.0.0.1:1080")).settingsAnchor(
								"url")
							TextField("User name", text: draft.user).settingsAnchor("user")
							SecureField("Password", text: draft.password).settingsAnchor("password")
						}
						.disabled(isOff)
						if draft.wrappedValue.settings.sendsPasswordUnencrypted() {
							SettingsWarning(text: "The proxy receives this password unencrypted, unless its address starts with https.")
						}
					} footer: {
						Text(
							"Connections to other devices, relays and discovery servers go through this proxy, and fail when it can't be reached. The address starts with socks5, http or https and ends with the port, such as socks5://127.0.0.1:1080. A proxy can't carry QUIC or STUN, so those are not used for what goes through it, and discovery servers only hear of relays, not of this device's addresses. Connections from other devices and local discovery don't use it."
						)
					}
					Section {
						SettingsLinesEditor(title: "Connect directly", text: draft.bypass).settingsAnchor("bypass")
							.disabled(isOff)
					} header: {
						Text("Connect directly")
					} footer: {
						Text(
							"These don't use the proxy: addresses, networks such as 192.168.0.0/16, host names, and zones such as *.example.com. One per line."
						)
					}
				}
			)
		}
	}
#endif
