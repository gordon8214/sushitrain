// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

@main @MainActor enum MobileEntry {
	static func main() {
		#if DEBUG
			if ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_SETTINGS_SEARCH"] == "1" {
				SettingsSearchFixtureApp.main()
				return
			}
			if ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_HISTORY_FIXTURE"] == "1" {
				HistoryFixtureApp.main()
				return
			}
		#endif
		SynctrainMobileApp.main()
	}
}

struct SynctrainMobileApp: App {
	@UIApplicationDelegateAdaptor(MobileAppDelegate.self) private var delegate
	@Environment(\.scenePhase) private var scenePhase
	@State private var model: AppModel

	init() {
		LegacyPhotoBackupPreferences.remove(from: .standard)
		let model = AppModel()
		_model = State(initialValue: model)
		// Not from a view: a launch for background events has no scene
		Task { await model.start() }
	}

	var body: some Scene {
		WindowGroup {
			RootView(model: model)
		}
		.onChange(of: scenePhase) { _, phase in
			if phase == .background {
				BackgroundRefresh.schedule()
			}
		}
		.backgroundTask(.appRefresh(BackgroundRefresh.identifier)) {
			await BackgroundRefresh.run()
		}
	}
}

#if DEBUG
	private struct SettingsSearchFixtureApp: App {
		var body: some Scene { WindowGroup { SettingsView() } }
	}
#endif
