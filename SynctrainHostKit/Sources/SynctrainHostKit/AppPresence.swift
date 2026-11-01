// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Where the Mac app shows itself. Having neither can't be expressed, as nothing would lead back to the app: taking it
/// out of one place puts it in the other, which also goes for the user removing the menu bar item.
public enum AppPresence: String, Sendable {
	case menuBarAndDock
	case menuBar
	case dock

	public static let standard = AppPresence.menuBarAndDock
	public static let storageKey = "appPresence"

	/// The one setting there was before, which both hid the Dock icon and added the menu bar item
	private static let hideInDockKey = "hideInDock"

	public var inMenuBar: Bool {
		get { self != .dock }
		set { self = newValue ? (inDock ? .menuBarAndDock : .menuBar) : .dock }
	}

	public var inDock: Bool {
		get { self != .menuBar }
		set { self = newValue ? (inMenuBar ? .menuBarAndDock : .dock) : .menuBar }
	}

	/// For what isn't a view, which has no `@AppStorage`
	public static func stored(in defaults: UserDefaults) -> AppPresence {
		defaults.string(forKey: storageKey).flatMap(AppPresence.init(rawValue:)) ?? standard
	}

	/// Keeps the look of anyone who changed `hideInDock`. Who never did has no such key, and gets the default. To be
	/// called before the setting is read, and after the sandboxed app's preferences arrive.
	public static func adoptHideInDock(in defaults: UserDefaults) {
		guard let hideInDock = defaults.object(forKey: hideInDockKey) as? Bool else {
			return
		}
		if defaults.object(forKey: storageKey) == nil {
			defaults.set((hideInDock ? menuBar : dock).rawValue, forKey: storageKey)
		}
		defaults.removeObject(forKey: hideInDockKey)
	}
}
