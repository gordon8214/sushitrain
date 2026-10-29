// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit

	/// Lists a directory of a folder accessed through a gateway. Syncthing on this device leaves such a folder alone (it
	/// is paused, so it cannot list it), so the items are listed from the index the Files app shows, which the app keeps
	/// up to date from the gateway. Files open in the Files app, which downloads them from the gateway.
	struct RemoteFolderBrowserView: View {
		@Environment(AppState.self) private var appState
		let folder: SushitrainFolder
		/// The directory shown, ending in a slash ("" for the folder root)
		let prefix: String
		private var coordinator = RemoteCoordinator.shared

		@State private var contents: Contents? = nil
		@State private var loadError: String? = nil
		@State private var openError: String? = nil
		@State private var showSettings = false

		/// Children read from the index at a time
		private nonisolated static let pageSize = 500

		init(folder: SushitrainFolder, prefix: String) {
			self.folder = folder
			self.prefix = prefix
		}

		private enum Contents {
			case listed(directories: [IndexItem], files: [IndexItem])
			/// The directory is not in the index: not listed from the gateway yet, or removed
			case missing
		}

		private struct Request: Hashable {
			let folderID: String
			let path: String
			let sort: BrowserViewSort
			let hideDotFiles: Bool
			/// Changes whenever the index does
			let indexRevision: Int
		}

		private var request: Request {
			Request(
				folderID: folder.folderID, path: prefix.withoutEndingSlash,
				sort: appState.userSettings.defaultBrowserSort.value ?? BrowserViewSort(),
				hideDotFiles: appState.userSettings.dotFilesHidden,
				indexRevision: FileProviderCoordinator.shared.indexRevision)
		}

		private var folderName: String {
			prefix.isEmpty ? folder.label() : (prefix.withoutEndingSlash.split(separator: "/").last.map(String.init) ?? prefix)
		}

		private var gateway: RemoteGatewayStatus? {
			coordinator.binding(folderID: folder.folderID)?.serviceID.flatMap { coordinator.status(serviceID: $0) }
		}

		var body: some View {
			List {
				self.gatewaySection()

				if case .listed(let directories, let files) = contents {
					Section {
						ForEach(directories, id: \.id) { directory in
							NavigationLink(
								destination: BrowserView(
									folder: folder, prefix: directory.path + "/", userSettings: appState.userSettings)
							) {
								HStack(spacing: 9.0) {
									Image(systemName: "folder.fill").foregroundStyle(Color.accentColor)
									Text(directory.name).multilineTextAlignment(.leading).foregroundStyle(Color.primary)
									Spacer()
								}.frame(maxWidth: .infinity)
							}
						}
					}

					Section {
						ForEach(files, id: \.id) { file in
							Button {
								self.openInFiles(path: file.path, isDirectory: false)
							} label: {
								RemoteFileRow(file: file)
							}
						}
					}

					FilesFooterView(subdirectories: directories.count, files: files.count)
						.listRowBackground(Color(.systemGroupedBackground))
				}
			}
			.overlay {
				self.overlayView()
			}
			.navigationTitle(folderName)
			.navigationBarTitleDisplayMode(.inline)
			.toolbar {
				ToolbarItemGroup(placement: .primaryAction) {
					Menu {
						Button(openInFilesAppLabel, systemImage: "arrow.up.forward.app") {
							self.openInFiles(path: prefix.withoutEndingSlash, isDirectory: true)
						}

						Button("Folder settings...", systemImage: "folder.badge.gearshape") {
							showSettings = true
						}
					} label: {
						Label("Actions", systemImage: "ellipsis")
					}
				}
			}
			.task(id: request) {
				await self.reload(request)
			}
			.refreshable {
				await coordinator.refresh()
				await self.reload(request)
			}
			.sheet(isPresented: $showSettings) {
				NavigationStack {
					FolderView(folder: self.folder)
						.toolbar {
							SheetButton(role: .save) {
								showSettings = false
							}
						}
				}
			}
			.alert("An error occurred", isPresented: Binding.isNotNil($openError)) {
				Button("OK") {}
			} message: {
				Text(openError ?? "")
			}
		}

		/// The gateway serving the folder (at the folder root), and what keeps it from being used
		@ViewBuilder private func gatewaySection() -> some View {
			let gateway = self.gateway
			let problem = gateway.flatMap { $0.problem ?? $0.folderProblems[folder.folderID] }
			if let gateway, prefix.isEmpty || problem != nil {
				Section {
					LabeledContent("Accessed through") {
						Text(gateway.displayName)
					}
					if let problem {
						Text(problem).foregroundStyle(.red)
					}
				} footer: {
					if prefix.isEmpty {
						Text(
							"Folders opened through a gateway are not synchronized on this device: the Files app downloads files from the gateway when they are opened, and saves edits through it."
						)
					}
				}
			}
		}

		@ViewBuilder private func overlayView() -> some View {
			if let loadError {
				ContentUnavailableView {
					Label("Unable to load files", systemImage: "exclamationmark.triangle")
				} description: {
					Text(loadError)
				} actions: {
					Button("Retry") {
						Task { await self.reload(request) }
					}
				}
			}
			else {
				switch contents {
				case nil:
					ProgressView()
				case .missing where prefix.isEmpty:
					ContentUnavailableView("Listing the folder on the gateway", systemImage: "clock.arrow.circlepath")
				case .missing:
					ContentUnavailableView(
						"Folder removed", systemImage: "trash", description: Text("This folder was removed."))
				case .listed(let directories, let files) where directories.isEmpty && files.isEmpty:
					ContentUnavailableView("There are currently no files in this folder.", systemImage: "questionmark.folder")
				case .listed:
					EmptyView()
				}
			}
		}

		@MainActor private func reload(_ request: Request) async {
			do {
				let contents = try await Task.detached(priority: .userInitiated) {
					try Self.list(request)
				}.value
				// A newer request may have started meanwhile
				guard !Task.isCancelled else { return }
				self.contents = contents
				self.loadError = nil
			}
			catch {
				guard !Task.isCancelled else { return }
				Log.warn("Could not list \(request.folderID):\(request.path) from the index: \(error.localizedDescription)")
				self.loadError = error.localizedDescription
			}
		}

		private nonisolated static func list(_ request: Request) throws(IndexError) -> Contents {
			guard let container = FileProviderStorage.container else {
				throw .unavailable("The app group container is not available")
			}
			let store = IndexStore(container: container)
			guard let directory = try store.item(folderID: request.folderID, path: request.path),
				directory.kind.isContainer
			else {
				return .missing
			}

			var children: [IndexItem] = []
			var after: Int64 = 0
			while true {
				let page = try store.children(of: directory.id, after: after, limit: pageSize)
				children += page
				guard page.count == pageSize, let last = page.last else {
					break
				}
				after = last.id
			}
			if request.hideDotFiles {
				children.removeAll { $0.name.starts(with: ".") }
			}

			let directories = children.filter(\.kind.isContainer)
				.sorted { $0.name.compare($1.name, options: .numeric) == .orderedAscending }
			let files = children.filter { !$0.kind.isContainer }.sorted(by: request.sort.isOrderedBefore)
			return .listed(directories: directories, files: files)
		}

		/// Shows an item in the Files app, which opens files (downloading them from the gateway)
		private func openInFiles(path: String, isDirectory: Bool) {
			let folderID = folder.folderID
			Task {
				if let url = await FileProviderStorage.userVisibleURL(folderID: folderID, path: path) {
					openURLInSystemFilesApp(url: url)
				}
				else if isDirectory {
					openError = String(localized: "This folder is not shown in the Files app. Is the Synctrain location turned on?")
				}
				else {
					openError = String(localized: "This file is not shown in the Files app. Is the Synctrain location turned on?")
				}
			}
		}
	}

	private struct RemoteFileRow: View {
		let file: IndexItem

		var body: some View {
			HStack(spacing: 9.0) {
				Image(systemName: "document").foregroundStyle(Color.accentColor)
				VStack(alignment: .leading, spacing: 2.0) {
					Text(file.name).multilineTextAlignment(.leading).foregroundStyle(Color.primary)
					HStack {
						Text(file.size, format: .byteCount(style: .file))
						if let date = file.modificationDate {
							Text(date, format: .dateTime.year().month().day().hour().minute())
						}
					}
					.font(.caption)
					.foregroundStyle(Color.secondary)
				}
				Spacer()
			}.frame(maxWidth: .infinity)
		}
	}

	extension BrowserViewSort {
		func isOrderedBefore(_ lhs: IndexItem, _ rhs: IndexItem) -> Bool {
			let r: Bool
			switch self.attribute {
			case .name:
				r = lhs.name.compare(rhs.name, options: .numeric) == .orderedAscending
			case .size:
				r = lhs.size < rhs.size
			case .dateModified:
				r = lhs.modifiedNanoseconds < rhs.modifiedNanoseconds
			}
			return ascending ? r : !r
		}
	}
#endif
