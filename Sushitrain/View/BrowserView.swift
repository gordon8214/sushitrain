// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import QuickLook
import UniformTypeIdentifiers
@preconcurrency import SushitrainCore
import SynctrainHostKit

struct BrowserRefreshActions {
	let refresh: @MainActor () async -> Void
	let rescan: @MainActor () async -> Void
}

private struct BrowserRefreshActionsKey: FocusedValueKey {
	typealias Value = BrowserRefreshActions
}

extension FocusedValues {
	var browserRefreshActions: BrowserRefreshActions? {
		get { self[BrowserRefreshActionsKey.self] }
		set { self[BrowserRefreshActionsKey.self] = newValue }
	}
}

enum BrowserViewStyle: String {
	case grid = "grid"
	case list = "list"
	case thumbnailList = "thumbnailList"
	case web = "web"
}

enum BrowserViewSortAttribute: String, Codable {
	case name = "name"
	case size = "size"
	case dateModified = "dateModified"
}

struct BrowserViewSort: Codable, Hashable {
	var attribute: BrowserViewSortAttribute = .name
	var ascending: Bool = true

	func isOrderedBefore(_ lhs: SushitrainEntry, _ rhs: SushitrainEntry) -> Bool {
		let r: Bool
		switch self.attribute {
		case .name:
			r = lhs.fileName().compare(rhs.fileName(), options: .numeric) == .orderedAscending
		case .size:
			r = lhs.size() < rhs.size()
		case .dateModified:
			if let lhd = lhs.modifiedAt()?.date(), let rhd = rhs.modifiedAt()?.date() {
				r = lhd.compare(rhd) == .orderedAscending
			}
			else if lhs.modifiedAt() == nil {
				r = rhs.modifiedAt() == nil ? true : false
			}
			else {
				r = lhs.modifiedAt() == nil ? false : true
			}
		}
		return ascending ? r : !r
	}
}

private struct FolderPopoverView: View {
	let folder: SushitrainFolder

	var body: some View {
		NavigationStack {
			FolderStatisticsView(folder: folder).frame(minWidth: 320, minHeight: 420, maxHeight: 400)
		}
	}
}

struct BrowserView: View {
	private enum ShowAlert: Identifiable {
		case error(String)

		var id: String {
			switch self {
			case .error(let e): return e
			}
		}
	}

	@Environment(AppState.self) private var appState
	@Environment(\.showToast) private var showToast
	@Environment(\.desktopBrowserSession) private var browserSession

	var folder: SushitrainFolder
	var prefix: String
	@ObservedObject var userSettings: AppUserSettings

	@State private var browserState: DesktopBrowserSettings
	init(folder: SushitrainFolder, prefix: String, userSettings: AppUserSettings, state: DesktopBrowserSettings? = nil) {
		self.folder = folder
		self.prefix = prefix
		self.userSettings = userSettings
		_browserState = State(initialValue: state ?? DesktopBrowserSettings())
	}
	private var searchText: String {
		get { browserState.searchText }
		nonmutating set { if browserSession?.active != false { browserState.searchText = newValue } }
	}
	private var viewStyle: BrowserViewStyle? {
		get { browserState.viewStyle }
		nonmutating set { if browserSession?.active != false { browserState.viewStyle = newValue } }
	}
	private var entriesSort: BrowserViewSort? {
		get { browserState.sort }
		nonmutating set { if browserSession?.active != false { browserState.sort = newValue } }
	}
	private var recursive: Bool {
		get { browserState.recursive }
		nonmutating set { if browserSession?.active != false { browserState.recursive = newValue } }
	}
	private var isSearching: Bool {
		get { browserState.searching }
		nonmutating set { if browserSession?.active != false { browserState.searching = newValue } }
	}
	@State private var localNativeURL: URL? = nil
	@State private var folderExists = false
	@State private var webViewAvailable = false

	@State private var showAlert: ShowAlert? = nil

	@State private var showStatusPopover = false

	private func currentViewStyle() -> Binding<BrowserViewStyle> {
		return Binding(
			get: { self.viewStyle ?? appState.userSettings.defaultBrowserViewStyle },
			set: {
				self.viewStyle = $0
				if $0 != .web {
					appState.userSettings.defaultBrowserViewStyle = $0
				}
			})
	}
	private var currentSort: BrowserViewSort {
		return self.entriesSort ?? appState.userSettings.defaultBrowserSort.value ?? BrowserViewSort()
	}

