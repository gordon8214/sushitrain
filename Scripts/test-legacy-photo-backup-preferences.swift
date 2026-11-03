// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

// Compile with SynctrainClient/LegacyPhotoBackupPreferences.swift, then run the executable.
@main private enum LegacyPhotoBackupPreferencesTests {
	static func main() {
		let suite = "synctrain-retired-photo-preferences-\(UUID().uuidString)"
		let otherSuite = suite + "-other"
		guard let defaults = UserDefaults(suiteName: suite), let other = UserDefaults(suiteName: otherSuite) else {
			preconditionFailure("Could not create disposable preference suites")
		}
		defer {
			defaults.removePersistentDomain(forName: suite)
			other.removePersistentDomain(forName: otherSuite)
		}
		let retired: [String: Any] = [
			"photoSyncSelectedAlbumID": "configured-album",
			"photoSyncFolderID": "ordinary-folder",
			"photoSyncSavedAlbumID": "saved-album",
			"photoSyncLastCompletedDate": 123.0,
			"photoSyncCategories": "photo,live,video",
			"photoSyncPurgeEnabled": true,
			"photoSyncPurgeAfterDays": 0,
			"photoBackupIncludeAllBurstAssets": true,
			"PhotoBackupFolderStructure": "byDateAndType",
			"photoSyncSubdirectoryPath": "existing-exports",
			"photoSyncMaxAgeDays": 180,
			"photoBackupTimeZone": "current",
			"photoBackupLastSuccessfulChangeToken": Data([1, 2, 3]),
			"photoBackupLivePhotoReplaceExtension": true,
		]
		let retained: [String: Any] = [
			"firstRunAt": 456.0, "lastRunBuild": 86, "loggingEnabled": true,
			"photoSyncUnrelated": "keep", "photoBackupUnrelated": "keep", "folderBookmark": Data([4, 5, 6]),
		]
		defaults.setPersistentDomain(retired.merging(retained) { _, value in value }, forName: suite)
		other.setPersistentDomain(retired, forName: otherSuite)

		func verifyRetainedPreferences() {
			let remaining = defaults.persistentDomain(forName: suite) ?? [:]
			precondition(NSDictionary(dictionary: remaining).isEqual(to: retained), "Cleanup changed unrelated settings")
			for key in retired.keys {
				precondition(defaults.object(forKey: key) == nil, "Retired key remains: \(key)")
			}
			precondition(
				NSDictionary(dictionary: other.persistentDomain(forName: otherSuite) ?? [:]).isEqual(to: retired),
				"Cleanup changed another preference suite")
		}
		for _ in 0..<2 {
			LegacyPhotoBackupPreferences.remove(from: defaults)
			verifyRetainedPreferences()
		}

		// A container migration that failed at launch can import these settings when startup is retried.
		for (key, value) in retired {
			defaults.set(value, forKey: key)
		}
		precondition(defaults.bool(forKey: "photoSyncPurgeEnabled"), "Retry fixture did not restore the retired settings")
		LegacyPhotoBackupPreferences.remove(from: defaults)
		verifyRetainedPreferences()
		print(
			"Passed: all 14 retired settings removed, unrelated settings preserved, cleanup idempotent and safe after reimport")
	}
}
