// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import Darwin
	import Security

	public enum HistoryBridgeLocation {
		public static var directory: URL {
			FileManager.default.homeDirectoryForCurrentUser.appending(
				path: "Library/Application Support/Synctrain/HistoryBridge")
		}
		public static let appIdentifier = "gordonbyrnes.Sushitrain"
		public static let clientIdentifier = "com.gordonbyrnes.BetterFinder"
	}

	final class HistoryBridgeChannel: @unchecked Sendable {
		let descriptor: Int32
		init(_ descriptor: Int32) {
			self.descriptor = descriptor
			var enabled: Int32 = 1
			setsockopt(descriptor, SOL_SOCKET, SO_NOSIGPIPE, &enabled, socklen_t(MemoryLayout.size(ofValue: enabled)))
			var timeout = timeval(tv_sec: 180, tv_usec: 0)
			setsockopt(descriptor, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
			setsockopt(descriptor, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
		}
		deinit { Darwin.close(descriptor) }
		func cancel() { Darwin.shutdown(descriptor, SHUT_RDWR) }
		func verify(identifier: String) throws {
			var token = audit_token_t()
			var size = socklen_t(MemoryLayout.size(ofValue: token))
			guard getsockopt(descriptor, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &size) == 0 else {
				throw failure("Cannot identify the local history peer.")
			}
			var user: uid_t = 0
			var group: gid_t = 0
			guard getpeereid(descriptor, &user, &group) == 0, user == getuid() else {
				throw failure("The history peer belongs to another user.")
			}
			let data = withUnsafeBytes(of: &token) { Data($0) }
			var code: SecCode?
			let attributes = [kSecGuestAttributeAudit as String: data] as CFDictionary
			guard SecCodeCopyGuestWithAttributes(nil, attributes, [], &code) == errSecSuccess, let code else {
				throw failure("Cannot verify the history peer's signature.")
			}
			var requirement: SecRequirement?
			let rule = "anchor apple generic and identifier \"\(identifier)\" and certificate leaf[subject.OU] = \"UF6P8KZZN4\""
			guard SecRequirementCreateWithString(rule as CFString, [], &requirement) == errSecSuccess, let requirement,
				SecCodeCheckValidity(code, [], requirement) == errSecSuccess
			else { throw failure("The local history peer is not the expected signed app.") }
		}
		func read<Value: Decodable>(_ type: Value.Type) throws -> Value {
			let header = try receive(count: 4)
			let count = header.reduce(0) { ($0 << 8) | Int($1) }
			guard count > 0, count <= 4 << 20 else { throw failure("Invalid history response size.") }
			return try JSONDecoder().decode(type, from: receive(count: count))
		}
		func write<Value: Encodable>(_ value: Value) throws {
			let data = try JSONEncoder().encode(value)
			guard !data.isEmpty, data.count <= 4 << 20 else { throw failure("The history response is too large.") }
			var length = UInt32(data.count).bigEndian
			try withUnsafeBytes(of: &length) { try send(Data($0)) }
			try send(data)
		}
		private func receive(count: Int) throws -> Data {
			var data = Data(count: count)
			try data.withUnsafeMutableBytes { buffer in
				var offset = 0
				while offset < count {
					let result = Darwin.read(descriptor, buffer.baseAddress!.advanced(by: offset), count - offset)
					if result < 0 && errno == EINTR { continue }
					guard result > 0 else { throw failure("The history connection closed or timed out.") }
					offset += result
				}
			}
			return data
		}
		private func send(_ data: Data) throws {
			try data.withUnsafeBytes { buffer in
				var offset = 0
				while offset < data.count {
					let result = Darwin.write(descriptor, buffer.baseAddress!.advanced(by: offset), data.count - offset)
					if result < 0 && errno == EINTR { continue }
					guard result > 0 else { throw failure("The history connection closed or timed out.") }
					offset += result
				}
			}
		}
		private func failure(_ text: String) -> HistoryBridgeError { .message(text) }
	}

	func historySocketAddress(_ path: String) throws -> sockaddr_un {
		var address = sockaddr_un()
		address.sun_family = sa_family_t(AF_UNIX)
		let bytes = Array(path.utf8) + [0]
		guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
			throw HistoryBridgeError.message("The local history socket path is too long.")
		}
		withUnsafeMutableBytes(of: &address.sun_path) { destination in destination.copyBytes(from: bytes) }
		address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
		return address
	}
#endif
