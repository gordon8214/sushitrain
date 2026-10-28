// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SystemConfiguration

	/// How the remote file service is reached, stored in the user defaults. Changes apply when the service starts.
	struct GatewaySettings: Equatable, Sendable {
		enum CertificateMode: String, CaseIterable, Sendable {
			/// A certificate the service creates, whose public key devices pin when pairing
			case selfSigned = "selfSigned"
			/// Certificate and key files the user chose (copied into the app's container)
			case files = "files"
		}

		static let defaultPort = 20920
		static let defaultContentCacheMiB = 8192
		static let validPorts = 1...65535

		var port = Self.defaultPort
		/// Where devices connect, for pairing codes (empty: this Mac's local host name)
		var publicHost = ""
		/// The name paired devices show (empty: this Mac's name)
		var serviceName = ""
		var certificateMode = CertificateMode.selfSigned
		var contentCacheMiB = Self.defaultContentCacheMiB

		private enum Key {
			static let port = "remoteFileServicePort"
			static let publicHost = "remoteFileServicePublicHost"
			static let serviceName = "remoteFileServiceName"
			static let certificateMode = "remoteFileServiceCertificateMode"
			static let contentCacheMiB = "remoteFileServiceContentCacheMiB"
		}

		static func load() -> Self {
			let defaults = UserDefaults.standard
			var settings = Self()
			if let port = defaults.object(forKey: Key.port) as? Int, validPorts.contains(port) {
				settings.port = port
			}
			settings.publicHost = defaults.string(forKey: Key.publicHost) ?? ""
			settings.serviceName = defaults.string(forKey: Key.serviceName) ?? ""
			if let mode = defaults.string(forKey: Key.certificateMode).flatMap(CertificateMode.init(rawValue:)) {
				settings.certificateMode = mode
			}
			if let cache = defaults.object(forKey: Key.contentCacheMiB) as? Int, cache > 0 {
				settings.contentCacheMiB = cache
			}
			return settings
		}

		func save() {
			let defaults = UserDefaults.standard
			defaults.set(port, forKey: Key.port)
			defaults.set(publicHost, forKey: Key.publicHost)
			defaults.set(serviceName, forKey: Key.serviceName)
			defaults.set(certificateMode.rawValue, forKey: Key.certificateMode)
			defaults.set(contentCacheMiB, forKey: Key.contentCacheMiB)
		}

		var isValid: Bool {
			Self.validPorts.contains(port)
		}

		@MainActor var effectivePublicHost: String {
			let host = publicHost.trimmingCharacters(in: .whitespacesAndNewlines)
			return host.isEmpty ? Self.localHostName : host
		}

		@MainActor var effectiveServiceName: String {
			let name = serviceName.trimmingCharacters(in: .whitespacesAndNewlines)
			return name.isEmpty ? Self.computerName : name
		}

		/// This Mac's names, read once and again whenever the service starts rather than in every view update
		@MainActor private static var systemNames = SystemNames()

		/// This Mac's Bonjour name, which devices on the same network can reach
		@MainActor static var localHostName: String { systemNames.localHostName }

		/// This Mac's name, as set in System Settings
		@MainActor static var computerName: String { systemNames.computerName }

		/// Reads this Mac's names again, as they may have changed in System Settings
		@MainActor static func refreshSystemNames() {
			systemNames = SystemNames()
		}
	}

	private struct SystemNames {
		let localHostName: String
		let computerName: String

		init() {
			if let name = SCDynamicStoreCopyLocalHostName(nil) as String?, !name.isEmpty {
				localHostName = name + ".local"
			}
			else {
				localHostName = Self.kernelHostName() ?? "localhost"
			}
			if let name = SCDynamicStoreCopyComputerName(nil, nil) as String?, !name.isEmpty {
				computerName = name
			}
			else {
				computerName = localHostName
			}
		}

		/// The kernel's host name, which (unlike ProcessInfo.hostName) never waits for DNS
		private static func kernelHostName() -> String? {
			var buffer = [CChar](repeating: 0, count: Int(MAXHOSTNAMELEN) + 1)
			guard gethostname(&buffer, buffer.count - 1) == 0 else {
				return nil
			}
			let name = String(decoding: buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
			return name.isEmpty ? nil : name
		}
	}
#endif
