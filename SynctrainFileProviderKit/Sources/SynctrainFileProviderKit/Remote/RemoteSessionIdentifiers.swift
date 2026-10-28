// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The identifiers of the background `URLSession`s (see `BackgroundTransfers`). The app and the extension each own one;
/// both are derived from the app's bundle identifier, so that the app can recreate the extension's session to finish
/// its events, from `application(_:handleEventsForBackgroundURLSession:completionHandler:)`.
public enum RemoteSessionIdentifier {
	/// The session of the app (`<app bundle ID>.remote.app`)
	public static func app(appBundleIdentifier: String) -> String {
		"\(appBundleIdentifier).remote.app"
	}

	/// The session of the File Provider extension (`<app bundle ID>.remote.extension`)
	public static func fileProviderExtension(appBundleIdentifier: String) -> String {
		"\(appBundleIdentifier).remote.extension"
	}

	/// The app's bundle identifier, from the bundle of the app or of an extension embedded in it (whose identifier
	/// extends the app's by one component)
	public static func appBundleIdentifier(of bundle: Bundle = .main) -> String? {
		guard let identifier = bundle.bundleIdentifier else {
			return nil
		}
		if bundle.bundleURL.pathExtension == "appex", let dot = identifier.lastIndex(of: ".") {
			return String(identifier[..<dot])
		}
		return identifier
	}
}
