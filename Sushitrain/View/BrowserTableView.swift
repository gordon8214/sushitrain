// Copyright (C) 2025 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import QuickLook
@preconcurrency import SushitrainCore

struct BrowserTableView: View {
	let folder: SushitrainFolder
	let prefix: String
	let files: [SushitrainEntry]
	let subdirectories: [SushitrainEntry]
	let viewStyle: BrowserViewStyle

	@State private var updateGeneration = 0
	@State private var scrollItem: SushitrainEntry.ID?
	@Environment(\.desktopBrowserSession) private var browserSession
	@State private var selection = Set<SushitrainEntry.ID>()
	@State private var openedEntry: (SushitrainEntry, Bool)? = nil
	@State private var sortOrder = [EntryComparator(order: .forward, sortBy: .name)]
	@State private var entries: [SushitrainEntry] = []
	@State private var showDownloaderFor: SushitrainEntry? = nil
	@State private var showVersionsFor: SushitrainEntry? = nil
	#if os(macOS)
		@State private var desktopEdit: DesktopEntryEdit?
		@State private var deletingEntry: DesktopEntryEdit?
		@State private var operationError: ErrorMessage?
	#endif

	@SceneStorage("BrowserTableViewConfig") private var columnCustomization: TableColumnCustomization<SushitrainEntry>
	@Environment(\.openURL) private var openURL
	@Environment(AppState.self) private var appState

	@Environment(\.openWindow) private var openWindow
	@Environment(\.pushBrowserRoute) private var pushBrowserRoute

	private static let formatter = ByteCountFormatter()

	private func entryById(_ id: SushitrainEntry.ID) -> SushitrainEntry? {
		if let found = subdirectories.first(where: { $0.id == id }) {
			return found
		}
		if let found = files.first(where: { $0.id == id }) {
			return found
		}
		return nil
	}

	private func copy(_ entry: SushitrainEntry) {
		if let url = entry.localNativeFileURL as? NSURL, let refURL = url.fileReferenceURL() {
			writeURLToPasteboard(url: refURL)
		}
		else if let url = URL(string: entry.onDemandURL()) {
			writeURLToPasteboard(url: url)
		}
	}

	var body: some View {
		Table(
			of: SushitrainEntry.self,
			selection: self.$selection,
			sortOrder: $sortOrder,
			columnCustomization: $columnCustomization,
			columns: {
				// Name and icon
				TableColumn("Name", sortUsing: EntryComparator(order: .forward, sortBy: .name)) {
					(entry: SushitrainEntry) in
					EntryNameView(entry: entry, viewStyle: self.viewStyle)
						.environment(self.appState)  // Needed because for some reason it does not propagate
				}
				.defaultVisibility(.visible)
				.customizationID("name")

				// File extension
				TableColumn(
					"File type", sortUsing: EntryComparator(order: .forward, sortBy: .fileExtension)
				) {
					(entry: SushitrainEntry) in
					Text(entry.extension())
						.foregroundStyle(Color.primary)
						.opacity(entry.isLocallyPresent() ? 1.0 : EntryView.remoteFileOpacity)
				}
				.width(min: 32, max: 64)
				.defaultVisibility(.hidden)
				.customizationID("extension")

				// File size
				TableColumn(
					"Size",
					sortUsing: EntryComparator(order: .forward, sortBy: .size)
				) { entry in
					if !entry.isDirectory() {
						Text(Self.formatter.string(fromByteCount: entry.size()))
							.foregroundStyle(Color.primary)
							.opacity(entry.isLocallyPresent() ? 1.0 : EntryView.remoteFileOpacity)
					}
				}
				.width(min: 100, max: 120)
				.defaultVisibility(.hidden)
				.alignment(.trailing)
				.customizationID("size")

				// Last modified date
				TableColumn(
					"Last modified",
					sortUsing: EntryComparator(order: .forward, sortBy: .lastModifiedDate)
				) { (entry: SushitrainEntry) in
					if let md = entry.modifiedAt()?.date(), !entry.isSymlink() {
						Text(md.formatted(date: .numeric, time: .shortened))
							.foregroundStyle(Color.primary)
							.opacity(entry.isLocallyPresent() ? 1.0 : EntryView.remoteFileOpacity)
					}
				}
				.width(min: 150, max: 180)
				.defaultVisibility(.hidden)
				.alignment(.leading)
				.customizationID("lastModifiedDate")

			},
			rows: {
				ForEach(entries) { entry in
					TableRow(entry).draggable(entry)
				}
			}
		)
		.task {
			await self.update()
		}
		.onChange(of: self.sortOrder, initial: false) { _, _ in
			rememberTableState()
			Task {
				await self.update()
			}
		}
		.onChange(of: self.files, initial: false) { _, _ in
			Task {
				await self.update()
			}
		}
		.onChange(of: self.subdirectories, initial: false) { _, _ in
			Task {
				await self.update()
			}
		}
		.contextMenu(
			forSelectionType: SushitrainEntry.ID.self,
			menu: self.itemsMenu,
			primaryAction: self.doubleClick
		)
		.navigationDestination(isPresented: Binding.isNotNil($openedEntry)) {
			self.nextView()
		}
		.sheet(item: $showDownloaderFor) { downloadEntry in
			self.downloaderSheet(entry: downloadEntry)
		}
		.sheet(item: $showVersionsFor) { entry in
			FileVersionsView(folder: self.folder, path: entry.path())
		}
		.scrollPosition(id: $scrollItem)
		.onAppear {
			if let saved = browserSession?.tables[.folder(folderID: folder.folderID, prefix: prefix)] {
				selection = saved.selection
				sortOrder = saved.sort
				scrollItem = saved.scroll
			}
		}
		.onChange(of: selection) { rememberTableState() }
		.onChange(of: scrollItem) { rememberTableState() }

		#if os(macOS)
			.sheet(item: $desktopEdit) { DesktopEntryMutationSheet(edit: $0) }
			.confirmationDialog(
				"Delete this item from all devices?", isPresented: Binding.isNotNil($deletingEntry), titleVisibility: .visible,
				presenting: deletingEntry
			) { target in
				Button("Delete from All Devices", role: .destructive) {
					Task {
						do { try await deleteDesktopEntry(target) }
						catch { operationError = ErrorMessage(error) }
					}
				}
			} message: { _ in
				Text(
					"This deletes the item on this device and synchronizes its deletion. If this is the last copy, it cannot be recovered."
				)
			}
			.errorAlert($operationError)
		#endif

	}

