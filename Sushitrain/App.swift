// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore
import BackgroundTasks
import Combine
import AppIntents
import SynctrainHostKit

@main
struct SushitrainApp: App {
	static var viewRouteActivityID = "nl.t-shaped.Sushitrain.view-route"

	@State fileprivate var appState: AppState

	// Reference to appState.userSettings. This is needed because `body` needs to observe this
	@ObservedObject private var userSettings: AppUserSettings

	fileprivate var delegate: SushitrainDelegate?
	private let qaService = QuickActionService.shared

	#if os(iOS)
		@UIApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
		@State private var memoryWarningPublisher = NotificationCenter.default.publisher(
			for: UIApplication.didReceiveMemoryWarningNotification)
	#endif

	#if os(macOS)
		@NSApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
		@Environment(\.scenePhase) private var scenePhase
		@Environment(\.openWindow) private var openWindow
		@AppStorage("hideInDock") var hideInDock: Bool = false
	#endif

	init() {
		// Install uncaught exception handler
		NSSetUncaughtExceptionHandler { e in
			Log.warn(
				"Uncaught exception: \(e.name) reason=\(e.reason ?? "unknown") returnAddress=\(e.callStackReturnAddresses) symbols=\(e.callStackSymbols)"
			)
		}

		// The first start outside the sandbox takes over the sandboxed app's preferences and engine data
		#if os(macOS)
			let containerMigration = ContainerMigrator.run()
		#endif

		// Find out all the relevant locations we need to access
		AppLocations.prepareCurrent()
		let engineLocations = EngineLocations.current
		let configDirectory = Self.configDirectoryURL()
		let filesPath = engineLocations.files.path(percentEncoded: false)
		let configPath = configDirectory.path(percentEncoded: false)
		let enableLoggingToFile = UserDefaults.standard.bool(forKey: "loggingEnabled")
		registerPhotoFilesystem()
		SushitrainRegisterTrashHandler(SystemTrash())
		#if os(iOS)
			FileProviderStorage.configureManagedFolderBase()
		#endif
		// Outside the sandbox on macOS, folders are the user's, anywhere in the home directory
		let client = SushitrainNewClient(
			configPath, filesPath, engineLocations.logs.path(percentEncoded: false),
			engineLocations.platform == .macOS, enableLoggingToFile)!
		if UserDefaults.standard.bool(forKey: "keepLogLevels") {
			client.applyLogLevels(UserDefaults.standard.string(forKey: "keptLogLevels") ?? "")
		}
		#if os(macOS)
			for message in containerMigration.messages {
				Log.info(message)
			}
			// Nothing may be written in the configuration folder before the engine data arrives there
			let engineDataArrived = containerMigration.error == nil
		#else
			let engineDataArrived = true
		#endif

		// Optionally clear v1 and/or v2 index
		let clearV1Index = engineDataArrived && UserDefaults.standard.bool(forKey: "clearV1Index")
		let clearV2Index = engineDataArrived && UserDefaults.standard.bool(forKey: "clearV2Index")
		if clearV1Index {
			Log.info("Deleting v1 index...")
			do {
				try client.clearLegacyDatabase()
				UserDefaults.standard.setValue(false, forKey: "clearV1Index")
			}
			catch {
				Log.warn("Failed to delete v1 index: " + error.localizedDescription)
			}
		}
		if clearV2Index {
			Log.info("Deleting v2 index...")
			do {
				try client.clearDatabase()
				UserDefaults.standard.setValue(false, forKey: "clearV2Index")
			}
			catch {
				Log.warn("Failed to delete v2 index: " + error.localizedDescription)
			}
		}

		let appState = AppState(client: client, configDirectory: configDirectory)
		#if os(macOS)
			appState.containerMigrationError = containerMigration.error
			appState.containerMigrationNotice = containerMigration.notice
		#endif
		self.appState = appState
		self.userSettings = appState.userSettings
		#if os(macOS)
			self.appDelegate.appState = appState
		#endif

		AppDependencyManager.shared.add(dependency: appState)
		appState.isLoggingToFile = enableLoggingToFile
		let delegate = SushitrainDelegate(appState: appState)
		self.delegate = delegate
		client.delegate = self.delegate

		// Start Syncthing node in the background
		#if os(macOS)
			let hideInDock = self.hideInDock
		#endif

		// Check if we need to show onboarding
		Task {
			await appState.start()

			// We can only set the streaming server delegate here, because client.server is nil until
			// afer client.start()
			client.server?.delegate = delegate
		}

		#if os(macOS)
			DispatchQueue.main.async {
				NSApp.setActivationPolicy(hideInDock ? .accessory : .regular)
			}
		#endif
	}

