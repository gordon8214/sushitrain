// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if DEBUG && os(macOS)
	import SwiftUI
	import SynctrainHostKit

	/// A native window for checking Files-mode settings without starting shared storage, credentials, or File Provider.
	/// AppEntry admits this only with an isolated test root, before either production app lifecycle starts.
	struct SettingsFixtureApp: App {
		@State private var presence = AppPresence.menuBarAndDock

		init() {
			MCPIntegration.shared.start()
			if ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_SETTINGS_FIXTURE"] == "populated" {
				RemoteCoordinator.shared.loadSettingsFixture()
			}
		}

		static var colorScheme: ColorScheme? {
			guard TestRun.isActive else { return nil }
			switch ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_SETTINGS_APPEARANCE"] {
			case "light": return .light
			case "dark": return .dark
			default: return nil
			}
		}

		var body: some Scene {
			Window("Synctrain Settings Fixture", id: "settingsFixture") {
				FilesSettingsView(presence: $presence)
			}
			.defaultSize(width: 1000, height: 720)
			.windowResizability(.contentMinSize)
			.restorationBehavior(.disabled)
		}
	}
#endif
