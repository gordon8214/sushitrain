// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import Foundation
	@preconcurrency import SushitrainCore
	import SynctrainHostKit

	/// How the Mac app sets up its engine next to Syncthing.app: before the engine starts, it decides
	/// (EngineSetupDecision) whether to start, to offer taking over Syncthing's setup, or to wait for Syncthing to quit,
	/// and it carries out the takeover the user chooses.
	enum EngineSetup {
		enum Choice: String {
			/// Syncthing's home directory is the app's configuration folder
			case useSyncthing = "syncthing-direct"
			/// A copy of it is in the app's own folder
			case importSyncthing = "syncthing-import"
			/// A new configuration and identity
			case startFresh = "fresh"
			/// Next to another Syncthing that keeps running
			case runAlongside = "alongside"
		}

		static let choiceKey = "engineSetupChoice"
		static let receiptName = "syncthing-takeover.json"

		/// The choice made
		static var choice: Choice? {
			get {
				UserDefaults.standard.string(forKey: choiceKey).flatMap(Choice.init(rawValue:))
			}
			set {
				UserDefaults.standard.set(newValue?.rawValue, forKey: choiceKey)
			}
		}

		/// What to do before the engine starts on the configuration folder the app uses. Locks the client holds itself
		/// are no Syncthing running.
		static func decision(configDirectory: URL, client: SushitrainClient) -> EngineSetupDecision {
			// The sandboxed app can't see Syncthing's directory
			guard !FolderAccess.usesSecurityScope else {
				return .start
			}
			// A configuration folder the user chose, but can't be found now (on a disk that isn't connected): the engine
			// says so, rather than the app offering to set up anew
			if ConfigDirectorySetting.isChosen
				&& !FileManager.default.fileExists(atPath: configDirectory.path(percentEncoded: false))
			{
				return .start
			}
			return EngineSetupDecision.decide(self.facts(configDirectory: configDirectory, client: client))
		}

		static func facts(configDirectory: URL, client: SushitrainClient) -> EngineSetupDecision.Facts {
			let fileManager = FileManager.default
			let home = SyncthingInstallation.home
			var facts = EngineSetupDecision.Facts()
			facts.synctrainHasConfiguration = ["config.xml", "cert.pem", "key.pem"].contains {
				fileManager.fileExists(atPath: configDirectory.appending(path: $0).path(percentEncoded: false))
			}
			facts.usesSyncthingHome = configDirectory.standardizedFileURL.path == home.standardizedFileURL.path
			// An app that ran before (before this choice existed) has its setup
			facts.choiceRecorded = self.choice != nil || UserDefaults.standard.integer(forKey: "lastRunBuild") > 0
			facts.syncthingHasConfiguration = SyncthingInstallation.hasConfiguration
			facts.syncthingRunning = SyncthingInstallation.isRunning(besides: client)
			if let own = ContainerMigration.sha256(of: configDirectory.appending(path: "cert.pem")) {
				facts.sameIdentity = own == ContainerMigration.sha256(of: home.appending(path: "cert.pem"))
			}
			facts.runsAlongside = self.choice == .runAlongside
			return facts
		}

		/// What the app did when it took over Syncthing's setup, for going back (Docs/macos.md)
		struct Receipt: Codable {
			var mode: String
			var date: Date
			var syncthingHome: String
			var deviceID: String
			/// Syncthing.app's StartAtLogin setting before the takeover
			var previousStartAtLogin: Bool?
			var loginItemRemoved: Bool
			/// The copy of Syncthing's engine data and Syncthing.app's preferences made first
			var backup: String?
			/// The app's own list of paused devices before it took over Syncthing's
			var previousPausedDevices: [String]
		}

		enum TakeoverError: LocalizedError {
			case syncthingRunning
			case tooNew
			case noIdentity
			case importFailed(String)

			var errorDescription: String? {
				switch self {
				case .syncthingRunning:
					return String(localized: "Quit Syncthing first.")
				case .tooNew:
					return String(
						localized: "Syncthing's configuration or database is from a newer version than this app's engine.")
				case .noIdentity:
					return String(
						localized:
							"Syncthing's device certificate or key is missing or damaged, so its device ID can't be kept.")
				case .importFailed(let problem):
					switch problem {
					case SushitrainImportProblemLocked:
						return String(localized: "Quit Syncthing first.")
					case SushitrainImportProblemInUse:
						return String(
							localized:
								"Synctrain's own configuration folder isn't empty, so the copy can't be put there.")
					case SushitrainImportProblemTooNew:
						return String(
							localized:
								"Syncthing's configuration or database is from a newer version than this app's engine.")
					case SushitrainImportProblemIdentity:
						return String(
							localized:
								"Syncthing's device certificate or key is missing or damaged, so its device ID can't be kept."
						)
					default:
						return problem
					}
				}
			}
		}

		/// Keeps Syncthing.app from starting again at login: turns off its StartAtLogin setting and removes its login
		/// item. Returns whether that was done, and else why not (the login item may have to go in System Settings).
		@MainActor static func stopSyncthingAtLogin() async -> (removed: Bool, problem: Error?) {
			SyncthingInstallation.turnOffStartAtLogin()
			do {
				_ = try await SyncthingInstallation.removeLoginItem()
				return (true, nil)
			}
			catch {
				return (false, error)
			}
		}

		/// Uses Syncthing's home directory as the configuration folder from now on, after backing it up, then restarts
		/// the app to start on it
		@MainActor static func useSyncthing(
			_ summary: SyncthingInstallation.Summary, appState: AppState,
			loginItemRemoved: Bool, previousStartAtLogin: Bool?
		) async throws {
			try self.checkTakeover(summary)
			let backup = try await self.backUpSyncthing()
			self.keepGUISetting(of: SyncthingInstallation.home)
			let previousPausedDevices = Array(appState.userSettings.userPausedDevices)
			appState.adoptSyncthingSettings(pausedDevices: summary.pausedDevices ?? [])
			ConfigDirectorySetting.choose(SyncthingInstallation.home)
			self.choice = .useSyncthing
			self.writeReceipt(
				Receipt(
					mode: Choice.useSyncthing.rawValue, date: Date(),
					syncthingHome: SyncthingInstallation.home.path(percentEncoded: false), deviceID: summary.deviceID,
					previousStartAtLogin: previousStartAtLogin, loginItemRemoved: loginItemRemoved,
					backup: backup.path(percentEncoded: false), previousPausedDevices: previousPausedDevices))
			AppRelauncher.relaunch()
		}

		/// Copies Syncthing's configuration, identity and database into the app's own configuration folder; Syncthing's
		/// own stay as they are. Then the engine can start.
		@MainActor static func importSyncthing(
			_ summary: SyncthingInstallation.Summary, appState: AppState,
			configDirectory: URL, loginItemRemoved: Bool, previousStartAtLogin: Bool?
		) async throws {
			try self.checkTakeover(summary)
			try await self.copySyncthingHome(to: configDirectory)
			self.keepGUISetting(of: configDirectory)
			// Recorded at once: the copy is in place now
			let previousPausedDevices = Array(appState.userSettings.userPausedDevices)
			appState.adoptSyncthingSettings(pausedDevices: summary.pausedDevices ?? [])
			self.choice = .importSyncthing
			self.writeReceipt(
				Receipt(
					mode: Choice.importSyncthing.rawValue, date: Date(),
					syncthingHome: SyncthingInstallation.home.path(percentEncoded: false), deviceID: summary.deviceID,
					previousStartAtLogin: previousStartAtLogin, loginItemRemoved: loginItemRemoved, backup: nil,
					previousPausedDevices: previousPausedDevices))
		}

		private static func checkTakeover(_ summary: SyncthingInstallation.Summary) throws {
			guard !SyncthingInstallation.isRunning(besides: nil) else {
				throw TakeoverError.syncthingRunning
			}
			guard !summary.tooNew else {
				throw TakeoverError.tooNew
			}
			guard !summary.deviceID.isEmpty, summary.identityError == nil else {
				throw TakeoverError.noIdentity
			}
		}

		/// Copies Syncthing's engine data (its lock held, so that it can't start meanwhile) to a new directory
		private static func copySyncthingHome(to destination: URL) async throws {
			let source = SyncthingInstallation.home.path(percentEncoded: false)
			let target = destination.path(percentEncoded: false)
			// The database may be large
			try await Task.detached(priority: .userInitiated) {
				var error: NSError? = nil
				let problem = SushitrainImportSyncthingHome(source, target, &error)
				if !problem.isEmpty {
					throw TakeoverError.importFailed(problem)
				}
				if let error {
					throw error
				}
			}.value
		}

		/// Its own GUI setting, rather than the one the app turns on for configurations it made earlier
		private static func keepGUISetting(of directory: URL) {
			var error: NSError? = nil
			_ = SushitrainKeepGUISetting(directory.path(percentEncoded: false), &error)
			if let error {
				Log.warn("Could not record the web GUI setting as kept: \(error.localizedDescription)")
			}
		}

		/// Copies Syncthing's engine data and Syncthing.app's preferences into a new folder in "Synctrain Takeover
		/// Backups" in Application Support
		private static func backUpSyncthing() async throws -> URL {
			let fileManager = FileManager.default
			let formatter = ISO8601DateFormatter()
			formatter.formatOptions = [.withFullDate, .withTime]
			let home = EngineLocations.testRoot ?? fileManager.homeDirectoryForCurrentUser
			let backup = home.appending(
				path:
					"Library/Application Support/Synctrain Takeover Backups/\(formatter.string(from: Date()))-\(UUID().uuidString.prefix(8))",
				directoryHint: .isDirectory)
			try fileManager.createDirectory(
				at: backup, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
			try await self.copySyncthingHome(to: backup.appending(path: "Syncthing", directoryHint: .isDirectory))
			let preferences = home.appending(
				path: "Library/Preferences/\(SyncthingInstallation.bundleIdentifier).plist")
			if fileManager.fileExists(atPath: preferences.path(percentEncoded: false)) {
				try fileManager.copyItem(at: preferences, to: backup.appending(path: preferences.lastPathComponent))
			}
			return backup
		}

		/// Next to the app's own engine data, whichever configuration folder it uses. A receipt that can't be written
		/// is logged: the takeover itself is done.
		private static func writeReceipt(_ receipt: Receipt) {
			let directory = EngineLocations.current.configDirectory
			do {
				try FileManager.default.createDirectory(
					at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
				let encoder = JSONEncoder()
				encoder.dateEncodingStrategy = .iso8601
				encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
				try encoder.encode(receipt).write(to: directory.appending(path: receiptName), options: [.atomic])
			}
			catch {
				Log.warn("Could not record the takeover of Syncthing's setup: \(error.localizedDescription)")
			}
		}
	}
#endif
