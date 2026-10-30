// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SushitrainCore
	import SwiftUI

	/// Syncthing's web GUI and REST API, which the engine runs on macOS: whether they are on, where, and with which API
	/// key, for tools and scripts that talk to the engine as they would to Syncthing.app
	struct EngineAccessSection: View {
		@Environment(AppState.self) private var appState
		@State private var isEnabled = false
		@State private var url = ""
		@State private var apiKey = ""
		@State private var status = ""
		@State private var showAPIKey = false
		@State private var confirmRegenerateAPIKey = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Section {
				Toggle(
					"Web GUI and REST API",
					isOn: Binding(
						get: { isEnabled },
						set: { enabled in
							do {
								try appState.client.setGUIEnabled(enabled)
								// They start and stop with the engine
								appState.restartEngine()
							}
							catch {
								errorMessage = ErrorMessage(error)
							}
							self.update()
						})
				)
				.disabled(appState.isEngineRestarting)

				if isEnabled {
					LabeledContent("Address") {
						Text(url)
							.textSelection(.enabled)
						Button("Copy address", systemImage: "doc.on.doc") {
							Self.copy(url, concealed: false)
						}
						.labelStyle(.iconOnly)
						.buttonStyle(.borderless)
						.help("Copy address")
					}
					LabeledContent("API key") {
						Text(showAPIKey ? apiKey : String(repeating: "•", count: 16))
							.textSelection(.enabled)
							.monospaced()
						Button(showAPIKey ? "Hide" : "Show", systemImage: showAPIKey ? "eye.slash" : "eye") {
							showAPIKey.toggle()
						}
						.labelStyle(.iconOnly)
						.buttonStyle(.borderless)
						Button("Copy API key", systemImage: "doc.on.doc") {
							Self.copy(apiKey, concealed: true)
						}
						.labelStyle(.iconOnly)
						.buttonStyle(.borderless)
						.help("Copy API key")
						Button("Regenerate API key", systemImage: "arrow.clockwise") {
							confirmRegenerateAPIKey = true
						}
						.labelStyle(.iconOnly)
						.buttonStyle(.borderless)
						.help("Regenerate API key")
						.confirmationDialog(
							"Regenerate the API key?", isPresented: $confirmRegenerateAPIKey, titleVisibility: .visible
						) {
							Button("Regenerate API key", role: .destructive) {
								self.regenerateAPIKey()
							}
						} message: {
							Text("Tools and scripts that use the current key lose access until they are given the new one.")
						}
					}
				}
				NavigationLink("Web GUI and REST API settings") {
					GUISettingsView()
				}
			} header: {
				Text("Engine access")
			} footer: {
				VStack(alignment: .leading) {
					Text(
						"Tools and scripts reach the engine here with the API key, as they would Syncthing. The key gives full control over this device's synchronization: keep it private."
					)
					if !status.isEmpty {
						Text(status)
					}
				}
			}
			// Again after the engine restarted, and when the app or the REST API saved the configuration
			.task(id: appState.isEngineRestarting) {
				self.update()
			}
			.onChange(of: appState.eventCounter) {
				self.update()
			}
			.errorAlert($errorMessage)
		}

		private func regenerateAPIKey() {
			let client = appState.client
			Task {
				do {
					// It waits for the service to listen again
					try await goBlocking { try client.regenerateGUIAPIKey() }
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				self.update()
			}
		}

		private func update() {
			let client = appState.client
			isEnabled = client.isGUIEnabled()
			url = client.guiurl()
			apiKey = client.guiapiKey()
			status = client.guiStatusMessage()
		}

		/// Copies text; concealed, clipboard managers that honor it don't keep it
		private static func copy(_ text: String, concealed: Bool) {
			let pasteboard = NSPasteboard.general
			pasteboard.clearContents()
			pasteboard.setString(text, forType: .string)
			if concealed {
				pasteboard.setString("", forType: NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType"))
			}
		}
	}
#endif
