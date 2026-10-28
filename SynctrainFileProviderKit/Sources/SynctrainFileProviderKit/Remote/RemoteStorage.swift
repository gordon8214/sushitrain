// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Where remote content lives in the app group: staged edits, transfers in progress, and verified cached content. All
/// of it is outside `Library/Caches` and `tmp`, which the system may purge, and paths are stored relative to `baseURL`
/// in the index (the container's location can change, for instance after a restore).
public struct RemoteStorage: Sendable, Hashable {
	public let baseURL: URL

	public init(baseURL: URL) {
		self.baseURL = baseURL
	}

	public init(container: SharedContainer) {
		baseURL = container.fileProviderURL.appending(path: "Remote", directoryHint: .isDirectory)
	}

	/// Immutable staged copies of edited files, until their operations are resolved
	public var stagingURL: URL {
		baseURL.appending(path: "Staged", directoryHint: .isDirectory)
	}

	/// Transfers in progress: partial files, manifests and ranges, one directory per transfer
	public var transfersURL: URL {
		baseURL.appending(path: "Transfers", directoryHint: .isDirectory)
	}

	/// Verified content, named after its content identity
	public var contentURL: URL {
		baseURL.appending(path: "Content", directoryHint: .isDirectory)
	}

	static let stagingDirectory = "Staged"
	static let transfersDirectory = "Transfers"

	public func url(relativePath: String) -> URL {
		baseURL.appending(path: relativePath, directoryHint: .notDirectory)
	}

	/// The relative path of a URL below `baseURL`, or nil when it is elsewhere
	public func relativePath(of url: URL) -> String? {
		let base = baseURL.standardizedFileURL.path(percentEncoded: false)
		let path = url.standardizedFileURL.path(percentEncoded: false)
		let prefix = base.hasSuffix("/") ? base : base + "/"
		guard path.hasPrefix(prefix) else {
			return nil
		}
		return String(path.dropFirst(prefix.count))
	}

	/// The verified copy of content with the identity, if it was downloaded (nil for identities that cannot name a file)
	public func cachedContentURL(_ contentID: ContentIdentity) -> URL? {
		guard contentID.isSafeFileName else {
			return nil
		}
		return contentURL.appending(path: contentID.rawValue, directoryHint: .notDirectory)
	}

	/// The directory of a transfer
	func transferURL(_ transferID: String) -> URL {
		transfersURL.appending(path: transferID, directoryHint: .isDirectory)
	}

	/// Creates the directories. On iOS, files created inside inherit their protection, which lets the app and the
	/// extension read them while the device is locked, once it has been unlocked after booting.
	public func createDirectories() throws {
		for url in [baseURL, stagingURL, transfersURL, contentURL] {
			try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
			#if os(iOS)
				try FileManager.default.setAttributes(
					[.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
					ofItemAtPath: url.path(percentEncoded: false))
			#endif
		}
	}
}
