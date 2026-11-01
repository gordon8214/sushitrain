// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit

	extension AppPresence {
		/// Without a Dock icon the app has no app menu either
		var activationPolicy: NSApplication.ActivationPolicy {
			inDock ? .regular : .accessory
		}

		/// For the app delegate, when the app is opened while it runs. That's the way back to it where macOS doesn't show
		/// the menu bar item, as in a full menu bar. With a Dock icon AppKit and SwiftUI take care of it, as they do for
		/// the windows that are only minimized.
		@MainActor static func reopen(hasVisibleWindows: Bool, openWindow: () -> Void) -> Bool {
			guard !stored(in: .standard).inDock, !hasVisibleWindows, !NSApp.windows.contains(where: \.isMiniaturized) else {
				return true
			}
			openWindow()
			NSApp.activate()
			return false
		}

		/// For the app delegate, when the app goes to the background. Opening a running app, as from Finder, makes it a
		/// regular one. Taking that back at once would make the app inactive, with what was opened behind other apps, and
		/// it can't then get to the front by itself, so it waits until the user moves on.
		@MainActor static func restoreActivationPolicy() {
			NSApp.setActivationPolicy(stored(in: .standard).activationPolicy)
		}
	}

	extension Scene {
		/// From the menu bar alone the app starts without a window
		func restoredOnlyWithDockIcon(in presence: AppPresence) -> some Scene {
			restorationBehavior(presence.inDock ? .automatic : .disabled)
		}
	}

	/// Where the app shows itself. Turning off the only place it shows puts it in the other (`AppPresence`).
	struct AppPresenceToggles: View {
		@Binding var presence: AppPresence

		var body: some View {
			Toggle(isOn: $presence.inMenuBar) {
				Label("Show in menu bar", systemImage: "menubar.rectangle")
			}
			Toggle(isOn: $presence.inDock) {
				Label("Show in Dock", systemImage: "dock.rectangle")
			}
		}
	}
#endif
