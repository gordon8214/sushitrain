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
	private var files = FilesDomain.shared
	private var coordinator = RemoteCoordinator.shared
	@AppStorage("viewerSort") private var sort = ViewerSort.name
	@AppStorage("hideDotFiles") private var hideDotFiles = true
	@State private var items: [IndexItem]? = nil
	@State private var loadError: String? = nil
	@State private var viewer = FileViewer()
	@State private var filesError: String? = nil

	init(directory: ViewedDirectory) {
		self.directory = directory
	}

	private struct LoadKey: Hashable {
		var revision: Int
		var folders: Int
	}

	private var shown: [IndexItem] {
		let visible = (items ?? []).filter { !hideDotFiles || !$0.name.hasPrefix(".") }
		return visible.sorted(by: sort.areInOrder)
	}

	var body: some View {
		List {
			FolderStatusBanner(folderID: directory.folderID)
			if let loadError {
				Text(loadError).foregroundStyle(.red)
			}
			ForEach(shown, id: \.id) { item in
				row(item)
			}
		}
		.overlay {
			if items == nil, loadError == nil {
				ProgressView()
			}
			else if shown.isEmpty, loadError == nil {
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
		.modifier(FileViewerPresentation(viewer: viewer))
		.alert("Could not show this in Files", isPresented: Binding.isNotNil($filesError)) {
			Button("OK") {}
		} message: {
			Text(filesError ?? "")
		}
		.refreshable {
			await coordinator.refresh()
		}
		.task(id: LoadKey(revision: files.indexRevision, folders: coordinator.bindings.count)) {
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
					viewer.open(item)
				} label: {
					ItemRow(item: item)
				}
				.buttonStyle(.plain)
			}
		}
		.contextMenu {
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
		switch result {
		case .success(let loaded):
			items = loaded
			loadError = nil
		case .failure(let error):
			loadError = error.userDescription
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