	/// The engine's configuration directory: on macOS the one the user chose, or Synctrain's own, on iOS the app's
	/// Application Support directory
	static func configDirectoryURL() -> URL {
		#if os(macOS)
			var isStale = false
			// A test run (SYNCTRAIN_TEST_ROOT) leaves the configuration folder the user chose alone
			let chosen = EngineLocations.testRoot == nil
			if chosen && !FolderAccess.usesSecurityScope, let path = ConfigDirectorySetting.path {
				// Never created here: if it can't be found (as on a disk that is not connected), the engine says so
				Log.info("Using custom config directory: \(path)")
				return URL(fileURLWithPath: path, isDirectory: true)
			}
			if chosen && FolderAccess.usesSecurityScope,
				let configDirectoryBookmark = UserDefaults.standard.data(forKey: ConfigDirectorySetting.bookmarkKey),
				let cd = try? URL(
					resolvingBookmarkData: configDirectoryBookmark, options: [.withSecurityScope],
					bookmarkDataIsStale: &isStale),
				cd.startAccessingSecurityScopedResource()
			{
				Log.info("Using custom config directory: \(cd)")

				if isStale {
					Log.info("Config directory bookmark is stale, recreating bookmark")
					if let bookmarkData = try? cd.bookmarkData(options: [.withSecurityScope]) {
						UserDefaults.standard.setValue(
							bookmarkData, forKey: ConfigDirectorySetting.bookmarkKey)
					}
				}
				return cd
			}

			// Its own folder, for this user only; Time Machine backs it up, as with Syncthing
			let configDirectory = EngineLocations.current.configDirectory
			do {
				try FileManager.default.createDirectory(
					at: configDirectory, withIntermediateDirectories: true,
					attributes: [.posixPermissions: 0o700])
			}
			catch {
				Log.warn("Could not create \(configDirectory.path): \(error.localizedDescription)")
			}
			return configDirectory
		#else
			var configDirectory = try! FileManager.default.url(
				for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil,
				create: true)

			// Exclude config and database directory from device back-up
			var excludeFromBackup = URLResourceValues()
			excludeFromBackup.isExcludedFromBackup = true
			do {
				try configDirectory.setResourceValues(excludeFromBackup)
			}
			catch {
				Log.warn(
					"Error excluding \(configDirectory.path) from backup: \(error.localizedDescription)"
				)
			}
			return configDirectory
		#endif
	}

	private func onReceiveMemoryWarning() {
		Log.info("Received memory pressure warning")
		self.appState.reduceMemoryUsage()
	}

