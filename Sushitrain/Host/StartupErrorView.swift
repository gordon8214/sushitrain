// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SushitrainCore
import SwiftUI

/// Shown when the app can't start. The engine leaves a configuration or identity it can't use as it is; when the user
/// can do something about it, this offers that.
struct StartupErrorView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.openURL) private var openURL
	let message: String
	@State private var confirmNewConfiguration = false
	@State private var confirmNewIdentity = false
	@State private var errorMessage: ErrorMessage? = nil

	private var problem: String {
		appState.client.startupProblem()
	}

	var body: some View {
		ContentUnavailableView {
			Label("Cannot start the app", systemImage: "exclamationmark.triangle.fill")
		} description: {
			Text(message)
		} actions: {
			#if os(macOS)
				if problem == SushitrainStartupProblemDamagedConfiguration
					|| problem == SushitrainStartupProblemNewerConfiguration
				{
					Button("Show in Finder") {
						let file = URL(fileURLWithPath: appState.client.configurationFilePath())
						NSWorkspace.shared.activateFileViewerSelecting([file])
					}
				}
			#endif
			if problem == SushitrainStartupProblemDamagedConfiguration && !appState.client.isUsingCustomConfiguration {
				Button("Start with a new configuration", role: .destructive) {
					confirmNewConfiguration = true
				}
			}
			if appState.isEngineRestarting {
				ProgressView()
			}
			else if appState.engineCanRestart {
				Button("Restart Syncthing") {
					appState.restartEngine()
				}
			}
			if problem == SushitrainStartupProblemDamagedIdentity {
				Button("Reset device identity", role: .destructive) {
					confirmNewIdentity = true
				}
			}
			Button("What do I do now?") {
				openURL(URL(string: "https://t-shaped.nl/synctrain-support#cannot-start")!)
			}
		}
		.alert("Start with a new configuration?", isPresented: $confirmNewConfiguration) {
			Button("Start with a new configuration", role: .destructive) {
				var error: NSError? = nil
				let asidePath = appState.client.setConfigurationAside(&error)
				if let error {
					errorMessage = ErrorMessage(error)
					return
				}
				Log.info("Set the configuration aside as \(asidePath); quitting to start anew")
				exit(0)
			}
			Button("Cancel", role: .cancel) {}
		} message: {
			Text(
				"The configuration file is renamed and kept next to the new one. The app then closes; when you open it again, it starts with a new configuration that keeps this device's identity but has no folders or devices."
			)
		}
		.alert("Read this carefully", isPresented: $confirmNewIdentity) {
			Button("Reset device identity", role: .destructive) {
				do {
					try appState.client.clearIdentity()
					exit(0)
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
			Button("Cancel", role: .cancel) {}
		} message: {
			Text(
				"If you reset the device identity, the device ID will change, and you will have to re-add this device to all other devices. Any files on this device will not be removed, but synchronization will not work until the other devices have re-accepted the new identity of this device again. Resetting the device identity should only be necessary if you have migrated from another device. The app will close to reset the device identity. This cannot be undone!"
			)
		}
		.errorAlert($errorMessage)
	}
}
