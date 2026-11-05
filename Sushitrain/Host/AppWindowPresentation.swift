// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit

	@MainActor enum AppWindowPresentation {
		/// Activate during the user's action, then let menu tracking finish before SwiftUI opens or raises the window.
		/// Opening it while the menu still owns focus can leave it behind the previously active app when the menu closes.
		static func open(_ openWindow: @escaping @MainActor () -> Void) {
			NSApp.unhideWithoutActivation()
			// A menu extra doesn't activate its app. Plain activate() can leave the other app in front.
			NSApp.activate(ignoringOtherApps: true)
			RunLoop.main.perform(inModes: [.default]) {
				MainActor.assumeIsolated {
					openWindow()
					NSApp.activate(ignoringOtherApps: true)
				}
			}
		}
	}
#endif
