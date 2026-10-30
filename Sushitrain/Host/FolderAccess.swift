// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SynctrainHostKit

/// How the app reaches files and folders the user chose.
///
/// On iOS and in the App Sandbox, a chosen location can only be used while its security scope is held, and is
/// remembered with a security-scoped bookmark. The Mac app outside the sandbox reaches whatever the user can, by its
/// path: it takes no scopes, and remembers paths.
enum FolderAccess {
	/// Whether chosen locations need security scopes and bookmarks. This follows the same check as the engine's
	/// locations (EngineLocations.current), so outside the sandbox no folder is external and no bookmark is looked for.
	static let usesSecurityScope = AppLocations.systemDirectoriesArePrivate

	/// Starts accessing a location the user chose, where that takes a security scope. Returns whether access started,
	/// and so must be stopped.
	static func startAccessing(_ url: URL) -> Bool {
		guard usesSecurityScope else {
			return false
		}
		return url.startAccessingSecurityScopedResource()
	}

	/// The path a bookmark (security-scoped or not) points to: where it resolves to, which follows a moved item, or else
	/// the path recorded in the bookmark itself (as for an item on a disk that is not connected). Outside the sandbox a
	/// bookmark made in it still gives its path this way.
	static func path(fromBookmark bookmark: Data) -> String? {
		var isStale = false
		if let url = try? URL(
			resolvingBookmarkData: bookmark, options: [.withoutUI, .withoutMounting], bookmarkDataIsStale: &isStale)
		{
			return url.path(percentEncoded: false)
		}
		guard let path = URL.resourceValues(forKeys: [.pathKey], fromBookmarkData: bookmark)?.path, !path.isEmpty else {
			return nil
		}
		return path
	}
}
