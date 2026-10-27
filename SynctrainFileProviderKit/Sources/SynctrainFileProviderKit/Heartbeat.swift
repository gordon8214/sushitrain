// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Written by the app while it is awake, so the extension knows whether it can fetch files from other devices through
/// the app. A suspended app still accepts connections on its listening socket but never answers, so the extension must
/// not rely on connecting alone.
public struct Heartbeat: Codable, Sendable, Equatable {
	/// The app promises to be reachable until this moment (it rewrites the heartbeat well before then)
	public var awakeUntil: Date
	/// Port of the loopback file provider server
	public var port: Int
	/// Bearer token for the loopback file provider server
	public var token: String

	public init(awakeUntil: Date, port: Int, token: String) {
		self.awakeUntil = awakeUntil
		self.port = port
		self.token = token
	}

	public func isFresh(at now: Date = Date()) -> Bool {
		now < awakeUntil && port > 0 && !token.isEmpty
	}

	public static func read(from url: URL) -> Heartbeat? {
		guard let data = try? Data(contentsOf: url) else {
			return nil
		}
		return try? JSONDecoder().decode(Heartbeat.self, from: data)
	}

	public func write(to url: URL) throws {
		let data = try JSONEncoder().encode(self)
		try data.write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
	}

	/// Marks the app as no longer reachable
	public static func clear(at url: URL) {
		try? FileManager.default.removeItem(at: url)
	}
}
