// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation

/// Moves the engine data a sandboxed Synctrain kept in its container to where the app keeps it outside the sandbox.
///
/// The data is copied (cloned, on APFS) into a staging folder next to the destination, checked, and then renamed into
/// place, so that the destination appears whole or not at all. The container is left as it is: it is the copy to go
/// back to.
public struct ContainerMigration: Sendable {
	public enum State: Equatable, Sendable {
		/// The container has no engine configuration or identity
		case nothingToMigrate
		/// The data was migrated before
		case done
		/// The destination already has an engine configuration or identity of its own, which is never replaced
		case destinationInUse
		/// The container can't be read (the error number), as when macOS protects it from the app
		case unreadable(Int32)
		case needed
	}

	public enum MigrationError: Error, Equatable, LocalizedError {
		/// A sandboxed Synctrain still runs on the container's data
		case containerInUse
		/// The container can't be read (the error number)
		case containerUnreadable(Int32)
		case destinationInUse
		/// A copied item differs from the original
		case copyDiffers(String)
		/// An item is a symbolic link, which would lead back into the container or elsewhere
		case symbolicLink(String)
		/// The device certificate and key did not arrive intact
		case identityDiffers

		public var errorDescription: String? {
			switch self {
			case .containerInUse: return "another app uses the data"
			case .containerUnreadable(let code): return String(cString: strerror(code))
			case .destinationInUse: return "the destination has engine data of its own"
			case .copyDiffers(let item): return "the copy of \(item) differs from the original"
			case .symbolicLink(let item): return "\(item) is a symbolic link"
			case .identityDiffers: return "the device identity was not copied intact"
			}
		}
	}

	/// What a migration did, kept in the destination
	public struct Receipt: Codable, Equatable, Sendable {
		public var migrated: Date
		public var source: String
		public var items: [String]
		/// The SHA-256 of the device certificate, to check that the device identity was carried over
		public var certificateSHA256: String?
	}

	public static let receiptName = "container-migration.json"

	/// The items that make up an engine configuration and device identity
	static let identityItems = ["config.xml", "cert.pem", "key.pem"]

	/// What the engine keeps in its configuration directory: its configuration, identity, databases, and the remote
	/// file service's state. Anything else there is left behind.
	static let engineItems = [
		"config.xml", "cert.pem", "key.pem", "https-cert.pem", "https-key.pem", "index-v2", "index-v0.14.0.db",
		"remote-managed.json", "synctrain-gateway", "RemoteFileService",
	]

	/// Caches inside `engineItems`, which are rebuilt elsewhere
	static let skippedItems = ["synctrain-gateway/content", "synctrain-gateway/thumbnails"]

	/// The engine data in the container
	public let source: URL

	/// The app's engine data folder
	public let destination: URL

	/// - Parameters:
	///   - source: the configuration directory in the container (its Library/Application Support)
	///   - destination: the configuration directory outside the sandbox
	public init(source: URL, destination: URL) {
		self.source = source
		self.destination = destination
	}

	/// Where the copy is assembled before it is moved into place
	public var staging: URL {
		self.destination.deletingLastPathComponent().appending(
			path: self.destination.lastPathComponent + ".migrating", directoryHint: .isDirectory)
	}

	public var receiptURL: URL {
		self.destination.appending(path: Self.receiptName, directoryHint: .notDirectory)
	}

	public func state() -> State {
		if FileManager.default.fileExists(atPath: self.receiptURL.path(percentEncoded: false)) {
			return .done
		}
		// An identity without a configuration (as when one was set aside) counts too. Only a missing item is absent:
		// one that can't be looked at may well be there.
		var found = false
		for item in Self.identityItems {
			switch Self.presence(of: self.source.appending(path: item)) {
			case 0:
				found = true
			case ENOENT, ENOTDIR:
				continue
			case let code:
				return .unreadable(code)
			}
		}
		if !found {
			return .nothingToMigrate
		}
		if self.destinationHasEngineData {
			return .destinationInUse
		}
		return .needed
	}

