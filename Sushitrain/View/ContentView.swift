// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SushitrainCore
import SwiftUI
import UniformTypeIdentifiers
import SynctrainHostKit

struct MainView: View {
	@Environment(AppState.self) private var appState
	@State var topLevelRoute: Route? = .start
	var initialColumnVisibility: NavigationSplitViewVisibility = .doubleColumn
	@State var host = WindowReference()

	var body: some View {
		Group {
			switch appState.startupState {
			case .notStarted:
				LoadingMainView(appState: appState)

			case .engineSetup(let decision):
				SyncthingTakeoverView(decision: decision)

			case .onboarding:
				OnboardingView(allowSkip: false)

			case .error(let e):
				StartupErrorView(message: e)
			case .started:
				ContentView(topLevelRoute: topLevelRoute, initialColumnVisibility: initialColumnVisibility, host: host)
					.showsToast()  // Back-up
			}
		}
		.background(
			WindowReader { window in
				QuickActionService.shared.add(window)
				host = WindowReference(window: window)
			}
		)
		// A window comes back with what it showed, as `singleMain` does
		.onAppear {
			if let window = host.window {
				QuickActionService.shared.add(window)
			}
		}
		.onDisappear {
			if let window = host.window {
				QuickActionService.shared.remove(window)
			}
		}
	}
}

/// The window a view is in. A window holds the state of its views, so holding it there would keep both for good.
struct WindowReference {
	weak var window: NSWindow?
}

/// Says which window a view is in
private struct WindowReader: NSViewRepresentable {
	let onWindow: (NSWindow) -> Void

	final class Reader: NSView {
		var onWindow: ((NSWindow) -> Void)?

		override func viewDidMoveToWindow() {
			super.viewDidMoveToWindow()
			guard let window, let onWindow else { return }
			// This happens during a view update, which can't change state
			DispatchQueue.main.async { onWindow(window) }
		}
	}

	func makeNSView(context: Context) -> Reader {
		let reader = Reader()
		reader.onWindow = onWindow
		return reader
	}

	func updateNSView(_ reader: Reader, context: Context) {}
}

struct NavigateToAction {
	typealias Action = (Route) -> Void
	let action: Action

	func callAsFunction(_ route: Route) {
		action(route)
	}
}

extension EnvironmentValues {
	@Entry var navigateTo = NavigateToAction(action: { _ in })
}

