// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// How much space downloaded files may take
enum CacheSettings {
	static let storageKey = "cacheLimitBytes"
	static let defaultLimit = 5 << 30
	static let choices = [1 << 30, 2 << 30, 5 << 30, 10 << 30, 20 << 30]

	static var limit: Int64 {
		Int64(UserDefaults.standard.object(forKey: storageKey) as? Int ?? defaultLimit)
	}
}
