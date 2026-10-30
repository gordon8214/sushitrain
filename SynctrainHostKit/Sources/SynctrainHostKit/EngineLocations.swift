// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// Where the engine keeps its configuration, database and logs, and the directory it treats as the user's home.
///
/// On macOS these are the standard places of an unsandboxed app (a `Synctrain` folder in Application Support and in
/// Logs, and the real home directory, so that `~` and `~/Sync` mean what they do in Syncthing). On iOS, and in the App
/// Sandbox on macOS, they stay where they always were in the app's container.
public struct EngineLocations: Sendable, Equatable {
	public enum Platform: Sendable {
		/// macOS outside the App Sandbox
		case macOS
		/// macOS in the App Sandbox, where the system directories are the app's container's, as in earlier versions
		case sandboxedMacOS
		case iOS
	}

	/// The layout these locations follow
	public let platform: Platform

	/// The default configuration and database directory (on macOS the user can choose another)
	public let configDirectory: URL

	/// Where log files are written, when the app saves its log
	public let logs: URL

	/// The directory the engine treats as the user's home (its `filesPath`)
	public let files: URL

	/// The copies of the certificate and key the user chose for the remote file service (macOS)
	public let remoteFileService: URL

	/// - Parameters:
	///   - home: the user's home directory (the container's, in the sandbox)
	///   - applicationSupport: the system Application Support directory
	///   - documents: the app's Documents directory (iOS and the sandbox)
	public init(platform: Platform, home: URL, applicationSupport: URL, documents: URL) {
		self.platform = platform
		switch platform {
		case .macOS:
			let synctrain = applicationSupport.appending(path: "Synctrain", directoryHint: .isDirectory)
			self.configDirectory = synctrain
			self.logs = home.appending(path: "Library/Logs/Synctrain", directoryHint: .isDirectory)
			self.files = home
			self.remoteFileService = synctrain.appending(path: "RemoteFileService", directoryHint: .isDirectory)

		case .iOS, .sandboxedMacOS:
			self.configDirectory = applicationSupport
			self.logs = documents
			self.files = documents
			self.remoteFileService = applicationSupport.appending(path: "RemoteFileService", directoryHint: .isDirectory)
		}
	}
}