	/// 0 when an item exists, otherwise the error number of looking at it (ENOENT when it does not exist)
	public static func presence(of url: URL) -> Int32 {
		var info = stat()
		return lstat(url.path(percentEncoded: false), &info) == 0 ? 0 : errno
	}

	/// The receipt of the migration, if one was made
	public func receipt() -> Receipt? {
		guard let data = try? Data(contentsOf: self.receiptURL) else {
			return nil
		}
		let decoder = JSONDecoder()
		decoder.dateDecodingStrategy = .iso8601
		return try? decoder.decode(Receipt.self, from: data)
	}

	private var destinationHasEngineData: Bool {
		(Self.identityItems + ["index-v2"]).contains {
			FileManager.default.fileExists(atPath: self.destination.appending(path: $0).path(percentEncoded: false))
		}
	}

	/// Copies the engine data into place. Call this before the engine starts, and only when `state()` is `.needed`.
	@discardableResult public func run(now: Date = Date()) throws -> Receipt {
		let fileManager = FileManager.default
		// Held throughout, so that no sandboxed engine starts on the container's data meanwhile
		let lock = try Self.lockContainer(self.source.appending(path: "syncthing.lock"))
		defer {
			if lock >= 0 {
				flock(lock, LOCK_UN)
				close(lock)
			}
		}
		switch self.state() {
		case .needed:
			break
		case .unreadable(let code):
			throw MigrationError.containerUnreadable(code)
		case .nothingToMigrate:
			throw MigrationError.containerUnreadable(ENOENT)
		case .destinationInUse, .done:
			// Set aside, an earlier migration's data would be lost from view
			throw MigrationError.destinationInUse
		}

		// What an interrupted migration left
		try? fileManager.removeItem(at: self.staging)
		do {
			try fileManager.createDirectory(
				at: self.staging, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
			var copied: [String] = []
			for item in Self.engineItems {
				let from = self.source.appending(path: item)
				switch Self.presence(of: from) {
				case 0:
					try self.copy(from, to: self.staging.appending(path: item), relativePath: item)
					copied.append(item)
				case ENOENT:
					continue
				case let code:
					throw MigrationError.containerUnreadable(code)
				}
			}
			try self.checkIdentity()

			let receipt = Receipt(
				migrated: now, source: self.source.path(percentEncoded: false), items: copied,
				certificateSHA256: Self.sha256(of: self.staging.appending(path: "cert.pem")))
			let encoder = JSONEncoder()
			encoder.dateEncodingStrategy = .iso8601
			encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
			try encoder.encode(receipt).write(
				to: self.staging.appending(path: Self.receiptName), options: [.atomic])
			try self.moveIntoPlace(now: now)
			return receipt
		}
		catch {
			try? fileManager.removeItem(at: self.staging)
			throw error
		}
	}

	/// Checks that the device certificate and key the container has are in the copy, unchanged
	private func checkIdentity() throws {
		for item in ["cert.pem", "key.pem"] {
			guard let original = Self.sha256(of: self.source.appending(path: item)) else {
				continue
			}
			guard Self.sha256(of: self.staging.appending(path: item)) == original else {
				throw MigrationError.identityDiffers
			}
		}
	}

	/// Renames the staging folder to the destination. A destination without engine data (what an earlier start or a
	/// failed attempt left, such as a lock file) is set aside first, never removed.
	private func moveIntoPlace(now: Date) throws {
		let staging = self.staging.path(percentEncoded: false)
		let destination = self.destination.path(percentEncoded: false)
		// Replaces an empty directory
		if rename(staging, destination) == 0 {
			return
		}
		guard errno == ENOTEMPTY || errno == EEXIST else {
			throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
		}
		let stamp = Int(now.timeIntervalSince1970)
		let aside = destination + ".premigration-\(stamp)-\(UUID().uuidString.prefix(8))"
		guard rename(destination, aside) == 0, rename(staging, destination) == 0 else {
			throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
		}
	}

	/// Copies an item, leaving out `skippedItems`, and checks that each copied file has the original's contents
	private func copy(_ from: URL, to: URL, relativePath: String) throws {
		let fileManager = FileManager.default
		let values = try from.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey])
		if values.isSymbolicLink == true {
			// The engine makes no links; copying one would point back into the container, and skipping it could lose
			// the identity or database
			throw MigrationError.symbolicLink(relativePath)
		}
		if values.isDirectory == true {
			try fileManager.createDirectory(
				at: to, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
			for name in try fileManager.contentsOfDirectory(atPath: from.path(percentEncoded: false)) {
				let child = relativePath + "/" + name
				if Self.skippedItems.contains(child) {
					continue
				}
				try self.copy(from.appending(path: name), to: to.appending(path: name), relativePath: child)
			}
			return
		}
		try fileManager.copyItem(at: from, to: to)
		guard Self.sameContents(from, to) else {
			throw MigrationError.copyDiffers(relativePath)
		}
	}

