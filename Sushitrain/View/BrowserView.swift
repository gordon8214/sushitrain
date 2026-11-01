// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import QuickLook
import UniformTypeIdentifiers
import PhotosUI
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
	private enum ConfirmableAction {
		case unlinkFolder
		case removeFolder

		var message: String {
			switch self {
			case .removeFolder:
				return String(
					localized:
						"Are you sure you want to remove this folder? Please consider carefully. All files in this folder will be removed from this device. Files that have not been synchronized to other devices yet cannot be recovered."
				)
			case .unlinkFolder:
				return String(
					localized:
						"Are you sure you want to unlink this folder? The folder will not be synchronized any longer. Files currently on this device will not be deleted."
				)
			}
		}

		var buttonTitle: String {
			switch self {
			case .removeFolder: return String(localized: "Remove the folder and all files")
			case .unlinkFolder: return String(localized: "Unlink the folder")
			}
		}
	}

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

	var folder: SushitrainFolder
	var prefix: String
	@ObservedObject var userSettings: AppUserSettings

	@State private var showSettings = false
	@State private var searchText = ""
	@State private var localNativeURL: URL? = nil
	@State private var folderExists = false
	@State private var showAddFilePicker = false
	@State private var showPhotoPicker = false
	@State private var selectedMedia: [PhotosPickerItem] = []
	@State private var isImportingMedia = false
	@State private var webViewAvailable = false
	@State private var viewStyle: BrowserViewStyle? = nil
	@State private var entriesSort: BrowserViewSort? = nil
	@State private var showAlert: ShowAlert? = nil
	@State private var showConfirmable: ConfirmableAction? = nil
	@State private var isBookmarked = false
	@State private var showNewBookmarkInfo = false
	@State private var recursive = false

	@State private var showIgnores = false
	@State private var showStatusPopover = false
	@State private var showDeletedFiles = false
	@State private var isSearching = false

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
		BrowserItemsView(
			folder: folder,
			prefix: prefix,
			// Note: this binding does *not* set appState.defaultBrowserViewStyle, because this one is primarily set programmatically
			viewStyle: $viewStyle,
			sortOrder: currentSort,
			recursive: recursive,
			searchText: $searchText,
			showSettings: $showSettings
		)

		.searchable(
			text: $searchText,
			isPresented: $isSearching,
			placement: SearchFieldPlacement.toolbar,
			prompt: "Search files in this folder..."
		)

		.navigationTitle("")

		.toolbar {
			self.toolbarContent()
		}
		.sheet(isPresented: $showSettings) {
			NavigationStack {
				FolderView(folder: self.folder)
					.presentationSizing(.fitted.sticky(horizontal: false, vertical: true))
					.frame(minWidth: 700, minHeight: 600)
					.toolbar {
						SheetButton(role: .save) {
							showSettings = false
						}
					}
			}
		}
		.sheet(isPresented: $showDeletedFiles) {
			DeletedFilesView(folder: folder)
		}
		.sheet(isPresented: $showIgnores) {
			NavigationStack {
				self.ignoreSheetView()
			}
		}

		.alert(
			"This location has been added to your bookmarks. Bookmarked locations can be accessed quickly from the start page.",
			isPresented: $showNewBookmarkInfo
		) {
			Button("OK") {}
		}

		.task {
			self.update()
		}

		.onChange(of: showSettings) { _, nv in
			// Needed to update the screen after removing a folder
			if !nv {
				Log.info("Update because of showSettings = \(nv)")
				self.folderExists = folder.exists()
				self.update()
			}
		}

		.fileImporter(
			isPresented: $showAddFilePicker,
			allowedContentTypes: [UTType.data],
			allowsMultipleSelection: true,
			onCompletion: self.addFilesFromImporter
		)
		.photosPicker(
			isPresented: $showPhotoPicker, selection: $selectedMedia,
			matching: .any(of: [.images, .videos]), preferredItemEncoding: .current
		)
		.onChange(of: selectedMedia) { _, items in
			guard !items.isEmpty else { return }
			selectedMedia = []
			importMedia(items)
		}
		.overlay {
			if isImportingMedia {
				ProgressView("Importing media…")
					.padding()
					.background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
			}
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
		.confirmationDialog(
			showConfirmable?.message ?? "", isPresented: Binding.isNotNil($showConfirmable), titleVisibility: .visible
		) {
			if let sc = showConfirmable {
				Button(sc.buttonTitle, role: .destructive, action: self.confirmedAction)
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

	@ToolbarContentBuilder private func toolbarContent() -> some ToolbarContent {
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

			self.folderMenu()
		}
	}

	@ViewBuilder private func ignoreSheetView() -> some View {
		IgnoresView(folder: folder)
			.navigationTitle("Files to ignore")
			.presentationSizing(.form)
			.frame(minWidth: 640, minHeight: 480)
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

			Toggle("Show files in subdirectories", systemImage: "text.insert", isOn: $recursive)

		} label: {
			Label("Filter and sort", systemImage: "line.3.horizontal.decrease")
		}
		.disabled(self.currentViewStyle().wrappedValue == .web)
	}

	private func addFilesFromImporter(result: Result<[URL], any Error>) {
		switch result {
		case .success(let fu):
			var accessing: [URL] = []
			for url in fu {
				if FolderAccess.startAccessing(url) {
					accessing.append(url)
				}
				else if FolderAccess.usesSecurityScope {
					Log.warn("failed to access security scoped URL from file importer: \(url)")
				}
			}

			Task { [accessing] in
				defer {
					for url in accessing {
						url.stopAccessingSecurityScopedResource()
					}
				}

				do {
					let numFilesAdded = try await self.dropFilesInBackground(fu)
					self.showToast(Toast(title: "\(numFilesAdded) files added", image: "document.badge.plus.fill"))
				}
				catch {
					Log.warn("failed to drop file: \(error)")
					self.showAlert = .error(error.localizedDescription)
				}
			}

		case .failure(_):
			break
		}
	}

	private func showInFinder() {
		// Open in Finder/Files
		if let localNativeURL = self.localNativeURL {
			openURLInSystemFilesApp(url: localNativeURL)
		}
	}

	private func importMedia(_ items: [PhotosPickerItem]) {
		let folder = self.folder
		let prefix = self.prefix
		isImportingMedia = true
		Task {
			defer { isImportingMedia = false }
			var count = 0
			var errors: [String] = []
			for item in items {
				do {
					guard let media = try await item.loadTransferable(type: ImportedMedia.self) else {
						throw CocoaError(.fileReadCorruptFile)
					}
					defer { try? FileManager.default.removeItem(at: media.directory) }
					count += try await Task.detached(priority: .utility) {
						try FileImport.copy([media.url], folder: folder, prefix: prefix)
					}.value
				}
				catch let failure as FileImport.Failure {
					count += failure.copiedURLs.count
					errors.append(failure.localizedDescription)
				}
				catch { errors.append(error.localizedDescription) }
			}
			if count > 0 {
				showToast(Toast(title: "\(count) files added", image: "document.badge.plus.fill"))
			}
			if !errors.isEmpty { showAlert = .error(errors.joined(separator: "\n")) }
		}
	}

	private func update() {
		self.folderExists = folder.exists()
		self.updateLocalURL()

		// Determine whether this view is bookmarked
		let route = self.route
		self.isBookmarked = userSettings.bookmarkedRoutes.contains(where: {
			Route(url: $0) == route
		})

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

	@ViewBuilder private func addMenuContents() -> some View {
		Button("Add from photo library...", systemImage: "photo.on.rectangle") {
			showPhotoPicker = true
		}.disabled(
			isImportingMedia || !folderExists || !self.folder.isRegularFolder || self.folder.isReceiveOnlyFolder)

		Button("Add files...", systemImage: "plus") {
			showAddFilePicker = true
		}.disabled(
			!folderExists || !self.folder.isRegularFolder || self.folder.isReceiveOnlyFolder)
	}

	@ViewBuilder private func folderOperationsMenu() -> some View {
		Menu {
			Button("Re-scan folder", systemImage: "sparkle.magnifyingglass", action: rescanFolder)
				.buttonStyle(.link)
				.keyboardShortcut("R", modifiers: [.command, .shift])

			Divider()

			Button("Unlink folder", systemImage: "folder.badge.minus", role: .destructive) {
				showConfirmable = .unlinkFolder
			}
			.buttonStyle(.link)
			.foregroundColor(.red)

			// Only allow removing a full folder when we are sure it is in the area managed by us
			if folder.isRegularFolder && folder.canRemoveFiles() {
				Button("Remove folder", systemImage: "trash", role: .destructive) {
					showConfirmable = .removeFolder
				}
				.buttonStyle(.link)
				.foregroundColor(.red)
			}
		} label: {
			Label("Folder actions", systemImage: "ellipsis")
		}.labelStyle(.titleAndIcon)
	}

	@ViewBuilder private func folderMenu() -> some View {
		let isReceiveEncrypted = folder.isReceiveEncryptedFolder
		Menu {
			if folderExists {
				self.addMenuContents()

				if folderExists && !self.prefix.isEmpty && !isReceiveEncrypted {
					if let entry = try? self.folder.getFileInformation(self.prefix.withoutEndingSlash) {
						NavigationLink(destination: EntryInfoView(entry: entry, showPath: false, siblings: nil)) {
							Label("Subdirectory properties...", systemImage: "folder.badge.gearshape")
						}
					}
				}

				if !isReceiveEncrypted {
					Toggle(isOn: Binding(get: { self.isBookmarked }, set: { self.setBookmarked($0) })) {
						Label("Bookmark", systemImage: self.isBookmarked ? "bookmark.fill" : "bookmark")
					}.labelStyle(.titleAndIcon)
				}

				Divider()

				if let versioningArchiveURL = folder.versioningArchiveURL {
					Button("File versions...", systemImage: "clock.arrow.circlepath") {
						openURLInSystemFilesApp(url: versioningArchiveURL)
					}
					.disabled(!folder.versioningArchiveExists)
				}

				if folder.canBrowseVersions() {
					Button("Deleted Files...", systemImage: "trash.circle") {
						showDeletedFiles = true
					}
				}
			}

			Button("Folder settings...", systemImage: "folder.badge.gearshape") {
				showSettings = true
			}

			if folder.isRegularFolder {
				Button("Files to ignore...", systemImage: "rectangle.dashed") {
					showIgnores = true
				}
			}

			self.folderOperationsMenu()
		} label: {
			Label("Folder settings", systemImage: "folder.badge.gearshape")
		}.disabled(!folderExists)
	}

	private var route: Route {
		return Route.folder(folderID: self.folder.folderID, prefix: self.prefix)
	}

	private func setBookmarked(_ fav: Bool) {
		let newRoute = self.route

		userSettings.bookmarkedRoutes.removeAll(where: { bookmarkedURL in
			guard let bookmarkedRoute = Route(url: bookmarkedURL) else {
				return true
			}

			return bookmarkedRoute == newRoute
		})

		if fav {
			userSettings.bookmarkedRoutes.append(newRoute.url)
		}
		Log.info("New bookmarks: \(userSettings.bookmarkedRoutes)")
		self.update()

		if fav {
			self.showNewBookmarkInfo = true
		}
	}

	private func confirmedAction() {
		do {
			switch self.showConfirmable {
			case .none:
				return
			case .unlinkFolder:
				try folder.unlinkFolderAndRemoveSettings()
				self.update()
			case .removeFolder:
				try folder.removeFolderAndSettings()
				self.update()
			}
		}
		catch let error {
			self.showAlert = .error(error.localizedDescription)
		}
		self.showConfirmable = .none
	}

	private func rescanFolder() {
		showToast(Toast(title: "Folder re-scan started", image: "sparkle.magnifyingglass"))

		do {
			try folder.rescan()
		}
		catch let error {
			self.showAlert = .error(error.localizedDescription)
		}
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
private struct ImportedMedia: Transferable {
	let url: URL
	var directory: URL { url.deletingLastPathComponent() }

	static var transferRepresentation: some TransferRepresentation {
		FileRepresentation(importedContentType: .data) { received in
			let directory = try AppLocations.current.makeTemporaryDirectory(prefix: "Import-")
			do {
				let url = directory.appendingPathComponent(received.file.lastPathComponent)
				try FileManager.default.copyItem(at: received.file, to: url)
				return ImportedMedia(url: url)
			}
			catch {
				try? FileManager.default.removeItem(at: directory)
				throw error
			}
		}
	}
}

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
	}

	private var isEmpty: Bool {
		return subdirectories.isEmpty && files.isEmpty
	}

	@ViewBuilder private func gridView() -> some View {
		GridScrollView(
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
