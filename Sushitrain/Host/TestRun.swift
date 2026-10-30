// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainHostKit

	/// A debug run with `SYNCTRAIN_TEST_ROOT` (EngineLocations.testRoot): the settings that decide which engine data the
	/// app uses are kept in the scratch directory instead of the real preferences, and nothing is changed in other
	/// apps' settings, so that the real app is as it was afterwards
	enum TestRun {
		static var isActive: Bool {
			EngineLocations.testRoot != nil
		}

		private static var url: URL? {
			EngineLocations.testRoot?.appending(path: "synctrain-test-settings.plist")
		}

		private static func settings() -> [String: Any] {
			guard let url, let data = try? Data(contentsOf: url),
				let settings = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
			else {
				return [:]
			}
			return settings
		}

		static func value(forKey key: String) -> Any? {
			self.settings()[key]
		}

		static func set(_ value: Any?, forKey key: String) {
			guard let url else {
				return
			}
			var settings = self.settings()
			settings[key] = value
			do {
				let data = try PropertyListSerialization.data(fromPropertyList: settings, format: .xml, options: 0)
				try data.write(to: url, options: [.atomic])
			}
			catch {
				Log.warn("Could not keep a test run's setting: \(error.localizedDescription)")
			}
		}
	}
#endif
