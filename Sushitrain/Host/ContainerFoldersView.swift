// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SushitrainCore
	import SwiftUI

	/// The folders still in the sandboxed app's container. The view that shows them loads them (`update`), as a section
	/// with nothing in it never appears, nor does its work start.
	@MainActor @Observable final class ContainerFolders {
		private(set) var folders: [SushitrainFolder] = []

		func update(_ appState: AppState) async {
			guard !FolderAccess.usesSecurityScope else {
				return
			}
			self.folders = await appState.folders().filter { ContainerMigrator.isInContainer($0) }.sorted {
				$0.displayName.localizedStandardCompare($1.displayName) == .orderedAscending
			}
		}
	}

	/// Offers to move the folders still in the sandboxed app's container to a folder the user chooses. They keep working
	/// where they are, so this is never forced. Each is renamed into a new directory there (on the same disk), with its
	/// synchronization paused meanwhile.
	struct ContainerFoldersSection: View {
		@Environment(AppState.self) private var appState
		let model: ContainerFolders
		@State private var showChooser = false
		@State private var isMoving = false
		@State private var errorMessage: String? = nil

		var body: some View {
			if !model.folders.isEmpty {
				Section {
					ForEach(model.folders, id: \.folderID) { folder in
						LabeledContent(folder.displayName) {
							Text((folder.path() as NSString).abbreviatingWithTildeInPath)
								.truncationMode(.middle)
								.lineLimit(1)
						}
					}
					Button("Move to another folder...") {
						showChooser = true
					}
					.buttonStyle(.link)
					.disabled(isMoving)
					.fileImporter(isPresented: $showChooser, allowedContentTypes: [.folder]) { result in
						if case .success(let base) = result {
							Task {
								await self.move(into: base)
							}
						}
					}
					// Rather than in the container, where the open panel was last
					.fileDialogDefaultDirectory(FileManager.default.homeDirectoryForCurrentUser)
					.alert(
						"Some folders could not be moved", isPresented: Binding.isNotNil($errorMessage),
						actions: {
							Button("OK") {
								errorMessage = nil
							}
						},
						message: {
							Text(errorMessage ?? "")
						})
				} header: {
					Text("Folders in the old app container")
				} footer: {
					Text(
						"These folders are still where the app kept them inside the App Sandbox. They keep working there. Moving them puts each in a new folder inside the folder you choose, on the same disk."
					)
				}
			}
		}

		private func move(into base: URL) async {
			if ContainerMigrator.isInContainer(base) {
				errorMessage = String(localized: "Choose a folder outside the old app container.")
				return
			}
			isMoving = true
			var failures: [String] = []
			for folder in model.folders {
				let name = folder.displayName
				let failure: String? = await Task.detached {
					var error: NSError? = nil
					let destination = folder.relocate(into: base.path(percentEncoded: false), error: &error)
					if let error {
						return "\(name): \(error.localizedDescription)"
					}
					Log.info("Moved folder \(name) out of the app container to \(destination)")
					return nil
				}.value
				if let failure {
					failures.append(failure)
				}
			}
			isMoving = false
			if !failures.isEmpty {
				errorMessage = failures.joined(separator: "\n")
			}
			await model.update(appState)
		}
	}

	/// ContainerFoldersSection in a sheet, offered once after the app moved out of its container
	struct ContainerFoldersSheet: View {
		@Environment(AppState.self) private var appState
		@Binding var isPresented: Bool
		@State private var model = ContainerFolders()

		var body: some View {
			Form {
				ContainerFoldersSection(model: model)
			}
			.formStyle(.grouped)
			.navigationTitle("Move folders out of the app container")
			.task {
				await model.update(appState)
			}
			.toolbar {
				ToolbarItem(placement: .confirmationAction) {
					Button("Done") {
						isPresented = false
					}
				}
			}
			.frame(minWidth: 480, minHeight: 300)
		}
	}
#endif
