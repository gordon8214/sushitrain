// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// The places where Synctrain keeps its own caches and temporary files.
///
/// Outside the App Sandbox, the system caches and temporary directories are shared with every other app, so Synctrain
/// keeps its files in subdirectories named after its bundle identifier and only ever deletes inside those.
public struct AppLocations: Sendable {
	/// Prefix of the folders the share extension stages shared items in, in the app group's caches directory.
	public static let shareHandoffPrefix = "SynctrainShare-"

	/// Prefixes of the temporary items earlier versions created directly in the system temporary directory.
	static let legacyTemporaryPrefixes = ["Downloads-", "Export-", "thumbnail-", "Synctrain-support-bundle-"]

	public let bundleIdentifier: String

	/// Synctrain's own caches directory (`<system caches>/<bundle id>`).
	public let caches: URL

	/// Synctrain's own temporary directory (`<system temporary directory>/<bundle id>`).
	public let temporary: URL

	/// The app group's caches directory, where the share extension stages items for the app, if there is an app group.
	public let groupCaches: URL?

	/// The thumbnail cache used before it moved into `caches`.
	let legacyThumbnails: URL

	/// Where earlier versions created temporary items.
	let systemTemporary: URL

	public init(bundleIdentifier: String, systemCaches: URL, systemTemporary: URL, groupCaches: URL?) {
		precondition(Self.isValidBundleIdentifier(bundleIdentifier), "invalid bundle identifier")
		self.bundleIdentifier = bundleIdentifier
		self.caches = systemCaches.appending(path: bundleIdentifier, directoryHint: .isDirectory)
		self.temporary = systemTemporary.appending(path: bundleIdentifier, directoryHint: .isDirectory)
		self.groupCaches = groupCaches
		self.legacyThumbnails = systemCaches.appending(path: "thumbnails", directoryHint: .isDirectory)
		self.systemTemporary = systemTemporary
	}

