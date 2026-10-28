// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation

	/// Keeps copies of the certificate and private key the user chose for the remote file service in the app's
	/// container, where the service can always read them. The originals are remembered with bookmarks, so that a renewed
	/// certificate is picked up when the service starts.
	enum GatewayCertificateStore {
		enum ImportError: LocalizedError {
			case unreadable(String)
			case noCertificate
			case noPrivateKey

			var errorDescription: String? {
				switch self {
				case .unreadable(let name):
					return String(localized: "\(name) could not be read.")
				case .noCertificate:
					return String(localized: "None of the chosen files contains a PEM certificate.")
				case .noPrivateKey:
					return String(localized: "None of the chosen files contains a PEM private key.")
				}
			}
		}

		private static let certificateBookmarkKey = "remoteFileServiceCertificateBookmark"
		private static let keyBookmarkKey = "remoteFileServiceKeyBookmark"

		static var directory: URL {
			URL.applicationSupportDirectory.appending(path: "RemoteFileService", directoryHint: .isDirectory)
		}

		static var certificateURL: URL {
			directory.appending(path: "certificate.pem", directoryHint: .notDirectory)
		}

		static var keyURL: URL {
			directory.appending(path: "key.pem", directoryHint: .notDirectory)
		}

		static var hasFiles: Bool {
			FileManager.default.fileExists(atPath: certificateURL.path(percentEncoded: false))
				&& FileManager.default.fileExists(atPath: keyURL.path(percentEncoded: false))
		}

		/// Copies the certificate (chain) and private key from the chosen files: one file holding both, or two files.
		/// The files are read away from the main actor.
		@concurrent static func importFiles(_ urls: [URL]) async throws {
			var certificate: (url: URL, data: Data)? = nil
			var key: (url: URL, data: Data)? = nil
			for url in urls {
				let data = try read(url)
				if certificate == nil && containsCertificate(data) {
					certificate = (url, data)
				}
				if key == nil && containsPrivateKey(data) {
					key = (url, data)
				}
			}
			guard let certificate else {
				throw ImportError.noCertificate
			}
			guard let key else {
				throw ImportError.noPrivateKey
			}
			try write(certificate: certificate.data, key: key.data)
			rememberOriginals(certificate: certificate.url, key: key.url)
		}

		/// Copies the chosen files again if they changed since they were copied (as when a certificate is renewed). The
		/// files are read away from the main actor.
		@concurrent static func refreshFromOriginals() async {
			guard let certificate = readBookmarked(certificateBookmarkKey), containsCertificate(certificate),
				let key = readBookmarked(keyBookmarkKey), containsPrivateKey(key)
			else {
				return
			}
			let current = try? Data(contentsOf: certificateURL)
			let currentKey = try? Data(contentsOf: keyURL)
			guard certificate != current || key != currentKey else {
				return
			}
			do {
				try write(certificate: certificate, key: key)
				Log.info("Copied the renewed certificate for the remote file service")
			}
			catch {
				Log.warn("Could not copy the renewed certificate for the remote file service: \(error.localizedDescription)")
			}
		}

		private static func containsCertificate(_ data: Data) -> Bool {
			String(decoding: data, as: UTF8.self).contains("-----BEGIN CERTIFICATE-----")
		}

		private static func containsPrivateKey(_ data: Data) -> Bool {
			let text = String(decoding: data, as: UTF8.self)
			return text.contains("-----BEGIN ") && text.contains("PRIVATE KEY-----")
		}

		private static func read(_ url: URL) throws -> Data {
			let accessing = url.startAccessingSecurityScopedResource()
			defer {
				if accessing {
					url.stopAccessingSecurityScopedResource()
				}
			}
			do {
				return try Data(contentsOf: url)
			}
			catch {
				throw ImportError.unreadable(url.lastPathComponent)
			}
		}

		/// Writes the key first: the service reloads the pair when the certificate file changes
		private static func write(certificate: Data, key: Data) throws {
			try FileManager.default.createDirectory(
				at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
			try writePrivately(key, to: keyURL)
			try certificate.write(to: certificateURL, options: [.atomic])
		}

		/// Replaces a file with one that only the owner can read from the moment it is created, then moves it into place
		private static func writePrivately(_ data: Data, to url: URL) throws {
			let path = url.path(percentEncoded: false)
			let temporary = path + ".tmp"
			unlink(temporary)
			let descriptor = open(temporary, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0o600)
			guard descriptor >= 0 else {
				throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
			}
			let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
			do {
				try handle.write(contentsOf: data)
				try handle.synchronize()
				try handle.close()
				guard rename(temporary, path) == 0 else {
					throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
				}
			}
			catch {
				unlink(temporary)
				throw error
			}
		}

		/// Remembers where the copied files came from, so that renewed files are copied again. Either both originals
		/// are remembered or neither: a renewed certificate must not be paired with an old key.
		private static func rememberOriginals(certificate: URL, key: URL) {
			let defaults = UserDefaults.standard
			do {
				let certificateBookmark = try bookmark(certificate)
				let keyBookmark = try bookmark(key)
				defaults.set(certificateBookmark, forKey: certificateBookmarkKey)
				defaults.set(keyBookmark, forKey: keyBookmarkKey)
			}
			catch {
				defaults.removeObject(forKey: certificateBookmarkKey)
				defaults.removeObject(forKey: keyBookmarkKey)
				Log.warn(
					"Could not remember the chosen certificate files; renewed files must be chosen again: \(error.localizedDescription)"
				)
			}
		}

		private static func bookmark(_ url: URL) throws -> Data {
			let accessing = url.startAccessingSecurityScopedResource()
			defer {
				if accessing {
					url.stopAccessingSecurityScopedResource()
				}
			}
			return try url.bookmarkData(options: [.withSecurityScope, .securityScopeAllowOnlyReadAccess])
		}

		private static func readBookmarked(_ key: String) -> Data? {
			guard let saved = UserDefaults.standard.data(forKey: key) else {
				return nil
			}
			var isStale = false
			let url: URL
			do {
				url = try URL(resolvingBookmarkData: saved, options: [.withSecurityScope], bookmarkDataIsStale: &isStale)
			}
			catch {
				Log.warn("Could not find a chosen certificate file again: \(error.localizedDescription)")
				return nil
			}
			let accessing = url.startAccessingSecurityScopedResource()
			defer {
				if accessing {
					url.stopAccessingSecurityScopedResource()
				}
			}
			if isStale {
				do {
					UserDefaults.standard.set(try bookmark(url), forKey: key)
				}
				catch {
					Log.warn("Could not renew the bookmark of a chosen certificate file: \(error.localizedDescription)")
				}
			}
			do {
				return try Data(contentsOf: url)
			}
			catch {
				Log.warn("Could not read the chosen certificate file \(url.lastPathComponent): \(error.localizedDescription)")
				return nil
			}
		}
	}
#endif
