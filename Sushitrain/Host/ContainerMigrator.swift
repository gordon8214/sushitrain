// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	import SushitrainCore
	import SynctrainHostKit

	/// Takes over what the sandboxed app kept in its container at the first start outside the sandbox: its preferences,
	/// then its engine data. The container stays as it was, as the copy to go back to; folders in it keep working there
	/// and can be moved later (ContainerFoldersView).
	enum ContainerMigrator {
		/// Set once the container's preferences were taken over (or there were none)
		static let preferencesMarkerKey = "containerMigration.v1"
		/// Set once the move of the container's folders was offered
		static let folderMoveOfferedKey = "containerFolderMoveOffered"

		private static var bundleIdentifier: String {
			Bundle.main.bundleIdentifier ?? "gordonbyrnes.Sushitrain"
		}

		private static var home: URL {
			EngineLocations.testRoot ?? FileManager.default.homeDirectoryForCurrentUser
		}

		/// The sandboxed app's data in its container
		static var containerData: URL {
			home.appending(path: "Library/Containers/\(bundleIdentifier)/Data", directoryHint: .isDirectory)
		}

		/// The engine data in the container, laid out as the sandboxed app had it
		static var containerEngineLocations: EngineLocations {
			EngineLocations(
				platform: .sandboxedMacOS, home: containerData,
				applicationSupport: containerData.appending(
					path: "Library/Application Support", directoryHint: .isDirectory),
				documents: containerData.appending(path: "Documents", directoryHint: .isDirectory))
		}

		/// What taking over the container led to
		struct Outcome: Sendable {
			/// Why the engine must not start (it would start anew, with another device identity)
			var error: String? = nil
			/// What to tell the user once the app runs
			var notice: String? = nil
			/// What to log, once logging is set up
			var messages: [String] = []
		}

		/// Set once the first start outside the sandbox left the container's engine data alone because a configuration
		/// folder was chosen, so that a later switch to the default location doesn't take in the container's old data
		static let engineDataSettledKey = "containerEngineDataSettled"
		/// Set once the user was told that the app kept a device identity other than the container's
		static let identityNoticeShownKey = "containerIdentityNoticeShown"

		/// Takes over the container's preferences and engine data, outside the sandbox. Call this first thing at launch:
		/// the configuration folder setting comes with the preferences. Nothing is logged yet, as logging is set up
		/// later: the outcome holds the messages.
		static func run() -> Outcome {
			var outcome = Outcome()
			guard !FolderAccess.usesSecurityScope else {
				return outcome
			}
			// A test run leaves the real preferences alone
			if EngineLocations.testRoot == nil && !UserDefaults.standard.bool(forKey: preferencesMarkerKey) {
				// A version still running in the sandbox would keep changing them (the engine data's move checks this
				// itself)
				if ContainerMigration.isLocked(
					containerEngineLocations.configDirectory.appending(path: "syncthing.lock"))
				{
					outcome.error = self.message(for: ContainerMigration.MigrationError.containerInUse)
					return outcome
				}
				if let failure = self.importPreferences(&outcome.messages) {
					// Without them, a configuration folder chosen in the sandbox would be passed over
					outcome.error = String(
						localized:
							"The app could not take over its settings from its old app container (\(failure)). Your data is still in the container."
					)
					return outcome
				}
			}
			self.migrateEngineData(&outcome)
			return outcome
		}

		/// Adds the container's preferences that are not set yet, after backing up both preference files. Returns why it
		/// could not, when the container has preferences.
		private static func importPreferences(_ messages: inout [String]) -> String? {
			let defaults = UserDefaults.standard
			guard !defaults.bool(forKey: preferencesMarkerKey) else {
				return nil
			}
			let source = containerData.appending(path: "Library/Preferences/\(bundleIdentifier).plist")
			switch ContainerMigration.presence(of: source) {
			case 0:
				break
			case ENOENT, ENOTDIR:
				defaults.set(true, forKey: preferencesMarkerKey)
				return nil
			case let code:
				return String(cString: strerror(code))
			}
			let preferences: [String: Any]
			do {
				let data = try Data(contentsOf: source)
				guard
					let read = try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
				else {
					return String(localized: "The preferences file is damaged.")
				}
				preferences = read
				let backup = try self.makeBackupDirectory()
				try FileManager.default.copyItem(at: source, to: backup.appending(path: "container-\(source.lastPathComponent)"))
				let current = home.appending(path: "Library/Preferences/\(bundleIdentifier).plist")
				if FileManager.default.fileExists(atPath: current.path(percentEncoded: false)) {
					try FileManager.default.copyItem(at: current, to: backup.appending(path: current.lastPathComponent))
				}
			}
			catch {
				return error.localizedDescription
			}

			let values = ContainerPreferences.valuesToImport(
				from: preferences, current: defaults.persistentDomain(forName: bundleIdentifier) ?? [:])
			for (key, value) in values {
				defaults.set(value, forKey: key)
			}
			defaults.set(true, forKey: preferencesMarkerKey)
			messages.append("Took over \(values.count) preferences from the app container")
			return nil
		}

		/// A new folder for backups, named after the time, in Application Support
		private static func makeBackupDirectory() throws -> URL {
			let formatter = ISO8601DateFormatter()
			formatter.formatOptions = [.withFullDate, .withTime]
			let name = "\(formatter.string(from: Date()))-\(UUID().uuidString.prefix(8))"
			let directory = home.appending(
				path: "Library/Application Support/Synctrain Migration Backups/\(name)", directoryHint: .isDirectory)
			try FileManager.default.createDirectory(
				at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
			return directory
		}

		/// Copies the container's engine data into the app's own folder, unless a configuration folder was chosen (its
		/// data stays where it is)
		private static func migrateEngineData(_ outcome: inout Outcome) {
			let defaults = UserDefaults.standard
			if defaults.bool(forKey: engineDataSettledKey) {
				return
			}
			if ConfigDirectorySetting.path != nil {
				self.takeOverRemoteFileService(&outcome.messages)
				if !TestRun.isActive {
					defaults.set(true, forKey: engineDataSettledKey)
				}
				return
			}
			let migration = ContainerMigration(
				source: containerEngineLocations.configDirectory, destination: EngineLocations.current.configDirectory)
			switch migration.state() {
			case .nothingToMigrate, .done:
				return
			case .unreadable(let code):
				outcome.error = self.message(for: ContainerMigration.MigrationError.containerUnreadable(code))
			case .destinationInUse:
				outcome.messages.append(
					"The app container has engine data, but so does \(migration.destination.path); using the latter and leaving the container alone"
				)
				let containerIdentity = ContainerMigration.sha256(of: migration.source.appending(path: "cert.pem"))
				let identity = ContainerMigration.sha256(of: migration.destination.appending(path: "cert.pem"))
				if let containerIdentity, containerIdentity != identity, !defaults.bool(forKey: identityNoticeShownKey) {
					defaults.set(true, forKey: identityNoticeShownKey)
					outcome.notice = String(
						localized:
							"The app uses the configuration in \(migration.destination.path(percentEncoded: false)), whose device identity differs from the one in its old app container. The data in the container was left as it is."
					)
				}
			case .needed:
				do {
					let receipt = try migration.run()
					outcome.messages.append(
						"Moved the engine data out of the app container: \(receipt.items.joined(separator: ", "))")
				}
				catch {
					outcome.messages.append("Could not move the engine data out of the app container: \(error)")
					outcome.error = self.message(for: error)
				}
			}
		}

		/// Copies the remote file service's certificate copies from the container, which are kept in the app's own folder
		/// whichever configuration folder is used
		private static func takeOverRemoteFileService(_ messages: inout [String]) {
			let source = containerEngineLocations.remoteFileService
			let destination = EngineLocations.current.remoteFileService
			let fileManager = FileManager.default
			guard fileManager.fileExists(atPath: source.path(percentEncoded: false)),
				!fileManager.fileExists(atPath: destination.path(percentEncoded: false))
			else {
				return
			}
			do {
				try fileManager.createDirectory(
					at: destination.deletingLastPathComponent(), withIntermediateDirectories: true,
					attributes: [.posixPermissions: 0o700])
				try fileManager.copyItem(at: source, to: destination)
				messages.append("Took over the remote file service's certificate from the app container")
			}
			catch {
				messages.append("Could not take over the remote file service's certificate from the app container: \(error)")
			}
		}

		/// What the startup screen says when the engine data could not be taken over
		private static func message(for error: any Error) -> String {
			switch error {
			case ContainerMigration.MigrationError.containerInUse:
				return String(
					localized:
						"An earlier version of the app still uses the data in its app container. Quit it, then try again."
				)
			case ContainerMigration.MigrationError.containerUnreadable(let code) where code == EPERM || code == EACCES:
				return String(
					localized:
						"The app may not read its old app container. Allow it in System Settings, under Privacy & Security, then try again. Your data is still in the container."
				)
			default:
				return String(
					localized:
						"The app could not move its data out of its old app container (\(error.localizedDescription)). Your data is still in the container."
				)
			}
		}

		/// Whether a folder is still in the container
		static func isInContainer(_ folder: SushitrainFolder) -> Bool {
			guard folder.isNativeFilesystem() else {
				return false
			}
			return self.isInContainer(URL(fileURLWithPath: (folder.path() as NSString).expandingTildeInPath))
		}

		/// Whether a location is in the container
		static func isInContainer(_ url: URL) -> Bool {
			let container = containerData.standardizedFileURL.pathComponents
			let components = url.standardizedFileURL.pathComponents
			return components.count > container.count && Array(components.prefix(container.count)) == container
		}
	}
#endif
