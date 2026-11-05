// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import PhotosUI
import UniformTypeIdentifiers
import SynctrainHostKit
@preconcurrency import SushitrainCore

/// Hosts folder actions outside the menu so sheets and confirmations survive menu dismissal.
struct FolderActionsView<Content: View>: View {
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

	@Environment(AppState.self) private var appState
	@Environment(\.showToast) private var showToast
	let folder: SushitrainFolder
	var prefix: String = ""
	@ObservedObject var userSettings: AppUserSettings
	var enablesRescanShortcut = false
	var onChange: () -> Void = {}
	@ViewBuilder var content: (FolderMenuContents, Binding<Bool>) -> Content

	@State private var showSettings = false
	@State private var folderExists = false
	@State private var showAddFilePicker = false
	@State private var showPhotoPicker = false
	@State private var selectedMedia: [PhotosPickerItem] = []
	@State private var isImportingMedia = false
	@State private var errorMessage: String?
	@State private var showConfirmable: ConfirmableAction?
	@State private var showNewBookmarkInfo = false
	@State private var showIgnores = false
	@State private var showDeletedFiles = false

	private var route: Route {
		Route.folder(folderID: folder.folderID, prefix: prefix)
	}

	private var isBookmarked: Bool {
		userSettings.bookmarkedRoutes.contains { Route(url: $0) == route }
	}

	var body: some View {
		content(
			FolderMenuContents(
				folder: folder, prefix: prefix, folderExists: folderExists,
				isImportingMedia: isImportingMedia,
				isBookmarked: Binding(get: { isBookmarked }, set: setBookmarked),
				enablesRescanShortcut: enablesRescanShortcut,
				addPhotos: { showPhotoPicker = true }, addFiles: { showAddFilePicker = true },
				showDeletedFiles: { showDeletedFiles = true }, showSettings: { showSettings = true },
				showIgnores: { showIgnores = true }, rescan: rescanFolder,
				unlink: { showConfirmable = .unlinkFolder }, remove: { showConfirmable = .removeFolder }
			),
			$showSettings
		)
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
				IgnoresView(folder: folder)
					.navigationTitle("Files to ignore")
					.presentationSizing(.form)
					.frame(minWidth: 640, minHeight: 480)
			}
		}

		.alert(
			"This location has been added to your bookmarks. Bookmarked locations can be accessed quickly from the start page.",
			isPresented: $showNewBookmarkInfo
		) {
			Button("OK") {}
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
		.alert("An error occurred", isPresented: Binding.isNotNil($errorMessage)) {
			Button("OK") { errorMessage = nil }
		} message: {
			Text(errorMessage ?? "")
		}
		.confirmationDialog(
			showConfirmable?.message ?? "", isPresented: Binding.isNotNil($showConfirmable), titleVisibility: .visible
		) {
			if let sc = showConfirmable {
				Button(sc.buttonTitle, role: .destructive, action: self.confirmedAction)
			}
		}
		.task(id: appState.eventCounter) {
			folderExists = folder.exists()
		}
		.onChange(of: showSettings) { _, isPresented in
			if !isPresented { update() }
		}
	}

	private func update() {
		folderExists = folder.exists()
		onChange()
	}

	private func dropFilesInBackground(_ urls: [URL]) async throws -> Int {
		let folder = folder
		let prefix = prefix
		return try await Task.detached(priority: .utility) {
			try FileImport.copy(urls, folder: folder, prefix: prefix)
		}.value
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
					self.errorMessage = error.localizedDescription
				}
			}

		case .failure(_):
			break
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
			if !errors.isEmpty { errorMessage = errors.joined(separator: "\n") }
		}
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
			self.errorMessage = error.localizedDescription
		}
		self.showConfirmable = .none
	}

	private func rescanFolder() {
		showToast(Toast(title: "Folder re-scan started", image: "sparkle.magnifyingglass"))

		do {
			try folder.rescan()
		}
		catch let error {
			self.errorMessage = error.localizedDescription
		}
	}
}

/// The gear menu and sidebar context menu use this same content.
struct FolderMenuContents: View {
	let folder: SushitrainFolder
	let prefix: String
	let folderExists: Bool
	let isImportingMedia: Bool
	@Binding var isBookmarked: Bool
	let enablesRescanShortcut: Bool
	let addPhotos: () -> Void
	let addFiles: () -> Void
	let showDeletedFiles: () -> Void
	let showSettings: () -> Void
	let showIgnores: () -> Void
	let rescan: () -> Void
	let unlink: () -> Void
	let remove: () -> Void

	var body: some View {
		let isReceiveEncrypted = folder.isReceiveEncryptedFolder
		if folderExists {
			FolderImportMenuItems(
				folder: folder, folderExists: folderExists, isImportingMedia: isImportingMedia, addPhotos: addPhotos,
				addFiles: addFiles)

			if !self.prefix.isEmpty && !isReceiveEncrypted {
				if let entry = try? self.folder.getFileInformation(self.prefix.withoutEndingSlash) {
					NavigationLink(destination: EntryInfoView(entry: entry, showPath: false, siblings: nil)) {
						Label("Subdirectory properties...", systemImage: "folder.badge.gearshape")
					}
				}
			}

			if !isReceiveEncrypted {
				Toggle(isOn: $isBookmarked) {
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
					showDeletedFiles()
				}
			}
		}

		Button("Folder settings...", systemImage: "folder.badge.gearshape") {
			showSettings()
		}

		if folder.isRegularFolder {
			Button("Files to ignore...", systemImage: "rectangle.dashed") {
				showIgnores()
			}
		}

		FolderOperationsMenu(
			folder: folder, enablesRescanShortcut: enablesRescanShortcut, rescan: rescan, unlink: unlink, remove: remove)
	}
}

private struct FolderImportMenuItems: View {
	let folder: SushitrainFolder
	let folderExists: Bool
	let isImportingMedia: Bool
	let addPhotos: () -> Void
	let addFiles: () -> Void

	var body: some View {
		Button("Add from photo library...", systemImage: "photo.on.rectangle") {
			addPhotos()
		}.disabled(
			isImportingMedia || !folderExists || !self.folder.isRegularFolder || self.folder.isReceiveOnlyFolder)

		Button("Add files...", systemImage: "plus") {
			addFiles()
		}.disabled(
			!folderExists || !self.folder.isRegularFolder || self.folder.isReceiveOnlyFolder)
	}

}

private struct FolderOperationsMenu: View {
	let folder: SushitrainFolder
	let enablesRescanShortcut: Bool
	let rescan: () -> Void
	let unlink: () -> Void
	let remove: () -> Void

	var body: some View {
		Menu {
			Button("Re-scan folder", systemImage: "sparkle.magnifyingglass", action: rescan)
				.buttonStyle(.link)
				.keyboardShortcut(enablesRescanShortcut ? KeyboardShortcut("R", modifiers: [.command, .shift]) : nil)

			Divider()

			Button("Unlink folder", systemImage: "folder.badge.minus", role: .destructive) {
				unlink()
			}
			.buttonStyle(.link)
			.foregroundStyle(.red)

			// Only allow removing a full folder when we are sure it is in the area managed by us
			if folder.isRegularFolder && folder.canRemoveFiles() {
				Button("Remove folder", systemImage: "trash", role: .destructive) {
					remove()
				}
				.buttonStyle(.link)
				.foregroundStyle(.red)
			}
		} label: {
			Label("Folder actions", systemImage: "ellipsis")
		}.labelStyle(.titleAndIcon)
	}

}

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
