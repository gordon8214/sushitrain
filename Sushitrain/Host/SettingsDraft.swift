// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	/// Settings being edited: the engine's object, in a struct so that SwiftUI sees it change
	@dynamicMemberLookup
	struct SettingsDraft<Settings: AnyObject> {
		let settings: Settings
		private var changes = 0

		init(_ settings: Settings) {
			self.settings = settings
		}

		subscript<Value>(dynamicMember keyPath: ReferenceWritableKeyPath<Settings, Value>) -> Value {
			get { settings[keyPath: keyPath] }
			set {
				settings[keyPath: keyPath] = newValue
				changes += 1
			}
		}
	}
#endif
