// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import UIKit

final class MobileAppDelegate: NSObject, UIApplicationDelegate {
	/// Background transfers finished while the app was not running: the app's own session, or the File Provider
	/// extension's (which the app recreates only to finish its events). The system's handler is called on the main
	/// thread, exactly once, after the events were handled.
	func application(
		_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String,
		completionHandler: @escaping () -> Void
	) {
		let completion: @MainActor @Sendable () -> Void = { completionHandler() }
		let handled = RemoteCoordinator.shared.handleBackgroundEvents(identifier: identifier, completion: completion)
		if !handled {
			Log.warn("Events for unknown background session \(identifier)")
			completionHandler()
		}
	}
}
