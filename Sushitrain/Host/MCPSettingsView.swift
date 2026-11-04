// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI

	struct MCPSettingsView: View {
		private var integration = MCPIntegration.shared
		var body: some View {
			Form {
				Section {
					Toggle("Enable MCP", isOn: Binding(get: { integration.enabled }, set: { integration.setEnabled($0) }))
						.accessibilityIdentifier("settings.mcp.enabled")
					Text(
						"Connected assistants can inspect settings, manage synchronization, "
							+ "and change or delete accessible files. Changes can propagate to other devices."
					)
					LabeledContent("Connection", value: "Local stdio")
					if let error = integration.error { Text(error).foregroundStyle(.red) }
				}
				Section("Client configuration") {
					Text(integration.configuration).font(.system(.body, design: .monospaced)).textSelection(.enabled)
					Button("Copy Configuration") {
						NSPasteboard.general.clearContents()
						NSPasteboard.general.setString(integration.configuration, forType: .string)
					}
					Text("Start Synctrain before connecting. Closing an MCP client leaves Synctrain running.")
				}
			}
			.task { integration.refresh() }
		}
	}
#endif