	private func rememberTableState() {
		guard browserSession?.active == true else { return }
		browserSession?.tables[.folder(folderID: folder.folderID, prefix: prefix)] = DesktopBrowserTableState(
			selection: selection, sort: sortOrder, scroll: scrollItem)
	}

	@ViewBuilder private func itemsMenu(items: Set<SushitrainEntry.ID>) -> some View {
		if items.isEmpty {
			EmptyView()
		}
		else if let item = items.first, items.count == 1 {
			// Single item selected
			if let oe = self.entryById(item) {
				#if os(macOS)
					DesktopSyncFileMenu(
						entry: oe, info: { openedEntry = (oe, false) }, versions: { showVersionsFor = oe },
						delete: { deletingEntry = DesktopEntryEdit(entry: oe, kind: .delete) }, edit: { desktopEdit = $0 },
						extra: {
							Button("Copy", systemImage: "document.on.document") { copy(oe) }.disabled(!oe.isLocallyPresent())
							EntryShareButton(
								entry: oe,
								showDownloader: Binding(get: { showDownloaderFor != nil }, set: { showDownloaderFor = $0 ? oe : nil })
							)
							if oe.canShowInFinder {
								Button(openInFilesAppLabel, systemImage: "arrow.up.forward.app") {
									do { try oe.showInFinder() }
									catch { operationError = ErrorMessage(error) }
								}
							}
							if oe.hasExternalSharingURL {
								Divider()
								FileSharingLinksView(entry: oe, sync: true)
							}
						}
					)
				#else

					if oe.isDirectory() {
						Button("Subdirectory properties", systemImage: "folder.badge.gearshape") {
							self.openedEntry = (oe, false)
						}.labelStyle(.titleAndIcon)
					}
					else {
						Button("Show info", systemImage: "info.circle") {
							self.openedEntry = (oe, false)
						}.labelStyle(.titleAndIcon)
					}

					// Preview and copy (only for files, not folders)
					if !oe.isDirectory() && !oe.isSymlink() {
						Button("Show preview", systemImage: "doc.text.magnifyingglass") {
							openWindow(
								id: "preview",
								value: Preview(folderID: self.folder.folderID, path: oe.path())
							)
						}.disabled(!oe.canPreview)

						// Copy
						Button("Copy", systemImage: "document.on.document") {
							self.copy(oe)
						}
						.disabled(!oe.isLocallyPresent())
						.labelStyle(.titleAndIcon)
					}

					// Regular sharing
					EntryShareButton(
						entry: oe,
						showDownloader: Binding(
							get: {
								self.showDownloaderFor != nil
							},
							set: { nv in
								if nv {
									self.showDownloaderFor = oe
								}
								else {
									self.showDownloaderFor = nil
								}
							})
					).labelStyle(.titleAndIcon)

					// Show item in Finder
					if oe.canShowInFinder {
						Button(openInFilesAppLabel, systemImage: "arrow.up.forward.app") {
							try? oe.showInFinder()
						}
						.labelStyle(.titleAndIcon)
					}

					if oe.canShowVersions {
						Button("Previous Versions...", systemImage: "clock.arrow.circlepath") {
							self.showVersionsFor = oe
						}
						.labelStyle(.titleAndIcon)
					}

					// External sharing
					if oe.hasExternalSharingURL {
						Divider()
						FileSharingLinksView(entry: oe, sync: true)
							.labelStyle(.titleAndIcon)
					}
				#endif
			}
		}
		else {
			// Multiple items selected
			Text("\(items.count) items selected")
		}
	}