private struct ContentView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.scenePhase) var scenePhase
	@Environment(\.horizontalSizeClass) private var horizontalSizeClass

	@State private var desktopEditors = MacSettingsNavigation(mode: .synchronize)
	@State private var browserNavigation = DesktopBrowserNavigation()
	@State private var lastBrowserRoute: Route?
	@State private var settingsSearch = SettingsSearch(catalog: .desktop, platform: "macOS")
	@AppStorage(AppPresence.storageKey) private var presence = AppPresence.standard
	@State private var showCustomConfigWarning = false
	@State private var showContainerFolders = false
	@State var topLevelRoute: Route? = .start
	@State private var columnVisibility = NavigationSplitViewVisibility.doubleColumn
	@State private var initialSearchTerm: String = ""
	@State private var error: String? = nil

	@State private var addingFolder = AddFolder()
	@State private var showAddingFolder = false

	// Tracks the route within the folder tab; used to force navigating back
	@Observable class FoldersRouteManager {
		var route: [Route] = []
	}
	@State private var foldersTabRouteManager = FoldersRouteManager()
	@State private var afterSelection: (() -> Void)? = nil

	let host: WindowReference

	init(topLevelRoute: Route?, initialColumnVisibility: NavigationSplitViewVisibility, host: WindowReference) {
		self._topLevelRoute = State(initialValue: topLevelRoute)
		self._columnVisibility = State(initialValue: initialColumnVisibility)
		self.host = host
	}

	/// What an action asks of this window, if it's the window that takes it
	private var quickAction: Route? {
		let service = QuickActionService.shared
		guard let action = service.action, let window = host.window, window === service.target else {
			return nil
		}
		return action
	}

	private var finderRequest: FinderActionRequest? {
		let service = QuickActionService.shared
		// Keep observing the queue while an attached sheet makes this window ineligible to receive its next request.
		guard let request = service.finderRequest, let window = host.window, window === service.target else { return nil }
		return request
	}

	@ViewBuilder private func splitBody() -> some View {
		NavigationSplitView(
			columnVisibility: $columnVisibility,
			sidebar: {
				DesktopEngineSidebar(
					selection: Binding(get: { topLevelRoute }, set: { requestDesktopRoute($0) }), navigation: desktopEditors,
					search: settingsSearch
				)
				.disabled(desktopEditors.isSaving)
				.contextMenu {
					FolderMetricPickerView(userSettings: appState.userSettings)
				}
			},
			detail: {
				switch self.topLevelRoute {
				case .desktop(let id):
					Group {
						if MacSettingsCategory(rawValue: id) != nil || desktopEditors.searchTarget != nil {
							MacManagementDetail(
								destination: id, mode: .synchronize, navigation: desktopEditors, presence: $presence, appState: appState)
						}
						else if id == "files", let route = lastBrowserRoute {
							DesktopRetainedBrowser(route: route, navigation: browserNavigation).id(route)
						}
						else {
							NavigationStack { DesktopSynchronizePage(destination: id) }
						}
					}.showsToast()
				case .start:
					NavigationStack {
						StartOrSearchView(topLevelRoute: $topLevelRoute, initialSearchTerm: $initialSearchTerm)
					}.showsToast()

				case .search(for: let searchFor):
					NavigationStack {
						self.searchView(for: searchFor)
					}.showsToast()

				case .devices:
					NavigationStack {
						DevicesView()
					}.showsToast()

				case .folders:
					NavigationStack(path: $foldersTabRouteManager.route) {
						DesktopManagedFolders()
					}.showsToast()

				case .folder:
					if let route = topLevelRoute {
						DesktopRetainedBrowser(route: route, navigation: browserNavigation).id(route)
					}

				case .file(let folderID, let path):
					NavigationStack(path: $foldersTabRouteManager.route) {
						RouteView(route: self.topLevelRoute!)
							.navigationDestination(for: Route.self) { r in
								RouteView(route: r)
							}
					}.id(folderID + ":" + path).showsToast()

				case nil:
					ContentUnavailableView("Select a folder", systemImage: "folder")
						.onTapGesture {
							columnVisibility = .doubleColumn
						}
						.showsToast()
				}
			}
		)
		.navigationSplitViewStyle(.balanced)
	}

	var body: some View {
		Group {
			self.splitBody().id(desktopEditors.editorRevision)
		}
		.modifier(
			FinderActionModifier(
				request: finderRequest,
				claim: { id in
					guard QuickActionService.shared.claimFinderRequest(id) else { return false }
					NSApplication.shared.unhideWithoutActivation()
					host.window?.makeKeyAndOrderFront(nil)
					NSApplication.shared.activate()
					return true
				},
				finish: { QuickActionService.shared.finishFinderRequest($0) })
		)
		// Initially too: an action that started the app waited for the engine, which this view comes after
		.onChange(of: quickAction, initial: true) { _, route in
			guard let route else { return }
			QuickActionService.shared.action = nil
			self.navigate(to: route)
			NSApplication.shared.unhideWithoutActivation()
			host.window?.makeKeyAndOrderFront(nil)
			NSApplication.shared.activate()
		}
		// The split view removes the pages of the previous selection among the actions this runs with, and this comes after
		.onChange(of: topLevelRoute, initial: true) {
			if case .folder = topLevelRoute { lastBrowserRoute = topLevelRoute }
			if let afterSelection {
				self.afterSelection = nil
				Task { afterSelection() }
			}
		}
		.onChange(of: scenePhase) { oldPhase, newPhase in
			self.appState.onScenePhaseChange(from: oldPhase, to: newPhase)
		}

		.alert(isPresented: Binding.isNotNil($error)) {
			Alert(
				title: Text("An error has occurred"),
				message: Text(error ?? ""),
				dismissButton: .default(Text("OK")) {
					self.error = nil
				})
		}

		.sheet(isPresented: $showAddingFolder) {
			NavigationStack {
				AddFolderView(adding: addingFolder, shown: $showAddingFolder)
			}
		}

		.onAppear {
			AppState.requestNotificationPermissionIfNecessary()
		}
		// Once, after the app moved out of its sandbox container
		.task {
			guard !FolderAccess.usesSecurityScope,
				!UserDefaults.standard.bool(forKey: ContainerMigrator.folderMoveOfferedKey)
			else {
				return
			}
			// Before looking, so that another window doesn't offer it too
			UserDefaults.standard.set(true, forKey: ContainerMigrator.folderMoveOfferedKey)
			if await appState.folders().contains(where: { ContainerMigrator.isInContainer($0) }) {
				showContainerFolders = true
			}
		}
		.sheet(isPresented: $showContainerFolders) {
			NavigationStack {
				ContainerFoldersSheet(isPresented: $showContainerFolders)
			}
		}

		.onContinueUserActivity(SushitrainApp.viewRouteActivityID) { ua in
			if let userInfo = ua.userInfo {
				Log.info("Receive view route handoff \(userInfo)")
				if let version = ua.userInfo?["version"] as? Int,
					version == 1,
					let urlString = ua.userInfo?["url"] as? String,
					let url = URL(string: urlString),
					let route = Route(url: url)
				{
					self.navigate(to: route)
				}
			}
		}
		.environment(\.settingsNavigation, desktopEditors)
		.background(SettingsWindowCloseGuard(navigation: desktopEditors))
		.managementDiscardAlert(navigation: desktopEditors)
		.onChange(of: desktopEditors.searchTarget?.id) { _, _ in
			if let entry = desktopEditors.searchTarget {
				browserNavigation.deactivate()
				topLevelRoute = .desktop(entry.category)
			}
		}
		.task(id: settingsSearchRevision) {
			settingsSearch.mode = "synchronize"
			guard appState.startupState == .started, !appState.isEngineRestarting else {
				settingsSearch.resetEngine()
				return
			}
			let client = appState.client
			await settingsSearch.refresh {
				let data = try await goBlocking { try client.settingsSearchIndex() }
				return try JSONDecoder().decode([SettingSearchEntry].self, from: data)
			}
		}
		.task { MCPIntegration.shared.start(appState) }

		.environment(\.navigateTo, NavigateToAction(action: { route in self.navigate(to: route) }))
	}

	private var settingsSearchRevision: String {
		"\(appState.finderConfigurationGeneration).\(appState.startupState == .started).\(appState.isEngineRestarting)"
	}

	private func requestDesktopRoute(_ route: Route?) {
		desktopEditors.requestDeparture {
			browserNavigation.deactivate()
			if let route, case .folder = route { browserNavigation.session(for: route).path = [] }
			topLevelRoute = route
		}
	}

	private func exists(route: Route) -> Bool {
		switch route {
		case .desktop, .devices, .start, .search(for: _), .folders:
			return true
		case .folder(let folderID, let prefix):
			guard appState.client.folder(withID: folderID) != nil else {
				return false
			}

			if let p = prefix {
				// TODO: check if prefix points to an actual folder
				return p.isEmpty || p.hasSuffix("/")
			}
			else {
				return true
			}
		case .file(let folderID, let path):
			guard let folder = appState.client.folder(withID: folderID) else {
				return false
			}

			guard let entry = try? folder.getFileInformation(path) else {
				return false
			}

			return !entry.isDeleted() && !entry.isSymlink()
		}
	}

	private func navigate(to route: Route) {
		desktopEditors.requestDeparture { navigateAccepted(to: route) }
	}

	private func navigateAccepted(to route: Route) {
		Log.info("Navigate to route=\(route) splitted=\(route.splitted)")

		if !self.exists(route: route) {
			self.error = String(localized: "The requested item cannot be found on this device.")
			return
		}

		browserNavigation.deactivate()

		// The start page has the search field
		if case .search(for: let searchFor) = route {
			self.select(.start) {
				self.initialSearchTerm = searchFor
			}
			return
		}

		if case .folder(folderID: _, prefix: _) = route {
			let splitted = route.splitted
			self.select(splitted[0]) {
				self.browserNavigation.session(for: splitted[0]).path = Array(splitted.dropFirst())
			}
		}
		else {
			self.select(route)
		}
	}

	/// Selects a route with nothing in front of it. Pages opened by a link are not in the stack's path, so only a change of
	/// selection removes them, which is why this goes through no selection when the route is the selection already.
	private func select(_ route: Route, then action: (() -> Void)? = nil) {
		self.foldersTabRouteManager.route = []
		if self.topLevelRoute == route {
			self.topLevelRoute = nil
			self.afterSelection = { self.select(route, then: action) }
		}
		else {
			self.topLevelRoute = route
			self.afterSelection = action
		}
	}

	@ViewBuilder private func searchView(for searchFor: String) -> some View {
		NavigationStack {
			SearchView(
				prefix: "",
				initialSearchText: searchFor
			)
			.navigationTitle("Search")
		}
	}
}

