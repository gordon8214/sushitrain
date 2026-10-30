// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// Finds files by name in the folders on this device
struct SearchView: View {
	private var coordinator = RemoteCoordinator.shared
	@State private var text = ""
	@State private var results: [IndexItem] = []
	@State private var searched = ""
	@State private var viewer = FileViewer()
	@State private var path: [ViewedDirectory] = []

	/// More results than this are left out: the search should be narrowed
	private static let limit = 200

	var body: some View {
		NavigationStack(path: $path) {
			List {
				ForEach(results, id: \.id) { item in
					row(item)
				}
			}
			.overlay {
				if text.isEmpty {
					ContentUnavailableView(
						"Search Your Folders", systemImage: "magnifyingglass",
						description: Text("Files and directories are found by name, in the folders on this device."))
				}
				else if results.isEmpty, searched == text {
					ContentUnavailableView.search(text: text)
				}
			}
			.navigationTitle("Search")
			.navigationDestination(for: ViewedDirectory.self) { directory in
				DirectoryView(directory: directory)
			}
			.searchable(text: $text)
			.modifier(FileViewerPresentation(viewer: viewer))
			.task(id: text) {
				await search()
			}
		}
	}

	@ViewBuilder private func row(_ item: IndexItem) -> some View {
		let detail = location(of: item)
		if item.kind.isContainer {
			NavigationLink(value: ViewedDirectory(folderID: item.folderID, itemID: item.id, title: item.name)) {
				ItemRow(item: item, detail: detail)
			}
		}
		else {
			Button {
				viewer.open(item)
			} label: {
				ItemRow(item: item, detail: detail)
			}
			.buttonStyle(.plain)
		}
	}

	/// The folder and directory an item is in
	private func location(of item: IndexItem) -> String {
		let label = coordinator.folders.first { $0.folderID == item.folderID }?.label ?? item.folderID
		let parent = FileNames.parentPath(item.path)
		return parent.isEmpty ? label : "\(label)/\(parent)"
	}

	private func search() async {
		let text = text
		guard !text.isEmpty else {
			results = []
			return
		}
		// Only what the user settled on takes the index's time
		try? await Task.sleep(for: .milliseconds(250))
		guard !Task.isCancelled, let store = RemoteEnvironment.shared?.store else {
			return
		}
		let terms = text.split(whereSeparator: \.isWhitespace).map(String.init)
		let limit = Self.limit
		let found = await Task.detached(priority: .userInitiated) {
			(try? store.searchItems(terms: terms, limit: limit)) ?? []
		}.value
		if !Task.isCancelled {
			results = found
			searched = text
		}
	}
}