	var body: some Scene {
		#if os(macOS)
			WindowGroup(id: "folder", for: String.self) { [appState] folderID in
				MainView(
					topLevelRoute: folderID.wrappedValue == nil ? .start : .folder(folderID: folderID.wrappedValue!, prefix: nil),
					initialColumnVisibility: folderID.wrappedValue == nil ? .doubleColumn : .detailOnly
				).environment(appState)
			}

			WindowGroup(id: "bookmark", for: URL.self) { [appState] bookmarkURL in
				MainView(
					topLevelRoute: bookmarkURL.wrappedValue.flatMap { Route(url: $0) } ?? .start,
					initialColumnVisibility: .detailOnly
				).environment(appState)
			}
			.defaultLaunchBehavior(.suppressed)

			WindowGroup(id: "preview", for: Preview.self) { [appState] preview in
				if appState.startupState == .started {
					if let p = preview.wrappedValue {
						PreviewWindow(preview: p)
							.environment(appState)
					}
				}
			}
			.windowManagerRole(.associated)

			WindowGroup(id: "decrypter") {
				DecrypterView()
			}
		#endif

		WindowGroup(id: "main") { [appState] in
			MainView()
				#if os(iOS)
					.modifier(IncomingFilesModifier())
				#endif
				.environment(appState)
				#if os(iOS)
					.onReceive(memoryWarningPublisher) { _ in
						self.onReceiveMemoryWarning()
					}
				#endif

				#if os(macOS)
					.handlesExternalEvents(preferring: ["*"], allowing: ["*"])
					.onContinueUserActivity(SushitrainApp.viewRouteActivityID) { ua in
						Log.info("Receive view-route handoff at app level: \(String(describing: ua.userInfo))")
					}
				#endif
		}
		.commands {
			self.commands()
		}
		#if os(macOS)
			.handlesExternalEvents(matching: ["*"])

			.onChange(of: scenePhase, initial: true) { _, _ in
				// Retain the scene's action once, so Dock clicks also work after all windows close.
				if appDelegate.openWindow == nil {
					appDelegate.openWindow = openWindow
				}
			}

			.onChange(of: hideInDock, initial: true) { _ov, nv in
				NSApp.setActivationPolicy(nv ? .accessory : .regular)
				NSApp.activate(ignoringOtherApps: true)
			}

			.defaultLaunchBehavior(hideInDock ? .suppressed : .presented)
			.restorationBehavior(hideInDock ? .disabled : .automatic)

		#endif

		#if os(macOS)
			Window("Import files", id: "import") {
				IncomingFilesWindow(incoming: appDelegate.incomingFiles)
					.environment(appState)
			}
			.defaultLaunchBehavior(.suppressed)
			.restorationBehavior(.disabled)
			.windowResizability(.contentSize)

			// About window
			Window("About Synctrain", id: "about") {
				AboutView().environment(appState)
			}
			.windowResizability(.contentSize)

			// Support window
			Window("Questions, support & feedback", id: "support") {
				NavigationStack {
					SupportView()
				}.environment(appState)
			}
			.windowResizability(.contentSize)

			// Pass app state object explicitly, as sometimes Environment fails to resolve
			// for some reason...
			MenuBarExtraView(appState: appState, hideInDock: $hideInDock)

			Settings {
				NavigationStack {
					TabbedSettingsView(hideInDock: $hideInDock)
				}.environment(appState)
			}
			.windowResizability(.contentSize)

			Window("Statistics", id: "stats") {
				TotalStatisticsView(appPersistentStatistics: appState.appPersistentStatistics)
					.environment(appState)
					.frame(minWidth: 320, maxWidth: 320, minHeight: 320)
			}
			.windowResizability(.contentSize)

			Window("Synctrain", id: "singleMain") {
				if appState.startupState == .started {
					MainView().environment(appState)
				}
			}
			.windowResizability(.contentSize)
		#endif
	}

	@CommandsBuilder private func commands() -> some Commands {
		CommandGroup(after: .sidebar) {
			Toggle("Hide dotfiles", isOn: appState.userSettings.$dotFilesHidden)
				.keyboardShortcut(".", modifiers: [.command, .shift])

			Toggle("Hide hidden folders", isOn: appState.userSettings.$hideHiddenFolders)
				.keyboardShortcut("P", modifiers: [.command, .shift])
		}

		#if os(macOS)
			BrowserRefreshCommands()

			CommandGroup(replacing: CommandGroupPlacement.help) {
				Button("Questions, support & feedback...") {
					openWindow(id: "support")
				}
			}

			CommandGroup(replacing: CommandGroupPlacement.appInfo) {
				Button("About Synctrain") {
					// Open the "about" window
					openWindow(id: "about")
				}

				Button("Statistics...") {
					openWindow(id: "stats")
				}

				Button("Decrypt a folder...") {
					openWindow(id: "decrypter")
				}

				Button("Restart Syncthing") {
					appState.restartEngine()
				}
				.disabled(
					(appState.startupState != .started && !appState.engineCanRestart) || appState.isEngineRestarting)
			}
		#endif
	}
}

