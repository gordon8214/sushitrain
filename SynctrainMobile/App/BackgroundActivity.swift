// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import UIKit

/// Background time for work that should finish when the app leaves the foreground. Ends when the work is done or
/// when the time is up, whichever comes first (iOS terminates apps that keep it longer).
@MainActor final class BackgroundActivity {
	private var identifier = UIBackgroundTaskIdentifier.invalid

	init(name: String, onExpiration: (@MainActor @Sendable () -> Void)? = nil) {
		identifier = UIApplication.shared.beginBackgroundTask(withName: name) { [weak self] in
			MainActor.assumeIsolated {
				onExpiration?()
				self?.end()
			}
		}
	}

	func end() {
		guard identifier != .invalid else {
			return
		}
		UIApplication.shared.endBackgroundTask(identifier)
		identifier = .invalid
	}
}
