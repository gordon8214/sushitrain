// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Locations inside the app group container that the app and the File Provider extension share
public struct SharedContainer: Sendable, Hashable {
	/// The Info.plist key holding the app group identifier (set from the SYNCTRAIN_APP_GROUP build setting)
	public static let appGroupInfoKey = "SynctrainAppGroup"

	/// The identifier of the File Provider domain
	public static let domainIdentifier = "synctrain"

	public let baseURL: URL

	public init(baseURL: URL) {
		self.baseURL = baseURL
	}

	/// The shared container of the app group named in the bundle's Info.plist, or nil when it is not available
	public static func appGroup(bundle: Bundle = .main) -> SharedContainer? {
		guard let groupID = bundle.object(forInfoDictionaryKey: appGroupInfoKey) as? String, !groupID.isEmpty,
			let url = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: groupID)
		else {
			return nil
		}
		return SharedContainer(baseURL: url)
	}

	public var fileProviderURL: URL {
		baseURL.appending(path: "FileProvider", directoryHint: .isDirectory)
	}

	public var indexURL: URL {
		fileProviderURL.appending(path: "gateway.sqlite", directoryHint: .notDirectory)
	}

	/// Creates the shared directory. Files created inside inherit their data protection class, which allows the
	/// extension and the app to read them while the device is locked, after it has been unlocked once.
	public func createDirectories() throws {
		try FileManager.default.createDirectory(at: fileProviderURL, withIntermediateDirectories: true)
		#if os(iOS)
			try FileManager.default.setAttributes(
				[.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
				ofItemAtPath: fileProviderURL.path(percentEncoded: false))
		#endif
	}
}
