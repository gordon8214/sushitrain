// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

enum LegacyPhotoBackupPreferences {
	/// Retire only the former backup settings. Exported files and the Photos library are never opened or changed.
	static func remove(from defaults: UserDefaults) {
		for key in [
			"photoSyncSelectedAlbumID", "photoSyncFolderID", "photoSyncSavedAlbumID", "photoSyncLastCompletedDate",
			"photoSyncCategories", "photoSyncPurgeEnabled", "photoSyncPurgeAfterDays", "photoBackupIncludeAllBurstAssets",
			"PhotoBackupFolderStructure", "photoSyncSubdirectoryPath", "photoSyncMaxAgeDays", "photoBackupTimeZone",
			"photoBackupLastSuccessfulChangeToken", "photoBackupLivePhotoReplaceExtension",
		] {
			defaults.removeObject(forKey: key)
		}
	}
}
