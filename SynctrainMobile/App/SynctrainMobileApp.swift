// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

@main struct SynctrainMobileApp: App {
	@UIApplicationDelegateAdaptor(MobileAppDelegate.self) private var delegate
	@Environment(\.scenePhase) private var scenePhase
	@State private var model: AppModel

	init() {
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
