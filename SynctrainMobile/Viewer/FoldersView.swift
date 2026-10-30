// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// The folders chosen from the paired gateways: the app's main screen
struct FoldersView: View {
	private var coordinator = RemoteCoordinator.shared
	@State private var path: [ViewedDirectory] = []

	var body: some View {
		NavigationStack(path: $path) {
			List {
				ForEach(coordinator.folders, id: \.folderID) { folder in
					NavigationLink(
						value: ViewedDirectory(folderID: folder.folderID, itemID: folder.rootItemID, title: folder.label)
					) {
						row(folder)
					}
				}
				NavigationLink {
					ChooseFoldersView()
				} label: {
					Label("Choose Folders…", systemImage: "plus")
				}
			}
			.navigationTitle("Folders")
			.navigationDestination(for: ViewedDirectory.self) { directory in
				DirectoryView(directory: directory)
			}
			.refreshable {
				await coordinator.refresh()
			}
		}
	}

	@ViewBuilder private func row(_ folder: IndexFolder) -> some View {
		let binding = coordinator.binding(folderID: folder.folderID)
		let status = binding.flatMap { coordinator.status(serviceID: $0.serviceID) }
		VStack(alignment: .leading, spacing: 2) {
			Label(folder.label, systemImage: "folder.fill")
			if let status {
				Text(status.displayName).font(.caption).foregroundStyle(.secondary)
			}
		}
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
