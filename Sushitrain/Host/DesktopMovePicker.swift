// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// Enumerates directory metadata only; moving does not open file contents.
	struct DesktopMovePicker: View {
		let item: IndexItem
		let environment: RemoteEnvironment?
		let move: (Int64) -> Void
		@Environment(\.dismiss) private var dismiss
		@State private var parent: Int64?
		@State private var ancestors: [Int64?] = []
		@State private var directories: [IndexItem] = []
		@State private var problem: String?
		@State private var loading = false
		var body: some View {
			NavigationStack {
				List {
					if let problem { Text(problem).foregroundStyle(.red) }
					ForEach(directories, id: \.id) { directory in
						Button {
							ancestors.append(parent)
							parent = directory.id
						} label: {
							Label(directory.name.isEmpty ? directory.folderID : directory.name, systemImage: "folder")
						}
						.disabled(directory.id == item.id)
					}
				}
				.disabled(loading)
				.overlay { if loading { ProgressView() } }
				.navigationTitle("Move \(item.name)")
				.safeAreaInset(edge: .bottom) {
					Text("Choose a folder on this gateway. Finder also supports moving files between gateways.")
						.font(.callout).foregroundStyle(.secondary).padding()
				}
				.toolbar {
					ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
					ToolbarItem {
						Button("Up", systemImage: "arrow.up") { parent = ancestors.removeLast() }.disabled(ancestors.isEmpty)
					}
					ToolbarItem(placement: .confirmationAction) {
						Button("Move") { if let parent { move(parent) } }.disabled(parent == nil)
					}
				}
				.task(id: parent) { await load() }
			}.frame(minWidth: 460, minHeight: 360)
		}
		private func load() async {
			guard let store = environment?.store else {
				problem = "The shared index is unavailable."
				return
			}
			let current = parent
			let folderID = item.folderID
			loading = true
			directories = []
			let task = Task.detached { () -> Result<[IndexItem], Error> in
				Result {
					if let current {
						var rows: [IndexItem] = []
						var after: Int64 = 0
						while true {
							try Task.checkCancellation()
							let page = try store.children(of: current, after: after, limit: 500)
							rows += page.filter { $0.kind.isContainer }
							if page.count < 500 { return rows }
							after = page.last!.id
						}
					}
					let service = try store.remoteBinding(folderID: folderID)?.serviceID
					return try store.remoteBindings().filter { $0.serviceID == service }.compactMap { binding in
						guard let folder = try store.folder(id: binding.folderID) else { return nil }
						var root = try store.item(id: folder.rootItemID)
						root?.name = folder.label
						return root
					}
				}
			}
			let result = await withTaskCancellationHandler {
				await task.value
			} onCancel: {
				task.cancel()
			}
			guard !Task.isCancelled, parent == current else { return }
			loading = false
			switch result {
			case .success(let rows):
				directories = rows.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
				problem = nil
			case .failure(let error): problem = error.localizedDescription
			}
		}
	}
#endif
