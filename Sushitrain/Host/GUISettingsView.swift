// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// The settings of Syncthing's web GUI and REST API. Unlike the app's other settings they are saved together: each
	/// change makes the running service listen again, and a user and a password only make sense as a pair.
	struct GUISettingsView: View {
		/// The settings being edited: the engine's object, in a struct so that SwiftUI sees it change
		@dynamicMemberLookup
		private struct Draft {
			let settings: SushitrainGUISettings
			private var changes = 0

			init(_ settings: SushitrainGUISettings) {
				self.settings = settings
			}

			subscript<Value>(dynamicMember keyPath: ReferenceWritableKeyPath<SushitrainGUISettings, Value>) -> Value {
				get { settings[keyPath: keyPath] }
				set {
					settings[keyPath: keyPath] = newValue
					changes += 1
				}
			}
		}

		/// The themes Syncthing comes with
		private static let themes = ["default", "light", "dark", "black"]

		@Environment(AppState.self) private var appState
		@State private var draft: Draft? = nil
		@State private var saved: SushitrainGUISettings? = nil
		// As text, as a number field only commits its value when it loses focus, which clicking Save doesn't do
		@State private var sessionCookieDuration = ""
		@State private var isOverridden = false
		@State private var isSaving = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			Group {
				if let draft = Binding($draft) {
					Form {
						self.connectionSection(draft)
						self.authenticationSection(draft)
						self.webGUISection(draft)
						self.insecureSection(draft)
					}
					.autocorrectionDisabled()
					// The engine reads the settings while it saves them
					.disabled(isSaving)
				}
				else {
					ProgressView()
				}
			}
			.formStyle(.grouped)
			.navigationTitle("Web GUI and REST API")
			.toolbar {
				// Says what it does, where the system would call a button of this role Done
				ToolbarItem(placement: .confirmationAction) {
					Button("Save") {
						self.save()
					}
					.disabled(!self.canSave)
				}
			}
			// Again after the engine restarted, and when something else saved the configuration
			.task(id: appState.isEngineRestarting) {
				self.refresh()
			}
			.onChange(of: appState.eventCounter) {
				self.refresh()
			}
			.errorAlert($errorMessage)
		}

		@ViewBuilder private func connectionSection(_ draft: Binding<Draft>) -> some View {
			Section {
				TextField("Address", text: draft.address, prompt: Text(verbatim: "127.0.0.1:8384"))
					.disabled(isOverridden)
				Toggle("Use HTTPS", isOn: draft.useTLS)
					.disabled(isOverridden)
			} header: {
				Text("Connection")
			} footer: {
				if isOverridden {
					Text("The STGUIADDRESS environment variable sets the address and whether HTTPS is used.")
				}
				else {
					Text(
						"Saved changes take effect right away, without restarting the engine. The address can also be the path of a Unix socket."
					)
				}
			}
		}

		@ViewBuilder private func authenticationSection(_ draft: Binding<Draft>) -> some View {
			let usesLDAP = draft.wrappedValue.authMode == "ldap"
			Section {
				Picker("Sign in with", selection: draft.authMode) {
					Text("User name and password").tag("static")
					Text("LDAP server").tag("ldap")
				}
				if usesLDAP {
					TextField("Server address", text: draft.ldapAddress, prompt: Text(verbatim: "ldap.example.com:636"))
					Picker("Transport", selection: draft.ldapTransport) {
						Text("Unencrypted").tag("plain")
						Text(verbatim: "TLS").tag("tls")
						Text(verbatim: "StartTLS").tag("starttls")
					}
					Toggle("Skip certificate verification", isOn: draft.ldapInsecureSkipVerify)
					TextField("Bind DN", text: draft.ldapBindDN, prompt: Text(verbatim: "cn=%s,dc=example,dc=com"))
					TextField("Search base DN", text: draft.ldapSearchBaseDN, prompt: Text(verbatim: "dc=example,dc=com"))
					TextField("Search filter", text: draft.ldapSearchFilter, prompt: Text(verbatim: "(uid=%s)"))
				}
				else {
					TextField("User name", text: draft.user)
					SecureField(
						"New password", text: draft.password, prompt: draft.wrappedValue.hasPassword ? Text("Unchanged") : nil)
				}
				if draft.wrappedValue.settings.isExposed() {
					Label(
						"Others can reach the web GUI and REST API without signing in, and control this device's synchronization.",
						systemImage: "exclamationmark.triangle.fill"
					)
					.foregroundStyle(.red)
				}
				else if draft.wrappedValue.settings.isUnprotected() {
					self.warning(
						"Without signing in, any user or program on this Mac can use the web GUI and REST API, and control this device's synchronization."
					)
				}
			} header: {
				Text("Authentication")
			} footer: {
				if usesLDAP {
					Text("The web GUI asks this server to check sign-ins. Saving changes to it restarts the engine.")
				}
				else {
					Text(
						"Leave the password empty to keep it. A configuration the app made has a random password nobody knows: set one to sign in to the web GUI. Without a user name, nobody has to sign in. Tools and scripts use the API key either way."
					)
				}
			}
		}

		@ViewBuilder private func webGUISection(_ draft: Binding<Draft>) -> some View {
			let theme = draft.wrappedValue.theme
			Section {
				Picker("Theme", selection: draft.theme) {
					Text("Default").tag("default")
					Text("Light").tag("light")
					Text("Dark").tag("dark")
					Text("Black").tag("black")
					if !Self.themes.contains(theme) {
						Text(verbatim: theme).tag(theme)
					}
				}
				Toggle("Use the browser's sign-in prompt", isOn: draft.sendBasicAuthPrompt)
				Toggle("Allow metrics without signing in", isOn: draft.metricsWithoutAuth)
				TextField("Session cookie lifetime (seconds)", text: $sessionCookieDuration)
					.onChange(of: sessionCookieDuration) {
						if let seconds = Int(sessionCookieDuration) {
							draft.wrappedValue.sessionCookieDurationS = seconds
						}
					}
				TextField("Session cookie path", text: draft.sessionCookiePath, prompt: Text(verbatim: "/"))
			} header: {
				Text("Web GUI")
			} footer: {
				Text("With a session cookie lifetime of 0 a session lasts a week, and below 0 it doesn't expire.")
			}
		}

		@ViewBuilder private func insecureSection(_ draft: Binding<Draft>) -> some View {
			Section("Insecure settings") {
				Toggle("Insecure admin access", isOn: draft.insecureAdminAccess)
				if draft.wrappedValue.insecureAdminAccess {
					self.warning("The web GUI no longer warns when others can reach it without signing in.")
				}
				Toggle("Skip host check", isOn: draft.insecureSkipHostCheck)
				if draft.wrappedValue.insecureSkipHostCheck {
					self.warning("Websites you visit can reach the web GUI on a local address through DNS rebinding.")
				}
				Toggle("Allow loading in frames", isOn: draft.insecureAllowFrameLoading)
				if draft.wrappedValue.insecureAllowFrameLoading {
					self.warning("Other websites can show the web GUI in a frame, and trick you into clicking in it.")
				}
			}
		}

		private func warning(_ text: LocalizedStringKey) -> some View {
			Label(text, systemImage: "exclamationmark.triangle.fill")
				.foregroundStyle(.orange)
		}

		private var canSave: Bool {
			guard let draft, let saved, !isSaving, !appState.isEngineRestarting, Int(sessionCookieDuration) != nil else {
				return false
			}
			return !draft.settings.equal(saved)
		}

		private func save() {
			guard let settings = draft?.settings, let saved else {
				return
			}
			let client = appState.client
			let needsEngineRestart = settings.needsEngineRestart(saved) && client.isGUIEnabled()
			isSaving = true
			Task {
				do {
					// Hashing a password takes a moment, as does the service's turn to listen again
					try await goBlocking { try client.setGUISettings(settings) }
					if needsEngineRestart {
						appState.restartEngine()
					}
					self.load()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isSaving = false
			}
		}

		/// Follows the configuration, unless that would throw away what is being edited
		private func refresh() {
			if let draft, let saved {
				guard draft.settings.equal(saved), appState.client.guiSettings()?.equal(saved) == false else {
					return
				}
			}
			self.load()
		}

		private func load() {
			let client = appState.client
			let settings = client.guiSettings()
			isOverridden = client.isGUIAddressOverridden()
			saved = client.guiSettings()
			draft = settings.map(Draft.init)
			sessionCookieDuration = settings.map { String($0.sessionCookieDurationS) } ?? ""
		}
	}
#endif
