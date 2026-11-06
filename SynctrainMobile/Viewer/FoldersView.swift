// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// The folders chosen from the paired gateways: the app's main screen
struct FoldersView: View {
	@State private var navigation: MobileBrowserNavigation
	var isPresentedModally = false
	var selectHistory = false
	init(
		navigation: MobileBrowserNavigation = MobileBrowserNavigation(), isPresentedModally: Bool = false,
		selectHistory: Bool = false
	) {
		_navigation = State(initialValue: navigation)
		self.isPresentedModally = isPresentedModally
		self.selectHistory = selectHistory
	}
	var body: some View {
		NavigationStack(path: $navigation.path) {
			MobileFolderList(navigation: navigation, isPresentedModally: isPresentedModally, selectHistory: selectHistory)
		}
	}
}

struct MobileFolderList: View {
	@Bindable var navigation: MobileBrowserNavigation
	var isPresentedModally = false
	var selectHistory = false
	@Environment(\.dismiss) private var dismiss
	private var coordinator = RemoteCoordinator.shared
	@State private var showSearch = false
	var body: some View {
		List {
			ForEach(coordinator.folders, id: \.folderID) { folder in
				NavigationLink(value: ViewedDirectory(folderID: folder.folderID, itemID: folder.rootItemID, title: folder.label)) {
					Label(folder.label, systemImage: "folder.fill")
				}.id(folder.folderID)
			}
			NavigationLink {
				ChooseFoldersView()
			} label: {
				Label("Choose Folders…", systemImage: "plus")
			}
		}
		.scrollPosition(id: $navigation.folderScrollID)
		.navigationTitle("Browse Files")
		.sheet(isPresented: $showSearch) {
			SearchView(navigation: navigation, isPresentedModally: true, selectHistory: selectHistory)
		}
		.toolbar {
			ToolbarItem(placement: .primaryAction) {
				Button("Search Files", systemImage: "magnifyingglass") { showSearch = true }
			}
			if isPresentedModally { ToolbarItem(placement: .cancellationAction) { Button("Done") { dismiss() } } }
		}
		.navigationDestination(for: ViewedDirectory.self) { directory in
			DirectoryView(directory: directory, navigation: navigation, selectHistory: selectHistory)
		}
		.refreshable { await coordinator.refresh() }
	}
}

/// What keeps a folder from being up to date, and the edits still waiting for its gateway
struct FolderStatusBanner: View {
	let folderID: String
	private var coordinator = RemoteCoordinator.shared

	init(folderID: String) {
		self.folderID = folderID
	}

	private var problem: String? {
		guard let binding = coordinator.binding(folderID: folderID),
			let status = coordinator.status(serviceID: binding.serviceID)
		else {
			return nil
		}
		return status.problem ?? status.folderProblems[folderID]
	}

	var body: some View {
		let operations = coordinator.operations(folderID: folderID)
		if problem != nil || !operations.isEmpty {
			Section {
				if let problem {
					Label(problem, systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
				}
				if !operations.isEmpty {
					NavigationLink {
						Form {
							RemoteOperationsSection(folderID: folderID)
						}
						.navigationTitle("Edits")
					} label: {
						Label("Edits waiting for the gateway: \(operations.count)", systemImage: "arrow.up.circle")
					}
				}
			}
		}
	}
}
