// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// What is going on in a folder, and what to do when it needs the user: the items that failed, and bringing a folder
	/// that only goes one way back in line
	struct FolderStatusSection: View {
		private enum Action: Identifiable {
			case revert, deleteUnexpected, override
			var id: Self { self }
		}

		@Environment(AppState.self) private var appState
		let folder: SushitrainFolder

		@State private var failedItems = 0
		@State private var localChanges = 0
		@State private var outOfSync = 0
		@State private var showFailedItems = false
		@State private var confirming: Action? = nil
		@State private var isWorking = false
		@State private var errorMessage: ErrorMessage? = nil

		private var isReceiveEncrypted: Bool { folder.folderType() == SushitrainFolderTypeReceiveEncrypted }

		var body: some View {
			Section("Status") {
				// What goes with this section is on this row, which is always there: a section passes modifiers on to its rows
				FolderStatusView(folder: folder)
					.task(id: appState.eventCounter) {
						await self.update()
					}
					.sheet(isPresented: $showFailedItems) {
						NavigationStack {
							FolderFailedItemsView(folder: folder)
								.toolbar {
									SheetButton(role: .done) {
										showFailedItems = false
									}
								}
						}
					}
					.confirmationDialog(
						self.question, isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } }),
						titleVisibility: .visible, presenting: confirming
					) { action in
						Button(self.confirmTitle(action), role: .destructive) {
							self.perform(action)
						}
					} message: { action in
						Text(self.explanation(action))
					}
					.errorAlert($errorMessage)

				if failedItems > 0 {
					LabeledContent {
						Button("Show...") {
							showFailedItems = true
						}
					} label: {
						Label("Failed items: \(failedItems)", systemImage: "exclamationmark.triangle.fill")
							.foregroundStyle(.red)
					}
				}

				if folder.isReceiveOnlyFolder && localChanges > 0 {
					LabeledContent {
						Button(isReceiveEncrypted ? "Delete Unexpected Items..." : "Revert Local Changes...") {
							confirming = isReceiveEncrypted ? .deleteUnexpected : .revert
						}
						.disabled(isWorking)
					} label: {
						Text(isReceiveEncrypted ? "Unexpected items: \(localChanges)" : "Locally changed items: \(localChanges)")
					}
				}

				if folder.folderType() == SushitrainFolderTypeSendOnly && outOfSync > 0 {
					LabeledContent {
						Button("Override Changes...") {
							confirming = .override
						}
						.disabled(isWorking)
					} label: {
						Text("Out of sync items: \(outOfSync)")
					}
				}
			}
		}

		private var question: String {
			switch confirming {
			case .override: return String(localized: "Override changes on other devices?")
			case .deleteUnexpected: return String(localized: "Delete unexpected items?")
			default: return String(localized: "Revert local changes?")
			}
		}

		private func confirmTitle(_ action: Action) -> String {
			switch action {
			case .revert: return String(localized: "Revert Local Changes")
			case .deleteUnexpected: return String(localized: "Delete Unexpected Items")
			case .override: return String(localized: "Override Changes")
			}
		}

		private func explanation(_ action: Action) -> String {
			switch action {
			case .revert:
				return String(
					localized:
						"What was added or changed in this folder on this device is removed, and the versions other devices have come back. If the folder keeps versions, regular files can be found there; anything else, such as links, can't be brought back."
				)
			case .deleteUnexpected:
				return String(
					localized:
						"What doesn't belong in this receive-encrypted folder is deleted. This can't be undone."
				)
			case .override:
				return String(
					localized:
						"What is in this folder on this device becomes what every device has. Their changes are overwritten, and whatever this device doesn't have is deleted on all of them: if this folder is empty, that is everything. This can't be undone."
				)
			}
		}

		@concurrent private func update() async {
			let failedItems = folder.failedItemCount()
			let localChanges = folder.isReceiveOnlyFolder ? folder.localChangedItems() : 0
			let outOfSync = folder.folderType() == SushitrainFolderTypeSendOnly ? folder.outOfSyncItems() : 0
			guard !Task.isCancelled else { return }
			await MainActor.run {
				self.failedItems = failedItems
				self.localChanges = localChanges
				self.outOfSync = outOfSync
			}
		}

		private func perform(_ action: Action) {
			isWorking = true
			Task {
				do {
					// Waits for what the folder is busy with
					try await goBlocking { [folder] in
						switch action {
						case .revert, .deleteUnexpected: try folder.revert()
						case .override: try folder.override()
						}
					}
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isWorking = false
				await self.update()
			}
		}
	}

	/// What a folder could not scan or synchronize, and why
	struct FolderFailedItemsView: View {
		@Environment(AppState.self) private var appState
		@Environment(\.openWindow) private var openWindow
		let folder: SushitrainFolder

		@State private var items: [SushitrainFailedItem] = []

		var body: some View {
			Group {
				if items.isEmpty {
					ContentUnavailableView(
						"No failed items", systemImage: "checkmark.circle",
						description: Text("Everything in this folder could be scanned and synchronized.")
					)
				}
				else {
					Form {
						Section {
							ForEach(items, id: \.path) { item in
								self.row(item)
							}
						} footer: {
							Text("The engine tries these again by itself, at the next scan or when something changes.")
						}
					}
					.formStyle(.grouped)
				}
			}
			.frame(minWidth: 520, minHeight: 320)
			.navigationTitle("Failed items")
			.task(id: appState.eventCounter) {
				await self.update()
			}
		}

		private func row(_ item: SushitrainFailedItem) -> some View {
			VStack(alignment: .leading, spacing: 4) {
				Text(verbatim: item.path)
					.textSelection(.enabled)
				Text(verbatim: item.error)
					.font(.caption)
					.foregroundStyle(.red)
					.textSelection(.enabled)
				HStack {
					if folder.isRegularFolder {
						Button(openInFilesAppLabel) {
							self.reveal(item)
						}
					}
					Button("Show in file browser") {
						self.showInBrowser(item)
					}
				}
				.buttonStyle(.link)
			}
		}

		/// As near as possible: what failed may never have reached this device
		private func reveal(_ item: SushitrainFailedItem) {
			guard var url = folder.localNativeURL?.appending(path: item.path) else {
				return
			}
			let fileManager = FileManager.default
			while !fileManager.fileExists(atPath: url.path(percentEncoded: false)) && url.pathComponents.count > 1 {
				url = url.deletingLastPathComponent()
			}
			openURLInSystemFilesApp(url: url)
		}

		private func showInBrowser(_ item: SushitrainFailedItem) {
			let directory = (item.path as NSString).deletingLastPathComponent
			let prefix = directory.isEmpty ? "" : directory + "/"
			openWindow(id: "bookmark", value: Route.folder(folderID: folder.folderID, prefix: prefix).url)
		}

		@concurrent private func update() async {
			let list = folder.failedItems()
			let items = (0..<(list?.count() ?? 0)).compactMap { list?.item(at: $0) }
			guard !Task.isCancelled else { return }
			await MainActor.run {
				self.items = items
			}
		}
	}
#endif