	private var folderName: String {
		if prefix.isEmpty {
			return self.folder.label()
		}
		let parts = prefix.split(separator: "/")
		if parts.count > 0 {
			return String(parts[parts.count - 1])
		}
		return prefix
	}

	var body: some View {
		FolderActionsView(
			folder: folder, prefix: prefix, userSettings: userSettings,
			enablesRescanShortcut: true, onChange: update
		) { folderMenu, showSettings in
			BrowserItemsView(
				folder: folder,
				prefix: prefix,
				// Note: this binding does *not* set appState.defaultBrowserViewStyle, because this one is primarily set programmatically
				viewStyle: Binding(get: { viewStyle }, set: { viewStyle = $0 }),
				sortOrder: currentSort,
				recursive: recursive,
				searchText: Binding(get: { searchText }, set: { searchText = $0 }),
				showSettings: showSettings
			)

			.searchable(
				text: Binding(get: { searchText }, set: { searchText = $0 }),
				isPresented: Binding(get: { isSearching }, set: { isSearching = $0 }),
				placement: SearchFieldPlacement.toolbar,
				prompt: "Search files in this folder..."
			)

			.navigationTitle("")

			.toolbar {
				self.toolbarContent(folderMenu: folderMenu)
			}

			.task {
				self.update()
			}

			.contextMenu {
				if let entry = try? self.folder.getFileInformation(self.prefix.withoutEndingSlash) {
					NavigationLink(destination: EntryInfoView(entry: entry, showPath: false, siblings: nil)) {
						Label("Subdirectory properties...", systemImage: entry.systemImage)
					}

				}
			}
			.onDrop(
				of: ["public.file-url"], isTargeted: nil,
				perform: { providers, _ in
					Task {
						self.showAlert = nil
						do {
							try await self.onDrop(providers)
						}
						catch {
							Log.warn("Failed to drop: \(error.localizedDescription)")
							self.showAlert = .error(error.localizedDescription)
						}
					}
					return true
				}
			)
			.alert(item: $showAlert) { alert in
				switch alert {
				case .error(let err):
					Alert(
						title: Text("An error occurred"),
						message: Text(err),
						dismissButton: .default(Text("OK"))
					)
				}
			}

			.userActivity(SushitrainApp.viewRouteActivityID) { ua in
				let routeURL = self.route.url
				ua.title = self.folderName
				ua.isEligibleForHandoff = true
				ua.targetContentIdentifier = routeURL.absoluteString
				ua.userInfo = [
					"version": 1,
					"url": routeURL.absoluteString,
				]
				ua.needsSave = true
			}
		}
	}

	@ToolbarContentBuilder private func toolbarContent(folderMenu: FolderMenuContents) -> some ToolbarContent {
		ToolbarItem(placement: .navigation) {
			let fsd = FolderStatusDescription(folder)
			HStack(alignment: .center) {
				Button(fsd.text, systemImage: fsd.systemImage) {
					showStatusPopover = true
				}
				.animation(.spring(), value: fsd.systemImage)
				.labelStyle(.iconOnly)
				.foregroundStyle(fsd.color)
				.accessibilityLabel(fsd.text)
				.popover(isPresented: $showStatusPopover, arrowEdge: .bottom) {
					FolderPopoverView(folder: folder)
				}
				Text(folderName).font(.headline).padding(.trailing, 20)
			}
		}

		ToolbarItemGroup(placement: .status) {
			Picker("View as", selection: self.currentViewStyle()) {
				Image(systemName: "list.bullet").tag(BrowserViewStyle.list)
					.accessibilityLabel(Text("List"))
					.keyboardShortcut("1", modifiers: .command)
				Image(systemName: "checklist.unchecked").tag(BrowserViewStyle.thumbnailList)
					.accessibilityLabel(Text("List with previews"))
					.keyboardShortcut("2", modifiers: .command)
				Image(systemName: "square.grid.2x2").tag(BrowserViewStyle.grid)
					.accessibilityLabel(Text("Grid"))
					.keyboardShortcut("3", modifiers: .command)

				if webViewAvailable {
					Image(systemName: "doc.text.image")
						.tag(BrowserViewStyle.web)
						.accessibilityLabel(Text("Web page"))
						.keyboardShortcut("4", modifiers: .command)
				}
			}
			.pickerStyle(.segmented)
			.disabled(isSearching)

			self.filterMenu()
		}

		ToolbarItemGroup(placement: .primaryAction) {
			Button(openInFilesAppLabel, systemImage: "arrow.up.forward.app") {
				self.showInFinder()
			}
			.disabled(localNativeURL == nil || isSearching)
			.keyboardShortcut("G", modifiers: .command)

			Menu {
				folderMenu
			} label: {
				Label("Folder settings", systemImage: "folder.badge.gearshape")
			}.disabled(!folderExists)
		}
	}

