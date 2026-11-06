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
	@Environment(\.dismiss) private var dismiss
	private let isPresentedModally: Bool
	@State private var navigation: MobileBrowserNavigation
	@State private var viewer = FileViewer()
	private struct HistorySelection: Identifiable { let id: Int64 }
	@State private var historyItem: HistorySelection?
	private let selectHistory: Bool
	init(
		navigation: MobileBrowserNavigation = MobileBrowserNavigation(), isPresentedModally: Bool = false,
		selectHistory: Bool = false
	) {
		_navigation = State(initialValue: navigation)
		self.isPresentedModally = isPresentedModally
		self.selectHistory = selectHistory
	}
	private var text: String { navigation.query }
	private var results: [IndexItem] {
		get { navigation.results }
		nonmutating set { navigation.results = newValue }
	}
	private var searched: String {
		get { navigation.searched }
		nonmutating set { navigation.searched = newValue }
	}

	/// More results than this are left out: the search should be narrowed
	private static let limit = 200

	var body: some View {
		NavigationStack(path: $navigation.searchPath) {
			List {
				ForEach(results, id: \.id) { item in
					row(item).id(item.id)
				}
			}
			.scrollPosition(id: $navigation.searchScrollID)
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
			.navigationTitle("Search Files")
			.toolbar {
				if isPresentedModally { ToolbarItem(placement: .cancellationAction) { Button("Done") { dismiss() } } }
			}
			.navigationDestination(for: ViewedDirectory.self) { directory in
				DirectoryView(directory: directory, navigation: navigation, selectHistory: selectHistory)
			}
			.searchable(text: $navigation.query)
			.modifier(FileViewerPresentation(viewer: viewer))
			.sheet(item: $historyItem) { item in GatewayHistoryView(itemID: item.id) { historyItem = nil } }
			.task(id: text) {
				await search()
			}
		}
		.onDisappear { viewer.cancel() }
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
				if selectHistory {
					historyItem = HistorySelection(id: item.id)
				}
				else {
					viewer.open(item)
				}
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
