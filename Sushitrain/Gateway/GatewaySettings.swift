// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SynctrainFileProviderKit
	import SystemConfiguration

	/// How the remote file service is reached, stored in the user defaults. Changes apply when the service starts, but
	/// for the address devices connect to (`publicHost`, `publicPort`), which a running service takes at once.
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

		/// The port the service listens on
		var port = Self.defaultPort
		/// Where devices connect, for pairing codes (empty: this Mac's local host name)
		var publicHost = ""
		/// The port devices connect to, for pairing codes, when a router forwards another one to this Mac (0: the port
		/// the service listens on)
		var publicPort = 0
		/// The name paired devices show (empty: this Mac's name)
		var serviceName = ""
		var certificateMode = CertificateMode.selfSigned
		var contentCacheMiB = Self.defaultContentCacheMiB

		private enum Key {
			static let port = "remoteFileServicePort"
			static let publicHost = "remoteFileServicePublicHost"
			static let publicPort = "remoteFileServicePublicPort"
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
			if let port = defaults.object(forKey: Key.publicPort) as? Int, validPorts.contains(port) {
				settings.publicPort = port
			}
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
			defaults.set(publicPort, forKey: Key.publicPort)
			defaults.set(serviceName, forKey: Key.serviceName)
			defaults.set(certificateMode.rawValue, forKey: Key.certificateMode)
			defaults.set(contentCacheMiB, forKey: Key.contentCacheMiB)
		}

		var isValid: Bool {
			Self.validPorts.contains(port) && (publicPort == 0 || Self.validPorts.contains(publicPort))
		}

		@MainActor var effectivePublicHost: String {
			let host = publicHost.trimmingCharacters(in: .whitespacesAndNewlines)
			return host.isEmpty ? Self.localHostName : host
		}

		var effectivePublicPort: Int {
			publicPort == 0 ? port : publicPort
		}

		/// Where devices connect (`host:port`), as pairing codes give it
		@MainActor var address: String {
			RemoteAddress.format(host: effectivePublicHost, port: effectivePublicPort)
		}

		/// The address as the user set it, for editing: empty while it is this Mac's local name and the listening port
		@MainActor var customAddress: String {
			publicHost.isEmpty && publicPort == 0 ? "" : address
		}

		/// These settings with the address the user typed: empty for this Mac's local name and the listening port. A
		/// host without a port keeps following the listening port, and so does this Mac's local name. Nil when the text
		/// is not an address.
		@MainActor func withAddress(_ text: String) -> Self? {
			var settings = self
			guard !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
				settings.publicHost = ""
				settings.publicPort = 0
				return settings
			}
			guard let address = RemoteAddress(parsing: text) else {
				return nil
			}
			settings.publicHost = address.host
			settings.publicPort = address.port ?? 0
			return settings.normalized()
		}

		/// These settings in the one form that stands for them, which the editors compare: the host trimmed and parsed
		/// (no brackets or scheme; a port saved with it, as the earlier host name field allowed, becomes the public port
		/// unless there is one), this Mac's local name as empty (it follows the Mac's name), and a public port equal to
		/// the listening one as 0 (it follows the listening port). A host that isn't an address stays as it is, trimmed.
		@MainActor func normalized() -> Self {
			var settings = self
			let host = publicHost.trimmingCharacters(in: .whitespacesAndNewlines)
			settings.publicHost = host
			if !host.isEmpty, let address = RemoteAddress(parsing: host) {
				let isLocalName = address.host.caseInsensitiveCompare(Self.localHostName) == .orderedSame
				settings.publicHost = isLocalName ? "" : address.host
				if let port = address.port, settings.publicPort == 0 {
					settings.publicPort = port
				}
			}
			if settings.publicPort == settings.port {
				settings.publicPort = 0
			}
			return settings
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