	/// Whether `identifier` is a bundle identifier, which makes it a single, real path component.
	static func isValidBundleIdentifier(_ identifier: String) -> Bool {
		return identifier.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "." || $0 == "-") }
			&& identifier.contains { $0.isLetter || $0.isNumber }
	}

	/// The caches directory of the app group `appGroup`, shared by the app and its extensions.
	public static func groupCaches(appGroup: String) -> URL? {
		return FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup)?
			.appending(path: "Library/Caches", directoryHint: .isDirectory)
	}

	/// Creates a new folder in the app group's caches directory, for the share extension to stage shared items in.
	public static func makeShareHandoffDirectory(in groupCaches: URL) throws -> URL {
		let directory = groupCaches.appending(
			path: shareHandoffPrefix + UUID().uuidString, directoryHint: .isDirectory)
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		return directory
	}

	/// The shared thumbnail cache.
	public var thumbnails: URL {
		self.caches.appending(path: "thumbnails", directoryHint: .isDirectory)
	}

	/// The remote file service's content spool and thumbnails (macOS), which it rebuilds when they are cleared.
	public var remoteFileService: URL {
		self.caches.appending(path: "RemoteFileService", directoryHint: .isDirectory)
	}

	/// Creates Synctrain's caches and temporary directories.
	public func prepare() throws {
		let fileManager = FileManager.default
		try fileManager.createDirectory(at: self.caches, withIntermediateDirectories: true)
		try fileManager.createDirectory(at: self.temporary, withIntermediateDirectories: true)
	}

	/// Moves the thumbnail cache from the system caches root into `caches`, or removes it if both exist. Only call this
	/// where that root is private to the app (iOS, or the App Sandbox), since elsewhere it may hold another app's files.
	public func moveLegacyThumbnails() throws {
		let fileManager = FileManager.default
		guard fileManager.fileExists(atPath: self.legacyThumbnails.path(percentEncoded: false)) else {
			return
		}
		if fileManager.fileExists(atPath: self.thumbnails.path(percentEncoded: false)) {
			try fileManager.removeItem(at: self.legacyThumbnails)
		}
		else {
			try fileManager.createDirectory(at: self.caches, withIntermediateDirectories: true)
			try fileManager.moveItem(at: self.legacyThumbnails, to: self.thumbnails)
		}
	}

	/// Creates a new, uniquely named directory inside `temporary`.
	public func makeTemporaryDirectory(prefix: String) throws -> URL {
		let directory = self.temporary.appending(
			path: "\(prefix)\(UUID().uuidString)", directoryHint: .isDirectory)
		try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
		return directory
	}

	/// The folder the share extension staged `url` in, if it is a staged shared item.
	public func shareHandoffDirectory(containing url: URL) -> URL? {
		guard let groupCaches = self.groupCaches else {
			return nil
		}
		let base = groupCaches.standardizedFileURL.pathComponents
		let components = url.standardizedFileURL.pathComponents
		guard components.count > base.count, Array(components.prefix(base.count)) == base,
			components[base.count].hasPrefix(Self.shareHandoffPrefix)
		else {
			return nil
		}
		return groupCaches.appending(path: components[base.count], directoryHint: .isDirectory)
	}

	/// Removes everything in `caches`. Returns the errors for items that could not be removed.
	@discardableResult public func clearCaches() -> [any Error] {
		return Self.removeContents(of: self.caches, named: self.bundleIdentifier)
	}

	/// Removes everything in `temporary`, and the share extension's staged items, except the items that contain one of
	/// `keeping` (such as shared files waiting to be imported). Returns the errors for items that could not be removed.
	@discardableResult public func clearTemporaryFiles(keeping: [URL] = []) -> [any Error] {
		let kept = keeping.map { $0.standardizedFileURL.pathComponents }
		let isUnused = { (item: URL) -> Bool in
			let components = item.standardizedFileURL.pathComponents
			return !kept.contains { Array($0.prefix(components.count)) == components }
		}
		var errors = Self.removeContents(of: self.temporary, named: self.bundleIdentifier, where: isUnused)
		if let groupCaches = self.groupCaches {
			errors += Self.removeContents(of: groupCaches) {
				$0.lastPathComponent.hasPrefix(Self.shareHandoffPrefix) && isUnused($0)
			}
		}
		return errors
	}

	/// Removes temporary files and staged shared items that were last modified before `date`, such as downloads
	/// handed to another app and shares that were never imported.
	///
	/// - Parameter includingLegacyItems: also remove what earlier versions left in the system temporary directory.
	///   Only pass true where that directory is private to the app (iOS, or the App Sandbox).
	@discardableResult public func pruneTemporaryFiles(modifiedBefore date: Date, includingLegacyItems: Bool)
		-> [any Error]
	{
		var errors = Self.removeContents(of: self.temporary, named: self.bundleIdentifier) {
			Self.modificationDate($0) < date
		}
		if let groupCaches = self.groupCaches {
			errors += Self.removeContents(of: groupCaches) {
				$0.lastPathComponent.hasPrefix(Self.shareHandoffPrefix) && Self.modificationDate($0) < date
			}
		}
		if includingLegacyItems {
			errors += Self.removeContents(of: self.systemTemporary) {
				Self.isLegacyTemporaryItem($0.lastPathComponent) && Self.modificationDate($0) < date
			}
		}
		return errors
	}

	/// Whether earlier versions created an item with this name in the system temporary directory.
	static func isLegacyTemporaryItem(_ name: String) -> Bool {
		return Self.legacyTemporaryPrefixes.contains { name.hasPrefix($0) } || UUID(uuidString: name) != nil
	}

	/// The directories whose sizes the temporary files size adds up.
	public var temporaryDirectories: [URL] {
		return [self.temporary] + self.shareHandoffs
	}

	private var shareHandoffs: [URL] {
		guard let groupCaches = self.groupCaches,
			let items = try? FileManager.default.contentsOfDirectory(
				at: groupCaches, includingPropertiesForKeys: nil, options: [])
		else {
			return []
		}
		return items.filter { $0.lastPathComponent.hasPrefix(Self.shareHandoffPrefix) }
	}

	private static func modificationDate(_ url: URL) -> Date {
		return (try? url.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate) ?? .distantPast
	}

	/// Removes the items in `directory` that `include` selects. When `name` is given, refuses to touch a directory with
	/// another name, so a wrong path can never empty a shared directory.
	static func removeContents(
		of directory: URL, named name: String? = nil, where include: (URL) -> Bool = { _ in true }
	) -> [any Error] {
		if let name, directory.lastPathComponent != name {
			return [CocoaError(.fileWriteNoPermission, userInfo: [NSURLErrorKey: directory])]
		}
		let fileManager = FileManager.default
		let items: [URL]
		do {
			items = try fileManager.contentsOfDirectory(
				at: directory, includingPropertiesForKeys: [.contentModificationDateKey], options: [])
		}
		catch CocoaError.fileReadNoSuchFile {
			return []
		}
		catch {
			return [error]
		}

		var errors: [any Error] = []
		for item in items where include(item) {
			do {
				try fileManager.removeItem(at: item)
			}
			catch {
				errors.append(error)
			}
		}
		return errors
	}
}
