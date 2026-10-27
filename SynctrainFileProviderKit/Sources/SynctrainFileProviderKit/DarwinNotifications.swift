// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Notifications sent between the app and the extension. Darwin notifications carry no payload and are only delivered
/// to processes that are running; anything that must not be lost is also written to the shared index.
public enum SharedNotification: String, Sendable, CaseIterable {
	/// The extension queued work for the app
	case workQueued = "nl.t-shaped.sushitrain.fileprovider.work-queued"

	public func post() {
		CFNotificationCenterPostNotification(
			CFNotificationCenterGetDarwinNotifyCenter(), CFNotificationName(rawValue as CFString), nil, nil, true)
	}
}

/// Calls the handler (on an arbitrary thread) whenever the notification is posted, until the observer is released
public final class SharedNotificationObserver: @unchecked Sendable {
	private let handler: @Sendable () -> Void
	private let name: SharedNotification

	public init(_ name: SharedNotification, handler: @escaping @Sendable () -> Void) {
		self.handler = handler
		self.name = name
		CFNotificationCenterAddObserver(
			CFNotificationCenterGetDarwinNotifyCenter(), Unmanaged.passUnretained(self).toOpaque(),
			{ _, observer, _, _, _ in
				guard let observer else {
					return
				}
				Unmanaged<SharedNotificationObserver>.fromOpaque(observer).takeUnretainedValue().handler()
			}, name.rawValue as CFString, nil, .deliverImmediately)
	}

	deinit {
		CFNotificationCenterRemoveObserver(
			CFNotificationCenterGetDarwinNotifyCenter(), Unmanaged.passUnretained(self).toOpaque(),
			CFNotificationName(name.rawValue as CFString), nil)
	}
}