struct RouteView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.desktopBrowserSession) private var browserSession
	let route: Route

	var body: some View {
		switch route {
		case .file(let folderID, let path):
			if let folder = self.appState.client.folder(withID: folderID) {
				if folder.exists() {
					if let entry = try? folder.getFileInformation(path) {
						EntryInfoView(entry: entry, showPath: false, siblings: nil)
					}
					else {
						ContentUnavailableView("File not found", systemImage: "document")
					}
				}
				else {
					ContentUnavailableView(
						"Folder was deleted", systemImage: "trash",
						description: Text("This folder was deleted."))
				}
			}
			else {
				ContentUnavailableView("Folder not found", systemImage: "folder")
			}

		case .folder(let folderID, let prefix):
			if let folder = self.appState.client.folder(withID: folderID) {
				if folder.exists() {
					BrowserView(
						folder: folder, prefix: prefix ?? "", userSettings: appState.userSettings,
						state: browserSession?.settings(for: .folder(folderID: folderID, prefix: prefix ?? ""))
					)
					#if SYNCTRAIN_BENCHMARK
						.onAppear { EngineBenchmark.folderIsVisible = true }
						.onDisappear { EngineBenchmark.folderIsVisible = false }
					#endif
				}
				else {
					ContentUnavailableView(
						"Folder was deleted", systemImage: "trash",
						description: Text("This folder was deleted."))
				}
			}
			else {
				ContentUnavailableView("Select a folder", systemImage: "folder")
			}

		case .desktop, .folders, .start, .devices, .search:
			EmptyView()
		}
	}
}