	@ViewBuilder private func filterMenu() -> some View {
		Menu {
			// Sort options
			Picker(
				"Sort",
				selection: Binding(
					get: { self.currentSort.attribute },
					set: {
						self.entriesSort = self.currentSort
						self.entriesSort?.attribute = $0
						userSettings.defaultBrowserSort = AsJSON(self.entriesSort)
					})
			) {
				Label("By name", systemImage: "textformat.size").tag(BrowserViewSortAttribute.name)
				Label("By size", systemImage: "scalemass.fill").tag(BrowserViewSortAttribute.size)
				Label("By date", systemImage: "calendar").tag(BrowserViewSortAttribute.dateModified)
			}.pickerStyle(.inline)
				.disabled(self.viewStyle != .grid)

			Toggle(
				"Descending", systemImage: "chart.bar.xaxis.descending",
				isOn: Binding(
					get: {
						return !self.currentSort.ascending
					},
					set: {
						self.entriesSort = self.currentSort
						self.entriesSort?.ascending = !$0
						userSettings.defaultBrowserSort = AsJSON(self.entriesSort)
					})
			)
			.disabled(self.viewStyle != .grid)

			Divider()

			Toggle("Hide dotfiles", systemImage: "eye.slash", isOn: userSettings.$dotFilesHidden)

			Toggle(
				"Show files in subdirectories", systemImage: "text.insert",
				isOn: Binding(get: { recursive }, set: { recursive = $0 }))

		} label: {
			Label("Filter and sort", systemImage: "line.3.horizontal.decrease")
		}
		.disabled(self.currentViewStyle().wrappedValue == .web)
	}

	private func showInFinder() {
		// Open in Finder/Files
		if let localNativeURL = self.localNativeURL {
			openURLInSystemFilesApp(url: localNativeURL)
		}
	}

	private func update() {
		self.folderExists = folder.exists()
		self.updateLocalURL()

		// Check for presence of index.html to enable web view
		if folderExists, !recursive, let entry = try? folder.getFileInformation(self.prefix + "index.html"),
			!entry.isDirectory() && !entry.isDeleted()
		{
			self.webViewAvailable = true
		}
		else {
			self.webViewAvailable = false
		}
	}

	private var route: Route {
		return Route.folder(folderID: self.folder.folderID, prefix: self.prefix)
	}

	private func onDrop(_ providers: [NSItemProvider]) async throws {
		var urls: [URL] = []

		for provider in providers {
			let data: Data? = try await withCheckedThrowingContinuation { cb in
				provider.loadDataRepresentation(forTypeIdentifier: "public.file-url") {
					(data, error) in
					if let e = error {
						cb.resume(throwing: e)
					}
					else {
						cb.resume(returning: data)
					}
				}
			}

			if let data = data, let path = NSString(data: data, encoding: 4),
				let url = URL(string: path as String)
			{
				urls.append(url)
			}
		}

		let numFilesAdded = try await self.dropFilesInBackground(urls)
		self.showToast(Toast(title: "\(numFilesAdded) files added", image: "plus"))
	}

	private func dropFilesInBackground(_ urls: [URL]) async throws -> Int {
		let folder = self.folder
		let prefix = self.prefix
		return try await Task.detached(priority: .utility) {
			return try FileImport.copy(urls, folder: folder, prefix: prefix)
		}.value
	}

