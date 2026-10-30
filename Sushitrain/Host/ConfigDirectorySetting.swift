// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation

	/// The configuration folder the user chose on macOS, if any.
	///
	/// Outside the App Sandbox it is a path. In the sandbox it is a security-scoped bookmark, as in earlier versions; the
	/// first time the app runs outside the sandbox, the bookmark's path becomes the setting.
	enum ConfigDirectorySetting {
		/// The chosen folder's path; empty when the user chose the default location
		static let pathKey = "configDirectoryPath"
		static let bookmarkKey = "configDirectoryBookmark"

		/// The path of the folder chosen outside the sandbox, or nil for the default location. Read the preferences
		/// migrated from the sandbox (ContainerMigration) before this, since the bookmark comes with them.
		static var path: String? {
			let defaults = UserDefaults.standard
			if let path = defaults.string(forKey: pathKey) {
				return path.isEmpty ? nil : path
			}
			// The bookmark stays, for a sandboxed build to use
			guard let bookmark = defaults.data(forKey: bookmarkKey) else {
				return nil
			}
			guard let path = FolderAccess.path(fromBookmark: bookmark) else {
				Log.warn("The configuration folder chosen in the sandbox cannot be found; using the default location")
				return nil
			}
			Log.info("Using the configuration folder chosen in the sandbox: \(path)")
			defaults.set(path, forKey: pathKey)
			return path
		}

		/// Whether a folder other than the default location was chosen (in the sandbox, with a bookmark)
		static var isChosen: Bool {
			if FolderAccess.usesSecurityScope {
				return UserDefaults.standard.data(forKey: bookmarkKey) != nil
			}
			return self.path != nil
		}

		/// Chooses a folder outside the sandbox, or the default location (nil), which in the sandbox removes the
		/// bookmark
		static func choose(_ url: URL?) {
			let defaults = UserDefaults.standard
			if FolderAccess.usesSecurityScope {
				precondition(url == nil, "the sandbox remembers a chosen folder with a bookmark")
				defaults.removeObject(forKey: bookmarkKey)
				return
			}
			defaults.set(url?.path(percentEncoded: false) ?? "", forKey: pathKey)
			defaults.removeObject(forKey: bookmarkKey)
		}
	}
#endif
