// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// A directory to show: a folder's root, or a directory in it
struct ViewedDirectory: Hashable {
	var folderID: String
	var itemID: Int64
	var title: String
}

/// How the items of a directory are ordered
enum ViewerSort: String, CaseIterable, Identifiable {
	case name
	case date
	case size

	var id: Self { self }

	var label: LocalizedStringKey {
		switch self {
		case .name: return "Name"
		case .date: return "Date"
		case .size: return "Size"
		}
	}

	func areInOrder(_ first: IndexItem, _ second: IndexItem) -> Bool {
		if first.kind.isContainer != second.kind.isContainer {
			return first.kind.isContainer
		}
		switch self {
		case .date where first.modifiedNanoseconds != second.modifiedNanoseconds:
			return first.modifiedNanoseconds > second.modifiedNanoseconds
		case .size where first.size != second.size:
			return first.size > second.size
		default:
			return first.name.localizedStandardCompare(second.name) == .orderedAscending
		}
	}
}

/// Lists a directory from the index, which also works offline. It follows changes as the index gets them.
struct DirectoryView: View {
	let directory: ViewedDirectory
	private let navigation: MobileBrowserNavigation?
	private let selectHistory: Bool
	private var files = FilesDomain.shared
	private var coordinator = RemoteCoordinator.shared
	@AppStorage("viewerSort") private var sort = ViewerSort.name
	@AppStorage("hideDotFiles") private var hideDotFiles = true
	private struct HistorySelection: Identifiable { let id: Int64 }
	@State private var historyItem: HistorySelection?
	@State private var state: MobileDirectoryState
	@State private var viewer = FileViewer()
	@State private var filesError: String? = nil

	init(
		directory: ViewedDirectory, navigation: MobileBrowserNavigation? = nil, selectHistory: Bool = false
	) {
		self.directory = directory
		self.navigation = navigation
		self.selectHistory = selectHistory
		_state = State(initialValue: navigation?.directory(directory.itemID) ?? MobileDirectoryState())
	}

	private struct LoadKey: Hashable {
		var revision: Int
		var folders: Int
		var listingRevision: Int
	}

	private var shown: [IndexItem] {
		let visible = (state.items ?? []).filter { !hideDotFiles || !$0.name.hasPrefix(".") }
		return visible.sorted(by: sort.areInOrder)
	}

	var body: some View {
		List {
			FolderStatusBanner(folderID: directory.folderID)
			if let loadError = state.error {
				Text(loadError).foregroundStyle(.red)
			}
			ForEach(shown, id: \.id) { item in
				row(item).id(item.id)
			}
		}
		.scrollPosition(id: $state.scrollItem)
		.overlay {
			if state.items == nil, state.error == nil {
				ProgressView()
			}
			else if shown.isEmpty, state.error == nil {
				ContentUnavailableView("No files", systemImage: "folder")
			}
		}
		.navigationTitle(directory.title)
		.toolbar {
			ToolbarItem(placement: .primaryAction) {
				Menu("Options", systemImage: "ellipsis") {
					Picker("Sort by", selection: $sort) {
						ForEach(ViewerSort.allCases) { Text($0.label).tag($0) }
					}
					Toggle("Hide Files Starting with a Dot", isOn: $hideDotFiles)
					Button("Show in Files", systemImage: "folder") {
						showInFiles(itemID: directory.itemID)
					}
				}
			}
		}
		.sheet(item: $historyItem) { item in GatewayHistoryView(itemID: item.id) { historyItem = nil } }
		.modifier(FileViewerPresentation(viewer: viewer))
		.alert("Could not show this in Files", isPresented: Binding.isNotNil($filesError)) {
			Button("OK") {}
		} message: {
			Text(filesError ?? "")
		}
		.refreshable {
			await coordinator.refresh()
		}
		.task(
			id: LoadKey(
				revision: files.indexRevision, folders: coordinator.bindings.count,
				listingRevision: navigation?.listingRevision ?? 0)
		) {
			await load()
		}
	}

	@ViewBuilder private func row(_ item: IndexItem) -> some View {
		Group {
			if item.kind.isContainer {
				NavigationLink(value: ViewedDirectory(folderID: item.folderID, itemID: item.id, title: item.name)) {
					ItemRow(item: item)
				}
			}
			else {
				Button {
					if selectHistory {
						historyItem = HistorySelection(id: item.id)
					}
					else {
						viewer.open(item)
					}
				} label: {
					ItemRow(item: item)
				}
				.buttonStyle(.plain)
			}
		}
		.contextMenu {
			if item.kind == .file {
				Button("Previous Versions…", systemImage: "clock.arrow.circlepath") { historyItem = HistorySelection(id: item.id) }
			}
			Button("Show in Files", systemImage: "folder") {
				showInFiles(itemID: item.id)
			}
		}
	}

	private func load() async {
		guard let store = RemoteEnvironment.shared?.store else {
			return
		}
		let parentID = directory.itemID
		let result = await Task.detached(priority: .userInitiated) { () -> Result<[IndexItem], IndexError> in
			do throws(IndexError) {
				var items: [IndexItem] = []
				while true {
					let page = try store.children(of: parentID, after: items.last?.id ?? 0, limit: 500)
					items += page
					if page.count < 500 {
						return .success(items)
					}
				}
			}
			catch {
				return .failure(error)
			}
		}.value
		guard !Task.isCancelled else { return }
		switch result {
		case .success(let loaded):
			state.items = loaded
			navigation?.retainListing(for: directory.itemID)
			state.error = nil
		case .failure(let error):
			state.error = error.userDescription
		}
	}

	private func showInFiles(itemID: Int64) {
		Task {
			let item = try? RemoteEnvironment.shared?.store.item(id: itemID)
			if let item, let url = await files.userVisibleURL(folderID: item.folderID, path: item.path) {
				openInFilesApp(url: url)
			}
			else {
				filesError = String(localized: "Is the Synctrain location turned on in the Files app?")
			}
		}
	}
}