	private func updateLocalURL() {
		if !self.folder.exists() {
			self.localNativeURL = nil
			return
		}

		// Check if we can get a local native URL
		var error: NSError? = nil
		let localNativePath = self.folder.localNativePath(&error)

		if error == nil {
			let localNativeURL = URL(fileURLWithPath: localNativePath).appendingPathComponent(
				self.prefix)

			if FileManager.default.fileExists(atPath: localNativeURL.path) {
				self.localNativeURL = localNativeURL
				return
			}
		}

		self.localNativeURL = nil
	}
}

/// Stage the picker-owned file before its transfer callback returns, without loading videos into memory.
private struct BrowserItemsView: View {
	private struct ReloadIdentity: Hashable {
		let folderID: String
		let prefix: String
		let state: Int
		let scanRevision: Int
		let sort: BrowserViewSort
		let recursive: Bool
		let hideDotFiles: Bool
	}

	private var reloadIdentity: ReloadIdentity {
		ReloadIdentity(
			folderID: folder.folderID, prefix: prefix, state: folder.folderStateForUpdating,
			scanRevision: appState.folderScanRevisions[folder.folderID, default: 0],
			sort: sortOrder, recursive: recursive,
			hideDotFiles: appState.userSettings.dotFilesHidden)
	}

	@State private var reloadGeneration = UUID()
	@State private var loadError: String?

	@Environment(AppState.self) private var appState
	@Environment(\.desktopBrowserSession) private var browserSession
	let folder: SushitrainFolder
	let prefix: String
	@Binding var viewStyle: BrowserViewStyle?
	let sortOrder: BrowserViewSort
	let recursive: Bool

	@Binding var searchText: String
	@Binding var showSettings: Bool

	@State private var subdirectories: [SushitrainEntry] = []
	@State private var files: [SushitrainEntry] = []
	@State private var isLoading = true
	@State private var showSpinner = false
	@State private var folderExists = false
	@State private var folderIsPaused = false

	@Environment(\.isSearching) private var isSearching

