// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore
import Combine
import AppIntents
import SynctrainHostKit

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
		@AppStorage(AppPresence.storageKey) var presence = AppPresence.standard
	#endif

	init() {
		// The first start outside the sandbox takes over the sandboxed app's preferences and engine data
		#if os(macOS)
			let containerMigration = ContainerMigrator.run()
			AppPresence.adoptHideInDock(in: .standard)
		#endif

		// Find out all the relevant locations we need to access
		AppLocations.prepareCurrent()
		LegacyPhotoBackupPreferences.remove(from: .standard)
		let engineLocations = EngineLocations.current
		let configDirectory = Self.configDirectoryURL()
		let filesPath = engineLocations.files.path(percentEncoded: false)
		let configPath = configDirectory.path(percentEncoded: false)
		let enableLoggingToFile = UserDefaults.standard.bool(forKey: "loggingEnabled")
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
			EnginePriority.stored.apply()
			CrashWatch.shared.logLastRun()
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
			FinderIntegration.shared.start(appState)
		#endif

		AppDependencyManager.shared.add(dependency: appState)
		appState.isLoggingToFile = enableLoggingToFile
		let delegate = SushitrainDelegate(appState: appState)
		self.delegate = delegate
		client.delegate = self.delegate

		// Start Syncthing node in the background
		#if os(macOS)
			let activationPolicy = self.presence.activationPolicy
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
				NSApp.setActivationPolicy(activationPolicy)
			}
		#endif
	}

	/// The engine's configuration directory: on macOS the one the user chose, or Synctrain's own, on iOS the app's
	/// Application Support directory
	static func configDirectoryURL() -> URL {
		#if os(macOS)
			var isStale = false
			// A test run (SYNCTRAIN_TEST_ROOT) keeps its own choice (TestRun), and leaves the one the user made alone
			if !FolderAccess.usesSecurityScope, let path = ConfigDirectorySetting.path {
				// Never created here: if it can't be found (as on a disk that is not connected), the engine says so
				Log.info("Using custom config directory: \(path)")
				return URL(fileURLWithPath: path, isDirectory: true)
			}
			if EngineLocations.testRoot == nil && FolderAccess.usesSecurityScope,
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
			// As the first scene, this is the one that opens at launch and when reopening the app from the Dock
			.defaultLaunchBehavior(presence.inDock && !FinderItemWindow.isLaunch ? .automatic : .suppressed)
			.restoredOnlyWithDockIcon(in: presence)

			WindowGroup("Synctrain", id: FinderItemWindow.id, for: FinderAction.self) { [appState] action in
				if let action = action.wrappedValue {
					FinderItemWindow(action: action).environment(appState)
				}
			}
			.defaultSize(width: 700, height: 650)
			.defaultLaunchBehavior(.suppressed)
			.restorationBehavior(.disabled)

			WindowGroup(id: "bookmark", for: URL.self) { [appState] bookmarkURL in
				MainView(
					topLevelRoute: bookmarkURL.wrappedValue.flatMap { Route(url: $0) } ?? .start,
					initialColumnVisibility: .detailOnly
				).environment(appState)
			}
			.defaultLaunchBehavior(.suppressed)
			.restoredOnlyWithDockIcon(in: presence)

			WindowGroup(id: "preview", for: Preview.self) { [appState] preview in
				if appState.startupState == .started {
					if let p = preview.wrappedValue {
						PreviewWindow(preview: p)
							.environment(appState)
					}
				}
			}
			.windowManagerRole(.associated)
			.restoredOnlyWithDockIcon(in: presence)

			WindowGroup(id: "decrypter") {
				DecrypterView()
			}
			.restoredOnlyWithDockIcon(in: presence)
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

			.onChange(of: presence.activationPolicy, initial: true) { _ov, nv in
				NSApp.setActivationPolicy(nv)
				NSApp.activate(ignoringOtherApps: true)
			}

			// An action with no window to take it. While the engine starts, one may be on its way.
			.onChange(of: appState.startupState == .started ? qaService.action : nil, initial: true) { _, action in
				if action != nil && qaService.target == nil {
					openWindow(id: "main")
					NSApp.activate()
				}
			}
			.onChange(of: appState.startupState == .started ? qaService.finderRequest?.id : nil, initial: true) { _, request in
				if request != nil && qaService.target == nil {
					openWindow(id: "main")
					NSApp.activate()
				}
			}
			.onChange(of: appState.startupState == .started ? qaService.finderWindowRequest?.id : nil, initial: true) { _, id in
				guard let id, let request = qaService.finderWindowRequest, request.id == id,
					qaService.claimFinderWindowRequest(id)
				else { return }
				qaService.presentedFinderWindowRequest = request
				openWindow(id: FinderItemWindow.id, value: request.action)
				NSApplication.shared.unhideWithoutActivation()
				NSApplication.shared.activate()
				qaService.finishFinderWindowRequest(id)
			}

			// A click on a notification can also be what starts the app. The window waits for the engine, as any open window
			// keeps the one that says how the start went from opening (`openStartWindow`).
			.onChange(of: appState.startupState == .started && qaService.wantsConnectionRequests, initial: true) { _, wanted in
				if wanted {
					qaService.wantsConnectionRequests = false
					openWindow(id: "connectionRequests")
					NSApp.activate()
				}
			}

			.defaultLaunchBehavior(.suppressed)
			.restoredOnlyWithDockIcon(in: presence)

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
			.restoredOnlyWithDockIcon(in: presence)

			// Support window
			Window("Questions, support & feedback", id: "support") {
				NavigationStack {
					SupportView()
				}.environment(appState)
			}
			.windowResizability(.contentSize)
			.restoredOnlyWithDockIcon(in: presence)

			// Pass app state object explicitly, as sometimes Environment fails to resolve
			// for some reason...
			MenuBarExtraView(appState: appState, presence: $presence)

			Window("Connection requests", id: "connectionRequests") {
				NavigationStack {
					ConnectionRequestsView()
				}
				.environment(appState)
			}
			.restoredOnlyWithDockIcon(in: presence)

			// Not restored: the engine isn't running yet when a window comes back
			Window("Syncthing Web GUI", id: WebGUIWindow.id) {
				WebGUIWindow().environment(appState)
			}
			.defaultSize(width: 1100, height: 800)
			.defaultLaunchBehavior(.suppressed)
			.restorationBehavior(.disabled)

			Window("Statistics", id: "stats") {
				TotalStatisticsView(appPersistentStatistics: appState.appPersistentStatistics)
					.environment(appState)
					.frame(minWidth: 320, maxWidth: 320, minHeight: 320)
			}
			.windowResizability(.contentSize)
			.restoredOnlyWithDockIcon(in: presence)

			Window("Synctrain", id: "singleMain") {
				if appState.startupState == .started {
					MainView().environment(appState)
				}
			}
			.windowResizability(.contentSize)
			.restoredOnlyWithDockIcon(in: presence)
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

			CommandGroup(replacing: .appSettings) {
				Button("Settings…") {
					openWindow(id: "appSettings")
					NSApp.activate()
				}
				.keyboardShortcut(",", modifiers: .command)
			}

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

				Divider()

				EngineMenuItems(appState: appState, hasShortcuts: true)
					.labelStyle(.titleOnly)

				Divider()

				// Here too, as the settings only open once the engine has started
				Button("Switch to Files in Finder…") {
					ModeSwitch.askToSwitchToFilesInFinder()
				}
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
		@Binding var presence: AppPresence
		@Environment(\.openWindow) private var openWindow

		@State private var folders: [SushitrainFolder] = []

		var body: some Scene {
			Window("Settings", id: "appSettings") {
				SynchronizeSettingsView(presence: $presence)
			}
			.windowToolbarStyle(.unified)
			.defaultLaunchBehavior(.suppressed)
			.defaultSize(width: 1000, height: 720)
			.windowResizability(.contentMinSize)
			.environment(appState)
			.restoredOnlyWithDockIcon(in: presence)

			MenuBarExtra("Synctrain", systemImage: appState.engineStatus.symbol, isInserted: $presence.inMenuBar) {
				Text(verbatim: appState.engineStatus.menuLine)
				CrashNoticeMenuItems()

				Divider()

				if appState.startupState == .started {
					OverallStatusView()
						.environment(appState)
						.task {
							await self.update()
						}

					Button("Open file browser...", systemImage: "macwindow") {
						AppWindowPresentation.open {
							openWindow(id: "singleMain")
						}
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
				}
				else {
					Button("Open Synctrain", systemImage: "macwindow") {
						openStartWindow()
					}

					Divider()
				}

				if appState.startupState == .started || appState.engineCanRestart {
					EngineMenuItems(appState: appState)

					Divider()
				}

				if appState.startupState == .started {
					Button("Settings", systemImage: "gear") {
						AppWindowPresentation.open {
							openWindow(id: "appSettings")
						}
					}

					Button("Statistics", systemImage: "chart.pie") {
						AppWindowPresentation.open {
							openWindow(id: "stats")
						}
					}
				}

				Button("About...", systemImage: "info.circle") {
					// Open the "about" window
					AppWindowPresentation.open {
						openWindow(id: "about")
					}
				}

				Divider()

				Toggle(isOn: $presence.inDock) {
					Label("Show in Dock", systemImage: "dock.rectangle")
				}

				Button("Quit Synctrain", systemImage: "multiply.circle") {
					NSApplication.shared.terminate(nil)
				}
			}
			.onChange(of: appState.startupState, initial: true) { _, state in
				// Nothing else opens a window here, and without one the engine would wait for good
				if (!presence.inDock || FinderItemWindow.isLaunch) && state.needsWindow {
					openStartWindow()
				}
			}
		}

		/// Shows how the start is going, which only the main window does. A group adds a window each time it is opened.
		private func openStartWindow() {
			AppWindowPresentation.open {
				if !NSApp.windows.contains(where: { $0.isVisible && $0.canBecomeMain }) {
					openWindow(id: "main")
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
					AppWindowPresentation.open {
						openWindow(id: "folder", value: fld.folderID)
					}
				}

			case .browser:
				AppWindowPresentation.open {
					openWindow(id: "folder", value: fld.folderID)
				}
			}
		}

		private func update() async {
			self.folders = await appState.folders().filter { $0.isHidden == false }.sorted()
		}
	}
#endif