#if os(macOS)
	private struct BrowserRefreshCommands: Commands {
		@FocusedValue(\.browserRefreshActions) private var actions

		var body: some Commands {
			CommandGroup(after: .toolbar) {
				Button("Refresh") {
					guard let actions else { return }
					Task { await actions.refresh() }
				}
				.keyboardShortcut("r", modifiers: .command)
				.disabled(actions == nil)

				Button("Rescan subdirectory") {
					guard let actions else { return }
					Task { await actions.rescan() }
				}
				.keyboardShortcut("r", modifiers: [.command, .shift])
				.disabled(actions == nil)
			}
		}
	}

	struct MenuBarExtraView: Scene {
		let appState: AppState
		@Binding var hideInDock: Bool
		@Environment(\.openWindow) private var openWindow

		@State private var folders: [SushitrainFolder] = []

		var body: some Scene {
			Window("Settings...", id: "appSettings") {
				NavigationStack {
					TabbedSettingsView(hideInDock: $hideInDock)
				}
			}.environment(appState)

			MenuBarExtra("Synctrain", systemImage: self.menuIcon, isInserted: $hideInDock) {
				if appState.startupState == .started {
					OverallStatusView()
						.environment(appState)
						.task {
							await self.update()
						}

					Button("Open file browser...", systemImage: "macwindow") {
						openWindow(id: "singleMain")
						NSApplication.shared.activate()
					}

					// List of folders
					if appState.userSettings.menuFolderAction != .hide {
						if !folders.isEmpty {
							Divider()
							ForEach(folders, id: \.folderID) { fld in
								Button("\(fld.displayName)") {
									self.openFolder(fld)
								}
							}
						}
					}

					Divider()

					Button("Settings", systemImage: "gear") {
						// Open the "about" window
						openWindow(id: "appSettings")
						NSApplication.shared.activate()
					}

					Button("Statistics", systemImage: "chart.pie") {
						openWindow(id: "stats")
						NSApplication.shared.activate()
					}
				}

				if appState.startupState == .started || appState.engineCanRestart {
					Button("Restart Syncthing", systemImage: "arrow.clockwise") {
						appState.restartEngine()
					}
					.disabled(appState.isEngineRestarting)
				}

				Button("About...", systemImage: "info.circle") {
					// Open the "about" window
					openWindow(id: "about")
					NSApplication.shared.activate()
				}

				Divider()

				Toggle(isOn: $hideInDock) {
					Label("Hide in dock", systemImage: "eye.slash")
				}

				Button("Quit Synctrain", systemImage: "multiply.circle") {
					NSApplication.shared.terminate(nil)
				}
			}
		}

		private func openFolder(_ fld: SushitrainFolder) {
			switch appState.userSettings.menuFolderAction {
			case .hide:
				break  // Should not be reached

			case .finder:
				if let lnu = fld.localNativeURL {
					openURLInSystemFilesApp(url: lnu)
				}
				else {
					openWindow(
						id: "folder",
						value: fld.folderID)
					NSApplication.shared.activate()
				}

			case .browser:
				openWindow(id: "folder", value: fld.folderID)
				NSApplication.shared.activate()

			case .finderExceptSelective:
				if fld.isSelective() != false {
					openWindow(
						id: "folder",
						value: fld.folderID)
					NSApplication.shared.activate()
				}
				else if let lnu = fld.localNativeURL {
					openURLInSystemFilesApp(url: lnu)
				}
				else {
					openWindow(
						id: "folder",
						value: fld.folderID)
					NSApplication.shared.activate()
				}
			}
		}

		private func update() async {
			self.folders = await appState.folders().filter { $0.isHidden == false }.sorted()
		}

		private var menuIcon: String {
			if appState.startupState == .started {
				if appState.client.connectedPeerCount() > 0 {
					if appState.client.isDownloading() || appState.client.isUploading() {
						return "folder.fill.badge.gearshape"
					}
					return "folder.fill"
				}
				else {
					return "folder"
				}
			}
			else {
				return "exclamationmark.triangle.fill"
			}
		}
	}
#endif
