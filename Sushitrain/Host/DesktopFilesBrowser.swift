// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import QuickLook
	@preconcurrency import FileProvider
	import SynctrainFileProviderKit
	import SynctrainHostKit

	/// Reads directory metadata from the shared index. Content is requested only by a file action.
	struct DesktopFilesBrowser: View {
		let folder: IndexFolder
		let parentID: Int64
		let title: String
		@Environment(DesktopFilesNavigation.self) private var navigation
		private var state: DesktopFilesBrowserState { navigation.browser(parentID: parentID) }
		@State private var preview: URL?
		@State private var history: FileSelection?
		@State private var information: FileSelection?
		@State private var deleting: IndexItem?
		@State private var renaming: IndexItem?
		@State private var moving: MoveSelection?
		@State private var creatingFolder = false
		@State private var folderName = ""
		@State private var newName = ""
		@State private var working = false
		@FocusState private var listHasFocus: Bool
		@AppStorage("hideDotFiles") private var hideDotFiles = true
		private struct FileSelection: Identifiable { let id: Int64 }
		private struct MoveSelection: Identifiable {
			let item: IndexItem
			var id: Int64 { item.id }
		}
		private var shown: [IndexItem] {
			state.items.filter {
				(!hideDotFiles || !$0.name.hasPrefix("."))
					&& (state.filter.isEmpty || $0.name.localizedStandardContains(state.filter))
			}
			.sorted { first, second in
				if first.kind.isContainer != second.kind.isContainer { return first.kind.isContainer }
				return first.name.localizedStandardCompare(second.name) == .orderedAscending
			}
		}

		var body: some View {
			@Bindable var state = state
			List(selection: $state.selection) {
				if let problem = state.problem { Text(problem).foregroundStyle(.red) }
				ForEach(shown, id: \.id) { item in
					DesktopFileRow(item: item, folder: folder)
						.tag(item.id)
						.contextMenu { fileActions(item) }
				}
				if state.hasMore { Button("Load More") { Task { await load(append: true) } } }
			}
			.focused($listHasFocus)
			.onAppear { if state.selection != nil { listHasFocus = true } }
			.scrollPosition(id: $state.scrollItem)
			.overlay { if state.loading { ProgressView() } }
			.navigationTitle(title)
			.searchable(text: $state.filter, prompt: "Filter this folder")
			.toolbar {
				Menu("Add", systemImage: "plus") {
					Button("New Folder…") {
						folderName = ""
						creatingFolder = true
					}
					Button("Import File…") { importFile() }
				}
				if let item = state.items.first(where: { $0.id == state.selection }) {
					Menu("Actions", systemImage: "ellipsis") { fileActions(item) }
				}
			}
			.disabled(working)
			.quickLookPreview($preview)
			.sheet(item: $moving, onDismiss: { listHasFocus = true }) { selection in
				let item = selection.item
				DesktopMovePicker(item: item, environment: navigation.environment) { parent in
					mutate(item, delete: false, toParent: parent, name: item.name)
					moving = nil
				}
			}
			.alert("New Folder", isPresented: $creatingFolder) {
				TextField("Folder name", text: $folderName)
				Button("Cancel", role: .cancel) {}
				Button("Create") { create(name: folderName) }.disabled(!FileNames.isValid(folderName))
			}
			.sheet(item: $history, onDismiss: { listHasFocus = true }) { selected in
				GatewayHistoryView(itemID: selected.id) { history = nil }
			}
			.sheet(item: $information, onDismiss: { listHasFocus = true }) { selected in
				if let item = state.items.first(where: { $0.id == selected.id }) {
					Form {
						LabeledContent("Name", value: item.name)
						LabeledContent("Path", value: item.path)
						LabeledContent("Size", value: item.size.formatted(.byteCount(style: .file)))
						if let date = item.modificationDate { LabeledContent("Modified", value: date.formatted()) }
						Button("Done") { information = nil }
					}.padding().frame(minWidth: 400)
				}
			}
			.confirmationDialog(
				"Delete this item?", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } })
			) {
				Button("Delete", role: .destructive) {
					if let item = deleting { mutate(item, delete: true) }
					deleting = nil
				}
			} message: {
				Text("Deletion is sent to the gateway and may synchronize to other devices.")
			}
			.alert("Rename", isPresented: Binding(get: { renaming != nil }, set: { if !$0 { renaming = nil } })) {
				TextField("Name", text: $newName)
				Button("Cancel", role: .cancel) { renaming = nil }
				Button("Rename") {
					if let item = renaming { mutate(item, delete: false) }
					renaming = nil
				}
			}
			.task(id: FilesDomain.shared.indexRevision) {
				if state.loadedRevision != FilesDomain.shared.indexRevision { await load() }
			}
			.refreshable {
				await RemoteCoordinator.shared.refresh()
				await load()
			}
		}

		@ViewBuilder private func fileActions(_ item: IndexItem) -> some View {
			ForEach(DesktopUICatalog.desktop.detailDescriptors["fileActions"] ?? []) { action in
				switch action.id {
				case "open":
					if item.kind.isContainer {
						NavigationLink(
							"Open", value: DesktopFilesDirectory(folderID: folder.folderID, parentID: item.id, title: item.name))
					}
					else {
						Button("Open") { open(item, previewing: false) }
					}
				case "preview": if !item.kind.isContainer { Button("Preview") { open(item, previewing: true) } }
				case "offline":
					Button("Download Now") { availability(item, download: true) }
					Button("Free Up Space") { availability(item, download: false) }.disabled(item.pending != .none)
				case "renameMove":
					Button("Rename…") {
						renaming = item
						newName = item.name
					}
					Button("Move…") { moving = MoveSelection(item: item) }
				case "previousVersions":
					if !item.kind.isContainer { Button("Previous Versions") { history = FileSelection(id: item.id) } }
				case "information": Button("Information") { information = FileSelection(id: item.id) }
				case "delete": Button("Delete…", role: .destructive) { deleting = item }
				default: EmptyView()
				}
			}
		}

		private func load(append: Bool = false) async {
			guard let store = navigation.environment?.store else {
				state.problem = "The shared index is unavailable."
				state.loading = false
				return
			}
			if append && state.loading { return }
			state.generation += 1
			let generation = state.generation
			state.loading = true
			defer { if state.generation == generation { state.loading = false } }
			let revision = FilesDomain.shared.indexRevision
			let count = append ? 500 : max(500, state.items.count)
			let parent = parentID
			let after = append ? state.lastID : 0
			let result = await Task.detached(priority: .userInitiated) {
				Result { try store.children(of: parent, after: after, limit: count + 1) }
			}.value
			guard !Task.isCancelled, generation == state.generation else { return }
			switch result {
			case .success(let result):
				let page = Array(result.prefix(count))
				state.loadedRevision = revision
				let combined = append ? state.items + page : page
				var seen = Set<Int64>()
				state.items = combined.filter { seen.insert($0.id).inserted }
				state.hasMore = result.count > count
				state.lastID = page.last?.id ?? after
				state.problem = nil
			case .failure(let error): state.problem = error.localizedDescription
			}
			state.loading = false
		}

		private func open(_ item: IndexItem, previewing: Bool) {
			Task {
				if let url = await FilesDomain.shared.userVisibleURL(folderID: item.folderID, path: item.path) {
					if previewing {
						preview = url
					}
					else {
						NSWorkspace.shared.open(url)
					}
				}
				else {
					state.problem = "The Finder location is unavailable."
				}
			}
		}

		private func availability(_ item: IndexItem, download: Bool) {
			working = true
			Task {
				defer { working = false }
				do {
					guard let manager = NSFileProviderManager(for: FilesDomain.domain) else { throw CocoaError(.fileNoSuchFile) }
					let identifier = NSFileProviderItemIdentifier(ItemIdentifiers.string(for: item.id))
					if download {
						try await manager.requestDownloadForItem(withIdentifier: identifier)
					}
					else {
						guard let store = navigation.environment?.store, let current = try store.item(id: item.id),
							current.pending == .none, try store.remoteOverlay(itemID: item.id) == nil
						else { throw CocoaError(.fileWriteUnknown) }
						try await manager.evictItem(identifier: identifier)
					}
				}
				catch { state.problem = error.localizedDescription }
			}
		}

		private func importFile() {
			let panel = NSOpenPanel()
			panel.canChooseDirectories = false
			panel.allowsMultipleSelection = false
			panel.begin { response in
				if response == .OK, let url = panel.url { create(name: url.lastPathComponent, source: url) }
			}
		}

		private func create(name: String, source: URL? = nil) {
			guard let environment = navigation.environment else { return }
			guard FileNames.isValid(name) else {
				state.problem = "Enter a valid file name."
				return
			}
			let parent = parentID
			let folderID = folder.folderID
			working = true
			Task {
				defer { working = false }
				do {
					let result = try await Task.detached { () throws -> RemoteMutationResult in
						let staged = try source.map { try RemoteUploader.stage($0, in: environment.storage) }
						do {
							return try environment.store.recordRemoteCreation(
								folderID: folderID, parentItemID: parent, name: name,
								kind: source == nil ? .directory : .file,
								modifiedNanoseconds: Int64(Date().timeIntervalSince1970 * 1_000_000_000),
								stagedFile: staged, origin: .provider)
						}
						catch {
							if let staged { environment.removeStagedFiles([staged.relativePath]) }
							throw error
						}
					}.value
					environment.removeStagedFiles(result.releasedStagedFiles)
					FilesDomain.shared.signalWorkingSet()
					RemoteCoordinator.shared.pokeOutbox()
					await RemoteCoordinator.shared.reloadState()
					await load()
				}
				catch { state.problem = error.localizedDescription }
			}
		}

		private func mutate(_ item: IndexItem, delete: Bool, toParent: Int64? = nil, name replacement: String? = nil) {
			guard let environment = navigation.environment else { return }
			let name = replacement ?? newName
			working = true
			Task {
				defer { working = false }
				do {
					let result = try await Task.detached { () throws -> RemoteMutationResult in
						if delete {
							return try environment.store.recordRemoteDeletion(
								itemID: item.id, expectedMetadataRevision: item.metadataRevision)
						}
						guard FileNames.isValid(name), let parent = toParent ?? item.parentID else {
							throw CocoaError(.fileWriteInvalidFileName)
						}
						return try environment.store.recordRemoteMove(
							itemID: item.id, toParentItemID: parent, name: name, expectedMetadataRevision: item.metadataRevision)
					}.value
					environment.removeStagedFiles(result.releasedStagedFiles)
					FilesDomain.shared.signalWorkingSet()
					RemoteCoordinator.shared.pokeOutbox()
					await RemoteCoordinator.shared.reloadState()
					await load()
				}
				catch { state.problem = error.localizedDescription }
			}
		}
	}

	private struct DesktopFileRow: View {
		let item: IndexItem
		let folder: IndexFolder
		var body: some View {
			HStack {
				if item.kind.isContainer {
					NavigationLink(value: DesktopFilesDirectory(folderID: folder.folderID, parentID: item.id, title: item.name)) {
						Label(item.name, systemImage: "folder")
					}
				}
				else {
					Label(item.name, systemImage: "doc")
				}
				Spacer()
				if item.pending != .none { Text("Pending").foregroundStyle(.secondary) }
				if !item.kind.isContainer { Text(item.size, format: .byteCount(style: .file)).foregroundStyle(.secondary) }
			}
		}
	}
#endif