	var body: some View {
		ZStack {
			if folderExists {
				if !isSearching {
					switch self.viewStyle ?? appState.userSettings.defaultBrowserViewStyle {
					case .grid:
						self.gridView()

					case .list, .thumbnailList:
						self.listView()

					case .web:
						if #available(macOS 26, *) {
							BrowserWebView(folderID: folder.folderID, path: self.prefix)
								.backgroundExtensionEffect()
						}
						else {
							BrowserWebView(folderID: folder.folderID, path: self.prefix)
						}
					}
				}
				else {
					// Search
					SearchResultsView(
						userSettings: appState.userSettings,
						searchText: $searchText,
						folderID: .constant(self.folder.folderID),
						prefix: Binding(get: { prefix }, set: { _ in () })
					)
				}
			}
			else {
				Rectangle()
					.fill(Color.clear)
					.frame(width: .infinity, height: .infinity)
			}
		}
		.overlay {
			self.overlayView()
		}
		.refreshable {
			await self.rescan()
		}
		.focusedSceneValue(
			\.browserRefreshActions,
			folderExists && !isLoading
				? BrowserRefreshActions(
					refresh: { await self.reload() },
					rescan: { await self.rescan() })
				: nil
		)
		.contextMenu {
			Button("Refresh") {
				Task {
					await self.reload()
				}
			}.disabled(!folderExists)

			Button("Rescan subdirectory") {
				Task {
					await self.rescan()
				}
			}.disabled(!folderExists)
		}
		.task(id: reloadIdentity) {
			await self.reload()
		}
		.onAppear {
			if let saved = browserSession?.items(for: .folder(folderID: folder.folderID, prefix: prefix)) {
				subdirectories = saved.directories
				files = saved.files
				folderExists = saved.exists
				folderIsPaused = saved.paused
			}
		}
		.onDisappear {
			browserSession?.retain(
				DesktopBrowserItems(directories: subdirectories, files: files, exists: folderExists, paused: folderIsPaused),
				for: .folder(folderID: folder.folderID, prefix: prefix))
		}

	}

	private var isEmpty: Bool {
		return subdirectories.isEmpty && files.isEmpty
	}

	@ViewBuilder private func gridView() -> some View {
		GridScrollView(
			directory: .folder(folderID: folder.folderID, prefix: prefix),
			userSettings: appState.userSettings,
			header: {
				HStack {
					Spacer()

					// You can pinch on macOS using the touch pad, but it is handy to have a slider as well
					Slider(
						value: Binding(
							get: {
								return Double(appState.userSettings.browserGridColumns)
							},
							set: { nv in
								withAnimation(.spring(response: 0.8)) {
									appState.userSettings.browserGridColumns = Int(nv)
								}
							}
						),
						in: 1.0...10.0, step: 1.0
					)
					.frame(minWidth: 50, maxWidth: 150)
					.padding(.horizontal, 20)
					.padding(.vertical, 15)
				}
			},
			content: { columns in
				GridFilesView(
					userSettings: appState.userSettings,
					prefix: self.prefix,
					files: files,
					subdirectories: subdirectories,
					folder: folder,
					columns: columns
				)
				.padding(.leading, 5.0)
			})
	}

	@ViewBuilder private func listView() -> some View {
		BrowserTableView(
			folder: folder,
			prefix: prefix,
			files: files,
			subdirectories: subdirectories,
			viewStyle: viewStyle ?? appState.userSettings.defaultBrowserViewStyle
		)
	}

	@ViewBuilder private func overlayView() -> some View {
		if isLoading {
			if isEmpty && showSpinner {
				ProgressView()
			}
			// Load the rest while already showing a part of the results
		}
		else if let loadError {
			ContentUnavailableView {
				Label("Unable to load files", systemImage: "exclamationmark.triangle")
			} description: {
				Text(loadError)
			} actions: {
				Button("Retry") {
					Task { await self.reload() }
				}
			}
		}
		else if !folderExists {
			ContentUnavailableView(
				"Folder removed", systemImage: "trash",
				description: Text("This folder was removed."))
		}
		else if isEmpty && folderIsPaused {
			ContentUnavailableView(
				"Synchronization disabled", systemImage: "pause.fill",
				description: Text(
					"Synchronization has been disabled for this folder. Enable it in folder settings to access files."
				)
			).onTapGesture {
				showSettings = true
			}
		}
		else if isEmpty {
			if self.prefix == "" {
				if self.folder.connectedPeerCount() == 0 {
					ContentUnavailableView(
						"Not connected", systemImage: "network.slash",
						description: Text(
							"Share this folder with other devices to start synchronizing files."
						)
					).onTapGesture {
						showSettings = true
					}
				}
				else {
					if folder.isReceiveEncryptedFolder {
						ContentUnavailableView(
							"The files in this folder cannot be shown",
							systemImage: "lock.circle.dotted",
							description: Text(
								"This folder is receiving encrypted files. While these files are stored on this device, they cannot be accessed from this device."
							)
						)
					}
					else {
						ContentUnavailableView(
							"There are currently no files in this folder.",
							systemImage: "questionmark.folder",
							description: Text(
								"If this is unexpected, ensure that the other devices have accepted syncing this folder with your device."
							)
						).onTapGesture {
							showSettings = true
						}
					}
				}
			}
			else {
				ContentUnavailableView(
					"There are currently no files in this folder.",
					systemImage: "questionmark.folder")
			}
		}
	}

	private func rescan() async {
		Log.info("Rescan subdir \(self.prefix)")
		try? self.folder.rescanSubdirectory(self.prefix)
		await self.reload()
	}

	@MainActor private func reload() async {
		guard !Task.isCancelled else { return }
		let generation = UUID()
		self.reloadGeneration = generation
		let request = self.reloadIdentity
		let folder = self.folder
		self.isLoading = true
		self.showSpinner = false
		self.loadError = nil
		let loadingSpinnerTask = Task {
			try await Task.sleep(nanoseconds: 300_000_000)
			if !Task.isCancelled && self.reloadGeneration == generation && self.isLoading {
				self.showSpinner = true
			}
		}
		defer { loadingSpinnerTask.cancel() }

		do {
			let (exists, paused, newSubdirectories, newFiles) = try await Task.detached {
				dispatchPrecondition(condition: .notOnQueue(.main))
				guard folder.exists() else {
					return (false, false, [SushitrainEntry](), [SushitrainEntry]())
				}
				// Syncthing does not list a paused folder ("no such folder"); the view says it is paused instead
				if folder.isPaused() {
					return (true, true, [SushitrainEntry](), [SushitrainEntry]())
				}
				var dirNames = try folder.list(request.prefix, directories: true, recurse: false).asArray()
					.sorted(by: { $0.compare($1, options: .numeric) == .orderedAscending })
				if request.hideDotFiles {
					dirNames = dirNames.filter { !$0.starts(with: ".") }
				}
				// Individual entries can disappear while the directory is being listed.
				var directories = dirNames.compactMap { try? folder.getFileInformation(request.prefix + $0) }
				var files = try folder.listEntries(
					prefix: request.prefix, directories: false, hideDotFiles: request.hideDotFiles,
					recursive: request.recursive)
				// Virtual filesystem entries cannot be checked for local presence.
				if folder.isNativeFilesystem() {
					files = files.filter { $0.isLocallyPresent() }
					directories = directories.filter { $0.isLocallyPresent() }
				}
				files.sort(by: request.sort.isOrderedBefore)
				return (true, folder.isPaused(), directories, files)
			}.value

			// Detached work can finish after cancellation or after a newer refresh.
			guard !Task.isCancelled, self.reloadGeneration == generation else { return }
			self.isLoading = false
			self.folderExists = exists
			self.folderIsPaused = paused
			if self.isEmpty || self.viewStyle != .grid {
				self.files = newFiles
				self.subdirectories = newSubdirectories
				self.autoSelectViewStyle()
			}
			else {
				withAnimation {
					self.files = newFiles
					self.subdirectories = newSubdirectories
					self.autoSelectViewStyle()
				}
			}
		}
		catch {
			guard !Task.isCancelled, self.reloadGeneration == generation else { return }
			Log.warn("Error listing: \(error.localizedDescription)")
			self.isLoading = false
			self.loadError = error.localizedDescription
		}
	}

	private func autoSelectViewStyle() {
		// These files are ignored when deciding whether to switch to a grid view or not
		let extensionsIgnored = Set([".aae", ".ds_store", ".db", ".gitignore", ".stignore", ".ini"])

		if self.viewStyle == nil && !self.files.isEmpty {
			// Do we have an index.html? If so switch to web view
			if appState.userSettings.automaticallyShowWebpages && !recursive
				&& self.files.contains(where: { $0.fileName() == "index.html" })
			{
				self.viewStyle = .web
			}
			else if appState.userSettings.automaticallySwitchViewStyle {
				// Check if we only have thumbnailable files; if so, switch to thumbnail mode
				let dotFilesHidden = appState.userSettings.dotFilesHidden
				let filtered = self.files.filter({
					!extensionsIgnored.contains($0.extension().lowercased()) && (!dotFilesHidden || !$0.fileName().starts(with: "."))
				})

				if !filtered.isEmpty && filtered.allSatisfy({ $0.canThumbnail && ($0.isImage || $0.isVideo) }) {
					self.viewStyle = .grid
				}
				else {
					self.viewStyle = .thumbnailList
				}
			}
			else {
				self.viewStyle = appState.userSettings.defaultBrowserViewStyle
			}
		}
	}
}

extension SushitrainFolder {
	fileprivate var folderStateForUpdating: Int {
		if !self.exists() {
			return -1
		}
		var hasher = Hasher()
		hasher.combine(self.state())
		hasher.combine(self.isPaused())
		return hasher.finalize()
	}
}

struct FilesFooterView: View {
	let subdirectories: Int
	let files: Int

	var body: some View {
		// Show number of items
		Group {
			if self.subdirectories > 0 && self.files == 0 {
				Text("\(self.subdirectories) subdirectories")
			}
			else if self.files > 0 && self.subdirectories == 0 {
				Text("\(self.files) files")
			}
			else if self.files > 0 && self.subdirectories > 0 {
				Text("\(self.files) files and \(self.subdirectories) subdirectories")
			}
		}
		.font(.footnote)
		.foregroundColor(.secondary)
		.frame(maxWidth: .infinity)
	}
}
