// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SushitrainCore
	import SwiftUI

	extension AppState {
		/// Opens the web GUI in the default browser. With a password nobody knows that would end at a sign-in page nobody
		/// can pass, so the web GUI's window asks for one first, and the browser follows.
		func openWebGUIInBrowser(_ openWindow: OpenWindowAction) {
			guard let url = self.engineMenuState.webGUIURL else {
				return
			}
			if self.engineMenuState.webGUINeedsPassword {
				self.webGUIBrowserIsWaiting = true
				openWindow(id: WebGUIWindow.id)
				NSApplication.shared.activate()
			}
			else {
				NSWorkspace.shared.open(url)
			}
		}
	}

	/// Opens the web GUI, in its window or in the default browser, for the menus and the settings. Takes the app state
	/// explicitly, for the reason EngineMenuItems does.
	struct WebGUIButtons: View {
		let appState: AppState
		var hasShortcuts = false
		@Environment(\.openWindow) private var openWindow

		var body: some View {
			let state = appState.engineMenuState
			let isAvailable = state.isRunning && state.webGUIURL != nil

			Button("Open Web GUI", systemImage: "globe") {
				openWindow(id: WebGUIWindow.id)
				NSApplication.shared.activate()
			}
			.keyboardShortcut(hasShortcuts ? KeyboardShortcut("o", modifiers: [.command, .option]) : nil)
			.disabled(!isAvailable)

			Button("Open Web GUI in Browser", systemImage: "arrow.up.forward.app") {
				appState.openWebGUIInBrowser(openWindow)
			}
			.keyboardShortcut(hasShortcuts ? KeyboardShortcut("o", modifiers: [.command, .option, .shift]) : nil)
			.disabled(!isAvailable)
		}
	}

	/// Syncthing's web GUI inside the app. What it shows follows the engine and the configuration, so that it never keeps
	/// a page of a service that went away: an engine that restarts takes the page along and brings a new one back, and so
	/// does any change to the GUI's configuration.
	struct WebGUIWindow: View {
		static let id = "webGUI"

		private struct PageID: Hashable {
			let url: URL
			let revision: String
			let reloadCount: Int
		}

		@Environment(AppState.self) private var appState
		@Environment(\.openWindow) private var openWindow
		@State private var reloadCount = 0
		@State private var showSettings = false

		var body: some View {
			let state = appState.engineMenuState
			let isAvailable = state.isRunning && state.webGUIURL != nil

			self.content(state)
				.showsToast()
				.frame(minWidth: 640, minHeight: 480)
				.toolbar {
					Button("Reload", systemImage: "arrow.clockwise") {
						reloadCount += 1
					}
					.keyboardShortcut("r")
					.disabled(!isAvailable || state.webGUINeedsPassword)

					Button("Open in Browser", systemImage: "arrow.up.forward.app") {
						appState.openWebGUIInBrowser(openWindow)
					}
					.disabled(!isAvailable)

					Button("Web GUI and REST API settings", systemImage: "gear") {
						showSettings = true
					}
				}
				.sheet(isPresented: $showSettings) {
					NavigationStack {
						GUISettingsView()
							.toolbar {
								ToolbarItem(placement: .cancellationAction) {
									Button("Done") {
										showSettings = false
									}
								}
							}
					}
					.frame(minWidth: 520, minHeight: 560)
				}
				.onDisappear {
					appState.webGUIBrowserIsWaiting = false
				}
		}

		@ViewBuilder private func content(_ state: EngineMenuState) -> some View {
			if !state.isRunning {
				self.engineStatus
			}
			else if let url = state.webGUIURL {
				if state.webGUINeedsPassword {
					WebGUIPasswordForm()
				}
				else {
					WebGUIPage(url: url)
						.id(PageID(url: url, revision: state.webGUIRevision, reloadCount: reloadCount))
				}
			}
			else {
				ContentUnavailableView(
					"The web GUI is not available", systemImage: "globe",
					description: Text("It is turned off, or listens on a Unix socket. Its settings are under Engine access.")
				)
			}
		}

		@ViewBuilder private var engineStatus: some View {
			if case .error(let message) = appState.startupState, !appState.isEngineRestarting {
				ContentUnavailableView {
					Label("Syncthing is not running", systemImage: "exclamationmark.triangle.fill")
				} description: {
					Text(message)
				} actions: {
					if appState.engineCanRestart {
						Button("Restart Syncthing") {
							appState.restartEngine()
						}
					}
				}
			}
			else {
				ProgressView()
			}
		}
	}

	/// The GUI's page. Its service can be about to listen, after a restart or a change of its address, so a page that
	/// doesn't load is tried again for a few seconds before that counts as an error. That goes for the first load only:
	/// what fails after it (as a download does, in the end) leaves the GUI on screen, and working.
	private struct WebGUIPage: View {
		private static let retryLimit = 10

		let url: URL
		@Environment(AppState.self) private var appState
		@State private var isLoading = true
		@State private var hasLoaded = false
		@State private var error: Error? = nil
		@State private var retries = 0
		@State private var loadCount = 0

		/// A service that answers with an HTTP error won't say otherwise a moment later
		private var isRetrying: Bool {
			!hasLoaded && error != nil && !(error is HTTPError) && retries < Self.retryLimit
		}

		var body: some View {
			ZStack {
				WebView(
					url: url, trustFingerprints: self.trustedCertificates, isWebApp: true, isLoading: $isLoading, error: $error
				)
				.id(loadCount)

				if let error, !hasLoaded, !isRetrying {
					ContentUnavailableView {
						Label("Cannot show this page", systemImage: "exclamationmark.triangle.fill")
					} description: {
						Text(error.localizedDescription)
					} actions: {
						Button("Try Again") {
							retries = 0
							self.load()
						}
					}
					.background(.background)
				}
				else if isLoading || isRetrying {
					ProgressView()
				}
			}
			.task(id: isRetrying) {
				guard isRetrying else {
					return
				}
				try? await Task.sleep(for: .milliseconds(500))
				if !Task.isCancelled {
					retries += 1
					self.load()
				}
			}
			.onChange(of: isLoading) {
				guard !isLoading, error == nil else {
					return
				}
				hasLoaded = true
				// The service answers, also to a browser
				if appState.webGUIBrowserIsWaiting {
					appState.webGUIBrowserIsWaiting = false
					NSWorkspace.shared.open(url)
				}
			}
		}

		private func load() {
			error = nil
			isLoading = true
			loadCount += 1
		}

		/// Read for each attempt: the service makes the certificate when it first needs one, after the address is known
		private var trustedCertificates: [Data] {
			appState.client.guiCertificateSHA256().map { [$0] } ?? []
		}
	}

	/// Chooses the password of a GUI whose password nobody knows, where its sign-in page would otherwise be a dead end.
	/// The window moves on to the page by itself once the password is saved.
	private struct WebGUIPasswordForm: View {
		@Environment(AppState.self) private var appState
		@State private var user = ""
		@State private var password = ""
		@State private var isSaving = false
		@State private var errorMessage: ErrorMessage? = nil

		var body: some View {
			VStack {
				Form {
					Section {
						TextField("User name", text: $user)
						SecureField("New password", text: $password)
					} header: {
						Text("Choose a password")
					} footer: {
						Text(
							"The web GUI asks you to sign in, and this configuration's password is a random one nobody knows. Choose the user name and password to sign in with."
						)
					}
				}
				.formStyle(.grouped)
				.scrollDisabled(true)
				.fixedSize(horizontal: false, vertical: true)
				.autocorrectionDisabled()
				.disabled(isSaving)

				Button("Save") {
					self.save()
				}
				.keyboardShortcut(.defaultAction)
				.disabled(user.isEmpty || password.isEmpty || isSaving)
			}
			.frame(maxWidth: 480)
			.padding()
			.frame(maxWidth: .infinity, maxHeight: .infinity)
			.onAppear {
				user = appState.client.guiSettings()?.user ?? ""
			}
			.errorAlert($errorMessage)
		}

		private func save() {
			let client = appState.client
			guard let settings = client.guiSettings() else {
				return
			}
			settings.user = user
			settings.password = password
			isSaving = true
			Task {
				do {
					// Hashing a password takes a moment
					try await goBlocking { try client.setGUISettings(settings) }
					// Not when the event comes around, which could leave the form open to be saved again
					appState.update()
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isSaving = false
			}
		}
	}
#endif