	@ViewBuilder private func downloaderSheet(entry: SushitrainEntry) -> some View {
		NavigationStack {
			EntryDownloaderView(file: entry, action: .share)
				.toolbar {
					SheetButton(role: .cancel) {
						showDownloaderFor = nil
					}
				}
		}
	}

	@ViewBuilder private func nextView() -> some View {
		if let (oe, honorTapToPreview) = openedEntry {
			if oe.isSymlink() {
				if !honorTapToPreview {
					// Just show symlink properties
					EntryInfoView(entry: oe, showPath: false, siblings: self.entries)
				}
				else if let targetEntry = try? oe.symlinkTargetEntry() {
					// Symlink to a directory
					if targetEntry.isDirectory() {
						if let targetFolder = targetEntry.folder {
							BrowserView(folder: targetFolder, prefix: targetEntry.path() + "/", userSettings: appState.userSettings)
						}
					}
					else {
						// Symlink to file
						if honorTapToPreview && appState.userSettings.tapFileToPreview {
							FileViewerView(
								file: targetEntry,
								siblings: entries,
								inSheet: false,
								isShown: .constant(true)
							)
							.navigationTitle(targetEntry.fileName())
						}
						else {
							EntryInfoView(entry: targetEntry, showPath: false, siblings: self.entries)
						}
					}
				}
				else {
					// Symlink to URL, case is handled elsewhere
					EmptyView()
				}
			}
			else if oe.isDirectory() {
				if honorTapToPreview {
					BrowserView(folder: folder, prefix: oe.path() + "/", userSettings: appState.userSettings)
				}
				else {
					EntryInfoView(entry: oe, showPath: false, siblings: self.entries)
				}
			}
			else {
				if honorTapToPreview && appState.userSettings.tapFileToPreview {
					FileViewerView(
						file: oe, siblings: entries,
						inSheet: false,
						isShown: .constant(true)
					)
					.navigationTitle(oe.fileName())
				}
				else {
					EntryInfoView(entry: oe, showPath: false, siblings: self.entries)
				}
			}
		}
		else {
			EmptyView()
		}
	}

	private func doubleClick(_ items: Set<SushitrainEntry.ID>) {
		if let item = items.first, items.count == 1 {
			if let oe = self.entryById(item) {
				// Symlink to URLs should be opened externally
				if oe.isSymlink() {
					if let targetURL = oe.symlinkTargetURL {
						openURL(targetURL)
						return
					}
				}

				// Tap to preview opens a new window
				if appState.userSettings.tapFileToPreview {
					if oe.canPreview {
						openWindow(
							id: "preview",
							value: Preview(
								folderID: self.folder.folderID,
								path: oe.path()
							))
						return
					}
				}

				// Directory routes participate in the retained value path.
				if oe.isDirectory(), let pushBrowserRoute {
					pushBrowserRoute(.folder(folderID: oe.folder?.folderID ?? folder.folderID, prefix: oe.path() + "/"))
				}
				else {
					self.openedEntry = (oe, true)
				}
			}
		}
	}

	private func update() async {
		updateGeneration += 1
		let generation = updateGeneration
		let values = subdirectories + files
		let ordering = sortOrder
		let sorted = await Task.detached {
			var values = values
			values.sort(using: ordering)
			return values
		}.value
		guard !Task.isCancelled, updateGeneration == generation else { return }
		entries = sorted
	}
}

private struct EntryNameView: View {
	let entry: SushitrainEntry
	let viewStyle: BrowserViewStyle

	@Environment(AppState.self) private var appState

	private var showThumbnail: Bool {
		return self.viewStyle == .thumbnailList
	}

	var body: some View {
		if self.showThumbnail && !self.entry.isDirectory() {
			// Thubmnail view shows thumbnail image next to the file name
			HStack(alignment: .center, spacing: 9.0) {
				ThumbnailView(file: entry, showFileName: false, showErrorMessages: false)
					.frame(width: 60, height: 40)
					.cornerRadius(6.0)
					.help(entry.fileName())

				// The entry name (grey when not locally present)
				Text(entry.fileName())
					.multilineTextAlignment(.leading)
					.foregroundStyle(entry.isConflictCopy() ? Color.red : Color.primary)
					.opacity(entry.isLocallyPresent() ? 1.0 : EntryView.remoteFileOpacity)
				Spacer()
			}
			.frame(maxWidth: .infinity)
			.padding(0)
		}
		else {
			HStack {
				Image(systemName: entry.systemImage)
					.foregroundStyle(entry.color ?? Color.accentColor)
				Text(entry.fileName())
					.multilineTextAlignment(.leading)
					.foregroundStyle(entry.isConflictCopy() ? Color.red : Color.primary)
					.opacity(entry.isLocallyPresent() ? 1.0 : EntryView.remoteFileOpacity)
				Spacer()
			}
			.frame(maxWidth: .infinity)
		}
	}
}
