// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import UIKit

/// What the work with gateways (SynctrainClient) needs from the app's life on this platform
@MainActor enum ClientLifecycle {
	enum Event {
		/// The app returns after being away
		case willResume
		/// The app can reach out freely
		case didBecomeActive
		/// The app won't be able to keep connections open
		case didStop

		fileprivate var notification: Notification.Name {
			switch self {
			case .willResume: return UIApplication.willEnterForegroundNotification
			case .didBecomeActive: return UIApplication.didBecomeActiveNotification
			case .didStop: return UIApplication.didEnterBackgroundNotification
			}
		}
	}

	/// Whether the app is on the screen, and may keep long polls open
	static var isActive: Bool {
		UIApplication.shared.applicationState == .active
	}

	/// Whether the app runs on limited time
	static var isInBackground: Bool {
		UIApplication.shared.applicationState == .background
	}

	static var backgroundTimeRemaining: TimeInterval {
		UIApplication.shared.backgroundTimeRemaining
	}

	static func observe(_ event: Event, handler: @escaping @MainActor @Sendable () -> Void) -> any NSObjectProtocol {
		NotificationCenter.default.addObserver(forName: event.notification, object: nil, queue: .main) { _ in
			MainActor.assumeIsolated(handler)
		}
	}

	/// Shows a location of the Synctrain location where the system shows files
	static func show(_ url: URL) {
		openInFilesApp(url: url)
	}
}
