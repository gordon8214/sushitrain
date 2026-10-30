// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Security
import SynctrainHostKit

extension AppLocations {
	/// This app's locations.
	///
	/// In debug builds on macOS, `SYNCTRAIN_TEST_ROOT` moves them, including the app group's caches, under a scratch
	/// directory, so a manual test run leaves the real ones alone.
	static let current: AppLocations = {
		var systemCaches = URL.cachesDirectory
		var systemTemporary = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
		let appGroup = Bundle.main.object(forInfoDictionaryKey: "SynctrainAppGroup") as? String
		var groupCaches = appGroup.flatMap { AppLocations.groupCaches(appGroup: $0) }
		if let root = EngineLocations.testRoot {
			systemCaches = root.appending(path: "Library/Caches", directoryHint: .isDirectory)
			systemTemporary = root.appending(path: "tmp", directoryHint: .isDirectory)
			groupCaches = root.appending(path: "Group/Library/Caches", directoryHint: .isDirectory)
		}

		return AppLocations(
			bundleIdentifier: Bundle.main.bundleIdentifier ?? "gordonbyrnes.Sushitrain", systemCaches: systemCaches,
			systemTemporary: systemTemporary, groupCaches: groupCaches)
	}()

	/// Whether the system caches and temporary directories belong to this app alone, as they do on iOS and in the App
	/// Sandbox, so that what earlier versions left there may be moved or removed. On macOS this is the app's own
	/// sandbox entitlement, not an environment variable a process could inherit.
	static let systemDirectoriesArePrivate: Bool = {
		#if os(iOS)
			return true
		#else
			guard let task = SecTaskCreateFromSelf(nil) else {
				return false
			}
			let sandboxed = SecTaskCopyValueForEntitlement(task, "com.apple.security.app-sandbox" as CFString, nil)
			return (sandboxed as? Bool) == true
		#endif
	}()

	/// Creates the app's directories and, in the background, removes temporary files older than a day.
	static func prepareCurrent() {
		let locations = AppLocations.current
		do {
			try locations.prepare()
		}
		catch {
			Log.warn("Could not create the app's cache and temporary directories: \(error.localizedDescription)")
		}

		let systemDirectoriesArePrivate = Self.systemDirectoriesArePrivate
		if systemDirectoriesArePrivate {
			do {
				try locations.moveLegacyThumbnails()
			}
			catch {
				Log.warn("Could not move the thumbnail cache into the app's cache directory: \(error.localizedDescription)")
			}
		}

		Task.detached(priority: .background) {
			let errors = locations.pruneTemporaryFiles(
				modifiedBefore: Date(timeIntervalSinceNow: -86400), includingLegacyItems: systemDirectoriesArePrivate)
			for error in errors {
				Log.warn("Could not remove an old temporary file: \(error.localizedDescription)")
			}
		}
	}
}