private struct LoadingMainView: View {
	@State var appState: AppState
	var body: some View {
		NavigationSplitView(
			sidebar: {
				EmptyView()
			},
			detail: {
				NavigationStack {
					LoadingView(appState: appState)
						.navigationTitle("Start")
						.searchable(text: .constant(""))
				}
			}
		)
		.navigationSplitViewStyle(.balanced)
	}
}

private struct LoadingView: View {
	@State var appState: AppState

	var body: some View {
		VStack(spacing: 10) {
			ProgressView().progressViewStyle(.circular)
			if !appState.isMigratedToNewDatabase {
				// We are likely migrating now
				Text(
					"Upgrading the database. This may take a few minutes, depending on how many files you have. Please do not close the app until this is finished."
				)
				.foregroundStyle(.secondary)
				.multilineTextAlignment(.center)
				.frame(maxWidth: 320)
			}
		}
	}
}

private struct StartOrSearchView: View {
	@Environment(AppState.self) private var appState
	@Binding var topLevelRoute: Route?
	@Binding var initialSearchTerm: String
	@State private var searchText: String = ""
	@FocusState private var isSearchFieldFocused

	// This is needed because isSearching is not available from the parent view
	struct InnerView: View {
		@Environment(AppState.self) private var appState
		@Binding var topLevelRoute: Route?
		@Binding var searchText: String
		@Environment(\.isSearching) private var isSearching

		var body: some View {
			if isSearching {
				SearchResultsView(
					userSettings: appState.userSettings,
					searchText: $searchText,
					folderID: .constant(""),
					prefix: .constant("")
				)
			}
			else {
				StartView(topLevelRoute: $topLevelRoute, userSettings: appState.userSettings)
			}
		}
	}

	@ViewBuilder private func view() -> some View {
		InnerView(topLevelRoute: $topLevelRoute, searchText: $searchText)
			.searchable(
				text: $searchText, placement: SearchFieldPlacement.toolbar,
				prompt: "Search all files and folders..."
			)
			.autocorrectionDisabled()
			// Once: coming back to this page later doesn't search again
			.onChange(of: initialSearchTerm, initial: true) { _, term in
				if !term.isEmpty {
					searchText = term
					initialSearchTerm = ""
					isSearchFieldFocused = true
				}
			}
	}

	var body: some View {
		if #available(iOS 18, *) {
			self.view().searchFocused($isSearchFieldFocused)
		}
		else {
			self.view()
		}
	}
}