	/// Compares two files a megabyte at a time, as databases may be many gigabytes
	private static func sameContents(_ a: URL, _ b: URL) -> Bool {
		guard let sizeA = try? a.resourceValues(forKeys: [.fileSizeKey]).fileSize,
			let sizeB = try? b.resourceValues(forKeys: [.fileSizeKey]).fileSize, sizeA == sizeB,
			let first = try? FileHandle(forReadingFrom: a), let second = try? FileHandle(forReadingFrom: b)
		else {
			return false
		}
		defer {
			try? first.close()
			try? second.close()
		}
		do {
			while true {
				// No data at the end
				let chunkA = try first.read(upToCount: 1 << 20) ?? Data()
				let chunkB = try second.read(upToCount: 1 << 20) ?? Data()
				if chunkA != chunkB {
					return false
				}
				if chunkA.isEmpty {
					return true
				}
			}
		}
		catch {
			return false
		}
	}

	public static func sha256(of url: URL) -> String? {
		guard let data = try? Data(contentsOf: url) else {
			return nil
		}
		return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
	}

	/// Takes the lock Syncthing takes on its configuration directory (a `flock`), and returns its file descriptor to
	/// release it with, or -1 when there is no lock file
	static func lockContainer(_ lockFile: URL) throws -> Int32 {
		let descriptor = open(lockFile.path(percentEncoded: false), O_RDONLY | O_CLOEXEC)
		guard descriptor >= 0 else {
			if errno == ENOENT {
				return -1
			}
			throw MigrationError.containerUnreadable(errno)
		}
		guard flock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
			let code = errno
			close(descriptor)
			throw code == EWOULDBLOCK ? MigrationError.containerInUse : MigrationError.containerUnreadable(code)
		}
		return descriptor
	}

	/// Whether another process holds the lock Syncthing takes on its configuration directory (a `flock`)
	public static func isLocked(_ lockFile: URL) -> Bool {
		let descriptor = open(lockFile.path(percentEncoded: false), O_RDONLY | O_CLOEXEC)
		guard descriptor >= 0 else {
			return false
		}
		defer { close(descriptor) }
		if flock(descriptor, LOCK_EX | LOCK_NB) != 0 {
			return errno == EWOULDBLOCK
		}
		flock(descriptor, LOCK_UN)
		return false
	}
}

/// The preferences a sandboxed Synctrain kept in its container
public enum ContainerPreferences {
	/// The container's values to add to the app's preferences: those it does not have yet, so that a value set since
	/// is kept. AppKit's own state (window frames, the open panel's last folder, which was inside the container) stays
	/// behind.
	public static func valuesToImport(from container: [String: Any], current: [String: Any]) -> [String: Any] {
		return container.filter { current[$0.key] == nil && !$0.key.hasPrefix("NS") }
	}
}
