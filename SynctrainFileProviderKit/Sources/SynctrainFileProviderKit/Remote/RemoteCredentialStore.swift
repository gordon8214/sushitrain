// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Security
import os

public enum RemoteCredentialError: Error, Sendable, Equatable {
	/// The Keychain reported an error
	case keychain(OSStatus)
}

/// Keeps the secrets of paired gateways: one client credential per gateway (by service ID), and the scoped tokens of
/// leases and uploads (by reference), which background tasks carry instead of the credential
public protocol RemoteCredentialStore: Sendable {
	func credential(serviceID: String) throws(RemoteCredentialError) -> String?
	func setCredential(_ credential: String, serviceID: String) throws(RemoteCredentialError)
	func removeCredential(serviceID: String) throws(RemoteCredentialError)
	func token(reference: String) throws(RemoteCredentialError) -> String?
	func setToken(_ token: String, reference: String) throws(RemoteCredentialError)
	func removeToken(reference: String) throws(RemoteCredentialError)
}

/// Keychain storage, shared by the app and the extension through an access group: generic passwords that are not
/// synchronized and are available after the first unlock, on this device only
public struct KeychainCredentialStore: RemoteCredentialStore {
	/// The access group shared by the app and the extension (the app group can serve as one)
	public let accessGroup: String?
	/// Prefix of the Keychain service attributes
	public let servicePrefix: String

	public init(accessGroup: String?, servicePrefix: String = "Synctrain.remote") {
		self.accessGroup = accessGroup
		self.servicePrefix = servicePrefix
	}

	private func query(kind: String, account: String) -> [String: Any] {
		var query: [String: Any] = [
			kSecClass as String: kSecClassGenericPassword,
			kSecAttrService as String: "\(servicePrefix).\(kind)",
			kSecAttrAccount as String: account,
			kSecAttrSynchronizable as String: kCFBooleanFalse as Any,
		]
		if let accessGroup {
			query[kSecAttrAccessGroup as String] = accessGroup
		}
		#if os(macOS)
			query[kSecUseDataProtectionKeychain as String] = true
		#endif
		return query
	}

	private func read(kind: String, account: String) throws(RemoteCredentialError) -> String? {
		var query = query(kind: kind, account: account)
		query[kSecReturnData as String] = true
		query[kSecMatchLimit as String] = kSecMatchLimitOne
		var result: CFTypeRef?
		let status = SecItemCopyMatching(query as CFDictionary, &result)
		if status == errSecItemNotFound {
			return nil
		}
		guard status == errSecSuccess, let data = result as? Data else {
			throw .keychain(status)
		}
		return String(decoding: data, as: UTF8.self)
	}

	private func write(_ value: String, kind: String, account: String) throws(RemoteCredentialError) {
		let query = query(kind: kind, account: account)
		let attributes: [String: Any] = [
			kSecValueData as String: Data(value.utf8),
			kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
		]
		var status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
		if status == errSecItemNotFound {
			status = SecItemAdd(query.merging(attributes) { $1 } as CFDictionary, nil)
		}
		guard status == errSecSuccess else {
			throw .keychain(status)
		}
	}

	private func delete(kind: String, account: String) throws(RemoteCredentialError) {
		let status = SecItemDelete(query(kind: kind, account: account) as CFDictionary)
		guard status == errSecSuccess || status == errSecItemNotFound else {
			throw .keychain(status)
		}
	}

	public func credential(serviceID: String) throws(RemoteCredentialError) -> String? {
		try read(kind: "credential", account: serviceID)
	}

	public func setCredential(_ credential: String, serviceID: String) throws(RemoteCredentialError) {
		try write(credential, kind: "credential", account: serviceID)
	}

	public func removeCredential(serviceID: String) throws(RemoteCredentialError) {
		try delete(kind: "credential", account: serviceID)
	}

	public func token(reference: String) throws(RemoteCredentialError) -> String? {
		try read(kind: "token", account: reference)
	}

	public func setToken(_ token: String, reference: String) throws(RemoteCredentialError) {
		try write(token, kind: "token", account: reference)
	}

	public func removeToken(reference: String) throws(RemoteCredentialError) {
		try delete(kind: "token", account: reference)
	}
}

// MARK: - Released tokens

extension IndexStore {
	/// The `meta` keys listing scoped tokens that no record of the index refers to anymore: `released_token:<ref>`
	static let releasedTokenPrefix = "released_token:"
	/// The first key after every key with the prefix (`;` follows `:`)
	private static let releasedTokenEnd = "released_token;"

	/// Lists token references that records stop holding, inside the transaction that drops them, so that the tokens
	/// are removed from the credential store once it committed (see `removeReleasedTokens`). The list survives an
	/// interruption, and either process removes what the other released.
	static func releaseTokens(_ connection: SQLiteConnection, _ references: [String?]) throws(IndexError) {
		let now = String(Date().timeIntervalSince1970)
		for case let reference? in references where !reference.isEmpty {
			try connection.run(
				"INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?)", [releasedTokenPrefix + reference, now])
		}
	}

