// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainHostKit

	/// What Synctrain does on this Mac, which is one thing at a time. The whole app differs between them, so the mode is
	/// settled before anything starts (`AppEntry`), and changing it restarts the app.
	enum AppMode: String {
		/// The engine runs, and every folder is synchronized in full. This Mac can be a gateway for other devices.
		case synchronize
		/// No engine runs. Finder shows the folders of a gateway on another system, which has all their files.
		case filesInFinder

		private static let storageKey = "appMode"

		/// The mode to run in, or nil when the user has yet to choose one: only a Mac that never ran the engine does. A
		/// mode that follows from what is on this Mac stays, so that a later change can't turn it into a question.
		static func resolve() -> AppMode? {
			if let stored = UserDefaults.standard.string(forKey: storageKey).flatMap(AppMode.init(rawValue:)) {
				return stored
			}
			if TestRun.isActive {
				// A test run can't relaunch, so it never stops at the choice, and leaves the choice unmade
				return TestRun.allowsFileProvider ? .filesInFinder : .synchronize
			}
			if hasEngineData {
				choose(.synchronize)
				return .synchronize
			}
			return nil
		}

		static func choose(_ mode: AppMode) {
			UserDefaults.standard.set(mode.rawValue, forKey: storageKey)
		}

		/// For the app's actions, and what looks things up for them, which reach for the engine's state. They run in
		/// either mode, and can't be hidden in one of them.
		static func requireEngine() throws {
			guard resolve() == .synchronize else {
				throw EngineNotRunning()
			}
		}

		/// Whether the engine has run on this Mac. This reads the settings as they are, as `ConfigDirectorySetting.path`
		/// writes and logs, which isn't set up yet. A sandboxed version's preferences aren't taken over at this point,
		/// so its container counts by itself, also when it can't be read.
		private static var hasEngineData: Bool {
			let defaults = UserDefaults.standard
			if defaults.string(forKey: ConfigDirectorySetting.pathKey)?.isEmpty == false
				|| defaults.data(forKey: ConfigDirectorySetting.bookmarkKey) != nil
				|| defaults.integer(forKey: "lastRunBuild") > 0
			{
				return true
			}
			let configuration = EngineLocations.current.configDirectory.appending(path: "config.xml")
			if FileManager.default.fileExists(atPath: configuration.path(percentEncoded: false)) {
				return true
			}
			let container = ContainerMigration.presence(of: ContainerMigrator.containerData)
			return container != ENOENT && container != ENOTDIR
		}
	}

	/// What the app's actions say in Files in Finder mode
	struct EngineNotRunning: LocalizedError {
		var errorDescription: String? {
			String(localized: "This action needs Synchronize mode: this Mac runs no engine in Files in Finder mode.")
		}
	}
#endif
