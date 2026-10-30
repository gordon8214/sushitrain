// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI
	import SynctrainHostKit

	/// Shown before the engine starts next to Syncthing.app (EngineSetupDecision, which AppState follows), and from the
	/// configuration settings: takes over Syncthing's setup, so that this Mac keeps its device ID, folders and devices,
	/// or waits for Syncthing to quit.
	struct SyncthingTakeoverView: View {
		@Environment(AppState.self) private var appState
		let decision: EngineSetupDecision
		/// From the settings, while the engine runs: only Syncthing's own configuration can be taken over then
		var fromSettings = false

		@State private var summary: SyncthingInstallation.Summary? = nil
		@State private var summaryError: String? = nil
		@State private var isRunning = false
		@State private var busyPorts: [String] = []
		@State private var isWorking = false
		@State private var errorMessage: String? = nil
		@State private var loginItemRemoved = false
		@State private var previousStartAtLogin = SyncthingInstallation.startsAtLogin

		var body: some View {
			Form {
				Section {
					Text(self.explanation)
				} header: {
					Text(self.title)
				}

				Section {
					if let summary {
						LabeledContent("Device ID") {
							Text(summary.deviceID.prefix(7))
								.monospaced()
								.textSelection(.enabled)
						}
						LabeledContent("Folders", value: "\(summary.folders?.count ?? 0)")
						LabeledContent("Devices", value: "\(summary.devices)")
					}
					else if let summaryError {
						Text(summaryError).foregroundStyle(.red)
					}
					LabeledContent("Status") {
						Text(isRunning ? String(localized: "Running") : String(localized: "Not running"))
					}
					// The app's own engine would count too
					if !fromSettings && !busyPorts.isEmpty {
						LabeledContent("Ports in use") {
							Text(busyPorts.joined(separator: ", "))
						}
					}
				} header: {
					Text("Syncthing on this Mac")
				}

				Section {
					if isRunning {
						Button("Quit Syncthing") {
							Task { await self.quit() }
						}
					}
					Button("Remove Syncthing from Login Items") {
						Task { await self.stopSyncthingAtLogin() }
					}
					.disabled(loginItemRemoved)
					if !TestRun.isActive {
						AutoStartToggleView()
					}
				} footer: {
					Text(
						"Syncthing.app starts its engine again unless it quits itself. Removing its login item keeps it from starting at the next login."
					)
				}

				self.choices()
			}
			.formStyle(.grouped)
			.disabled(isWorking)
			.overlay {
				if isWorking {
					ProgressView()
				}
			}
			// What it shows; AppState decides when to go on
			.task(id: decision) {
				while !Task.isCancelled {
					self.refresh()
					try? await Task.sleep(for: .seconds(2))
				}
			}
			.alert(
				"An error has occurred", isPresented: Binding.isNotNil($errorMessage),
				actions: {
					Button("OK") {
						errorMessage = nil
					}
				},
				message: {
					Text(errorMessage ?? "")
				})
		}

		private var title: String {
			switch decision {
			case .waitForSyncthing:
				return String(localized: "Waiting for Syncthing to quit")
			case .askToRunAlongside:
				return String(localized: "Syncthing is running")
			case .offerTakeover, .start:
				return String(localized: "Syncthing is set up on this Mac")
			}
		}

		private var explanation: String {
			switch decision {
			case .waitForSyncthing:
				return String(
					localized:
						"Syncthing runs with this Mac's device identity, which must not run twice. Synctrain starts once Syncthing has quit."
				)
			case .askToRunAlongside:
				return String(
					localized:
						"Another Syncthing runs on this Mac. Both would compete for the same ports. Quit it, or run Synctrain alongside it."
				)
			case .offerTakeover, .start:
				return String(
					localized:
						"Synctrain can take over Syncthing's setup, so that this Mac keeps its device ID, folders and devices, and nothing synchronizes again. Syncthing must quit first."
				)
			}
		}

		private var canTakeOver: Bool {
			guard let summary, !isRunning else {
				return false
			}
			return !summary.tooNew && !summary.deviceID.isEmpty && summary.identityError == nil
		}

		@ViewBuilder private func choices() -> some View {
			switch decision {
			case .offerTakeover, .start:
				Section {
					Button("Use Syncthing's configuration") {
						Task { await self.useSyncthing() }
					}
					.disabled(!canTakeOver)
					if !fromSettings {
						Button("Import a copy") {
							Task { await self.importSyncthing() }
						}
						.disabled(!canTakeOver)
						Button("Start with a new configuration") {
							EngineSetup.choice = .startFresh
							Task { await appState.continueStart() }
						}
					}
				} footer: {
					VStack(alignment: .leading) {
						if summary?.tooNew == true {
							Text(
								"Syncthing's configuration or database is from a newer version than this app's engine, which cannot use it."
							)
						}
						if summary?.identityError != nil {
							Text(
								"Syncthing's device certificate or key is missing or damaged, so its device ID can't be kept."
							)
						}
						if fromSettings {
							Text(
								"Using Syncthing's configuration backs it up first, then Synctrain restarts on it and leaves its own configuration as it is; Syncthing.app must not run anymore."
							)
						}
						else {
							Text(
								"Using Syncthing's configuration backs it up first, then Synctrain restarts on it; Syncthing.app must not run anymore. Importing a copy puts Syncthing's configuration, keys and database in Synctrain's own folder and leaves Syncthing's as they are. A new configuration gives this Mac a new device ID."
							)
						}
					}
				}
			case .askToRunAlongside:
				Section {
					Button("Run alongside Syncthing") {
						EngineSetup.choice = .runAlongside
						Task { await appState.continueStart() }
					}
				}
			case .waitForSyncthing:
				EmptyView()
			}
		}

		private func refresh() {
			let wasRunning = isRunning
			isRunning = SyncthingInstallation.isRunning(besides: appState.client)
			if summary == nil || wasRunning != isRunning {
				self.summarize()
			}
			busyPorts = isRunning ? self.portsInUse() : []
		}

		private func summarize() {
			do {
				summary = try SyncthingInstallation.summary()
				summaryError = nil
			}
			catch {
				summaryError = error.localizedDescription
			}
		}

		private func portsInUse() -> [String] {
			var addresses = ["127.0.0.1:22000"]
			if let gui = summary?.guiAddress, summary?.guiNetwork != "unix", !gui.isEmpty {
				addresses.append(gui)
			}
			return addresses.filter { SushitrainIsAddressInUse($0) }
		}

		private func quit() async {
			isWorking = true
			defer { isWorking = false }
			if !(await SyncthingInstallation.quit()) {
				errorMessage = String(localized: "Syncthing is still running. Quit it yourself, then try again.")
			}
			isRunning = SyncthingInstallation.isRunning(besides: appState.client)
			// What it holds now that it stopped
			self.summarize()
		}

		/// Keeps Syncthing.app from starting at login; says when its login item has to go in System Settings
		private func stopSyncthingAtLogin() async {
			let (removed, problem) = await EngineSetup.stopSyncthingAtLogin()
			loginItemRemoved = removed
			if let problem {
				errorMessage = problem.localizedDescription
				if case SyncthingInstallation.LoginItemError.notAllowed = problem {
					SyncthingInstallation.openLoginItemsSettings()
				}
			}
		}

		private func useSyncthing() async {
			guard let summary else {
				return
			}
			// Until the app restarts
			isWorking = true
			if !loginItemRemoved {
				await self.stopSyncthingAtLogin()
			}
			do {
				try await EngineSetup.useSyncthing(
					summary, appState: appState, loginItemRemoved: loginItemRemoved,
					previousStartAtLogin: previousStartAtLogin)
			}
			catch {
				isWorking = false
				errorMessage = error.localizedDescription
			}
		}

		private func importSyncthing() async {
			guard let summary else {
				return
			}
			isWorking = true
			defer { isWorking = false }
			if !loginItemRemoved {
				await self.stopSyncthingAtLogin()
			}
			do {
				try await EngineSetup.importSyncthing(
					summary, appState: appState, configDirectory: appState.configDirectory,
					loginItemRemoved: loginItemRemoved, previousStartAtLogin: previousStartAtLogin)
				await appState.continueStart()
			}
			catch {
				errorMessage = error.localizedDescription
			}
		}
	}
#endif
