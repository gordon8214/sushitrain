// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Where a gateway is reached, as people write it: a host name or IP address, with or without a port
/// (`studio.example.org`, `studio.example.org:20920`, `192.168.1.2:20920`, `[fd00::1]:20920` or `fd00::1`).
public struct RemoteAddress: Hashable, Sendable {
	/// The host, an IPv6 address without its brackets
	public var host: String
	/// The port, when one was written
	public var port: Int?

	public static let validPorts = 1...65535

	public init(host: String, port: Int? = nil) {
		self.host = host
		self.port = port
	}

	/// Reads an address someone typed or pasted (a `https://host[:port]` link too, whose port is 443 when it has none).
	/// Nil when it is empty, or is not a host with an optional port: a path, a space, a user name, another scheme, a
	/// name that isn't ASCII (enter its `xn--` form) or a port out of range.
	public init?(parsing text: String) {
		var rest = Substring(text.trimmingCharacters(in: .whitespacesAndNewlines))
		var isLink = false
		if let scheme = rest.range(of: "://") {
			guard rest[..<scheme.lowerBound].lowercased() == "https" else {
				return nil
			}
			isLink = true
			rest = rest[scheme.upperBound...]
			if rest.hasSuffix("/") {
				rest = rest.dropLast()
			}
		}
		let host: Substring
		var portText: Substring? = nil
		if rest.hasPrefix("[") {
			guard let close = rest.firstIndex(of: "]") else {
				return nil
			}
			host = rest[rest.index(after: rest.startIndex)..<close]
			let after = rest[rest.index(after: close)...]
			if !after.isEmpty {
				guard after.hasPrefix(":") else {
					return nil
				}
				portText = after.dropFirst()
			}
			guard Self.isIPv6(host) else {
				return nil
			}
		}
		else if rest.filter({ $0 == ":" }).count > 1 {
			// An IPv6 address without brackets has no port
			host = rest
			guard Self.isIPv6(host) else {
				return nil
			}
		}
		else if let colon = rest.firstIndex(of: ":") {
			host = rest[..<colon]
			portText = rest[rest.index(after: colon)...]
		}
		else {
			host = rest
		}
		guard Self.isHostName(host) || Self.isIPv6(host) else {
			return nil
		}
		self.host = String(host)
		if let portText {
			guard !portText.isEmpty, portText.allSatisfy(\.isASCIIDigit), let port = Int(portText),
				Self.validPorts.contains(port)
			else {
				return nil
			}
			self.port = port
		}
		else if isLink {
			self.port = 443
		}
	}

	/// The address as written in pairing codes and settings (`host:port`, `[fd00::1]:port`)
	public static func format(host: String, port: Int) -> String {
		host.contains(":") ? "[\(host)]:\(port)" : "\(host):\(port)"
	}

	/// A DNS name or IPv4 address in ASCII: labels of letters, digits, hyphens and underscores, none empty (but for a
	/// final dot) and none starting or ending with a hyphen. Other names would be converted when connecting, and then no
	/// longer match what is stored.
	private static func isHostName(_ host: Substring) -> Bool {
		let name = host.hasSuffix(".") ? host.dropLast() : host
		return !name.isEmpty
			&& name.split(separator: ".", omittingEmptySubsequences: false).allSatisfy { label in
				!label.isEmpty && !label.hasPrefix("-") && !label.hasSuffix("-")
					&& label.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }
			}
	}

	/// An IPv6 address without a zone (`%en0`, which Darwin accepts but URLs can't carry as typed)
	private static func isIPv6(_ host: Substring) -> Bool {
		var address = in6_addr()
		return !host.isEmpty && !host.contains("%") && inet_pton(AF_INET6, String(host), &address) == 1
	}
}

extension Character {
	fileprivate var isASCIIDigit: Bool { isASCII && isNumber }
}