	/// Lists the upload tokens of the outbox operations that match the condition, and drops them from the operations
	static func releaseUploadTokens(
		_ connection: SQLiteConnection, where condition: String, _ values: [any SQLBindable]
	) throws(IndexError) {
		try connection.run(
			"""
			INSERT OR IGNORE INTO meta (key, value)
			SELECT ? || upload_token_ref, ? FROM remote_outbox WHERE upload_token_ref IS NOT NULL AND (\(condition))
			""", [releasedTokenPrefix, String(Date().timeIntervalSince1970)] + values)
		try connection.run(
			"UPDATE remote_outbox SET upload_token_ref = NULL WHERE upload_token_ref IS NOT NULL AND (\(condition))",
			values)
	}

	/// Lists the lease tokens of the transfers that match the condition (before they are deleted or forget them)
	static func releaseTransferTokens(
		_ connection: SQLiteConnection, where condition: String, _ values: [any SQLBindable]
	) throws(IndexError) {
		try connection.run(
			"""
			INSERT OR IGNORE INTO meta (key, value)
			SELECT ? || token_ref, ? FROM remote_transfers WHERE token_ref IS NOT NULL AND (\(condition))
			""", [releasedTokenPrefix, String(Date().timeIntervalSince1970)] + values)
	}

	/// Removes the scoped tokens that no transfer or operation refers to anymore from the credential store. Tokens
	/// that cannot be removed now (for instance while the Keychain is not available) stay listed for the next call;
	/// references that a record holds again are no longer listed.
	public func removeReleasedTokens(from credentials: any RemoteCredentialStore, limit: Int = 100) {
		let prefix = Self.releasedTokenPrefix
		let listed: [(reference: String, inUse: Bool)]
		do {
			listed = try withConnection { connection throws(IndexError) in
				try connection.query(
					"""
					SELECT substr(key, ?) AS reference,
						EXISTS (SELECT 1 FROM remote_transfers WHERE token_ref = substr(meta.key, ?))
							OR EXISTS (SELECT 1 FROM remote_outbox WHERE upload_token_ref = substr(meta.key, ?))
					FROM meta WHERE key >= ? AND key < ? ORDER BY key LIMIT ?
					""",
					[
						prefix.count + 1, prefix.count + 1, prefix.count + 1, prefix, Self.releasedTokenEnd, limit,
					]
				) { (reference: $0.text(0), inUse: $0.bool(1)) }
			}
		}
		catch {
			return
		}
		var done: [String] = []
		for entry in listed {
			if entry.inUse {
				done.append(entry.reference)
				continue
			}
			do {
				try credentials.removeToken(reference: entry.reference)
				done.append(entry.reference)
			}
			catch {
				continue
			}
		}
		guard !done.isEmpty else {
			return
		}
		try? transaction { connection throws(IndexError) in
			for reference in done {
				try connection.run("DELETE FROM meta WHERE key = ?", [prefix + reference])
			}
		}
	}

	/// The token references listed for removal (for tests)
	func releasedTokenReferences() throws(IndexError) -> [String] {
		try withConnection { connection throws(IndexError) in
			try connection.query(
				"SELECT substr(key, ?) FROM meta WHERE key >= ? AND key < ? ORDER BY key",
				[Self.releasedTokenPrefix.count + 1, Self.releasedTokenPrefix, Self.releasedTokenEnd]
			) { $0.text(0) }
		}
	}
}

/// Secrets kept in memory, for tests and previews
public final class InMemoryCredentialStore: RemoteCredentialStore {
	private let storage = OSAllocatedUnfairLock(initialState: [String: String]())

	public init(credentials: [String: String] = [:]) {
		storage.withLock { state in
			for (serviceID, credential) in credentials {
				state["credential:" + serviceID] = credential
			}
		}
	}

	public func credential(serviceID: String) -> String? {
		storage.withLock { $0["credential:" + serviceID] }
	}

	public func setCredential(_ credential: String, serviceID: String) {
		storage.withLock { $0["credential:" + serviceID] = credential }
	}

	public func removeCredential(serviceID: String) {
		storage.withLock { $0["credential:" + serviceID] = nil }
	}

	public func token(reference: String) -> String? {
		storage.withLock { $0["token:" + reference] }
	}

	public func setToken(_ token: String, reference: String) {
		storage.withLock { $0["token:" + reference] = token }
	}

	public func removeToken(reference: String) {
		storage.withLock { $0["token:" + reference] = nil }
	}

	/// The references of the tokens kept (for tests)
	public var tokenReferences: [String] {
		storage.withLock { state in
			state.keys.filter { $0.hasPrefix("token:") }.map { String($0.dropFirst("token:".count)) }.sorted()
		}
	}
}
