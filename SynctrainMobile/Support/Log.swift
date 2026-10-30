// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import os

/// The app's log, in the system's unified log
enum Log {
	private static let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "Synctrain", category: "app")

	static func info(_ message: String) {
		logger.info("\(message, privacy: .public)")
	}

	static func warn(_ message: String) {
		logger.warning("\(message, privacy: .public)")
	}
}
