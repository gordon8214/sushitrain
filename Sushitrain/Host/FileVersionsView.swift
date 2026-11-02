// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	import SushitrainCore
	import SwiftUI

	/// The older versions of a file that its folder keeps, to open, save elsewhere or put back
	struct FileVersionsView: View {
		@Environment(\.dismiss) private var dismiss
		let folder: SushitrainFolder
		let path: String

		@State private var versions: [SushitrainFileVersion] = []
		@State private var isLoading = true
		@State private var loadError: String? = nil
		@State private var restoring: SushitrainFileVersion? = nil
		@State private var isWorking = false
		@State private var errorMessage: ErrorMessage? = nil

		private var fileName: String { (path as NSString).lastPathComponent }

		private static let openedVersionsDirectory = URL.temporaryDirectory.appending(
			path: "Opened versions", directoryHint: .isDirectory)

		/// To be called when the app starts, when nothing can have one of these open from this run
		static func removeOpenedVersions() {
			try? FileManager.default.removeItem(at: openedVersionsDirectory)
		}

		var body: some View {
			NavigationStack {
				Group {
					if isLoading {
						ProgressView()
					}
					else if let loadError {
						ContentUnavailableView(
							"Versions are not available", systemImage: "exclamationmark.triangle", description: Text(loadError))
					}
					else if versions.isEmpty {
						ContentUnavailableView(
							"No previous versions", systemImage: "clock.arrow.circlepath",
							description: Text("This folder has kept no older version of this file.")
						)
					}
					else {
						Form {
							Section {
								ForEach(Array(versions.enumerated()), id: \.offset) { _, version in
									self.row(version)
								}
							} header: {
								Text(verbatim: fileName)
							} footer: {
								Text(
									"Restoring puts a version back, and a file that is there now becomes a version in its place. Open and Save As copy only the contents."
								)
							}
						}
						.formStyle(.grouped)
						.disabled(isWorking)
					}
				}
				.frame(minWidth: 520, minHeight: 320)
				.navigationTitle("Previous Versions")
				.toolbar {
					SheetButton(role: .done) {
						dismiss()
					}
				}
			}
			.task {
				await self.load()
			}
			.confirmationDialog(
				"Restore this version?", isPresented: Binding(get: { restoring != nil }, set: { if !$0 { restoring = nil } }),
				titleVisibility: .visible, presenting: restoring
			) { version in
				Button("Restore") {
					self.restore(version)
				}
			} message: { _ in
				Text(self.restoreExplanation)
			}
			.errorAlert($errorMessage)
		}

		private var restoreExplanation: String {
			if folder.folderType() == SushitrainFolderTypeReceiveOnly {
				return String(
					localized:
						"A file that is there now becomes a version. In this receive-only folder the restored file counts as a local change, which Revert Local Changes would remove."
				)
			}
			return String(
				localized: "A file that is there now becomes a version, and other devices get the restored file. A link is removed."
			)
		}

		private func row(_ version: SushitrainFileVersion) -> some View {
			VStack(alignment: .leading, spacing: 4) {
				HStack {
					if let modified = version.modTime()?.date() {
						Text(modified.formatted(date: .abbreviated, time: .shortened))
					}
					Spacer()
					Text(ByteCountFormatter.string(fromByteCount: version.size, countStyle: .file))
						.foregroundStyle(.secondary)
				}
				if let archived = version.versionTime()?.date() {
					Text("Archived \(archived.formatted(date: .abbreviated, time: .shortened))")
						.font(.caption)
						.foregroundStyle(.secondary)
				}
				HStack {
					Button("Open") {
						self.open(version)
					}
					Button("Save As...") {
						self.saveAs(version)
					}
					Button("Restore...") {
						restoring = version
					}
				}
				.buttonStyle(.link)
			}
		}

		private func load() async {
			do {
				versions = try await goBlocking(returning: { [folder, path] in
					let list = try folder.fileVersions(path)
					return (0..<list.count()).compactMap { list.item(at: $0) }
				})
				loadError = nil
			}
			catch {
				loadError = error.localizedDescription
			}
			isLoading = false
		}

		private func export(_ version: SushitrainFileVersion, to destination: URL) async throws {
			let path = self.path
			let destinationPath = destination.path(percentEncoded: false)
			try await goBlocking { [folder] in
				try folder.exportFileVersion(path, version: version, destination: destinationPath)
			}
		}

		/// A copy in a directory of its own, so that it keeps the file's name and whichever app opens it can do so. They go
		/// together, for the next start of the app to clean up.
		private func open(_ version: SushitrainFileVersion) {
			Task {
				do {
					let directory = FileVersionsView.openedVersionsDirectory.appending(
						path: UUID().uuidString, directoryHint: .isDirectory)
					try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
					let copy = directory.appending(path: fileName)
					try await self.export(version, to: copy)
					NSWorkspace.shared.open(copy)
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
		}

		private func saveAs(_ version: SushitrainFileVersion) {
			let panel = NSSavePanel()
			panel.nameFieldStringValue = fileName
			panel.canCreateDirectories = true
			guard panel.runModal() == .OK, let destination = panel.url else {
				return
			}
			Task {
				do {
					try await self.export(version, to: destination)
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
			}
		}

		private func restore(_ version: SushitrainFileVersion) {
			let path = self.path
			isWorking = true
			Task {
				do {
					try await goBlocking { [folder] in try folder.restoreFileVersion(path, version: version) }
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isWorking = false
				await self.load()
			}
		}
	}

	/// The files a folder no longer has, of which it keeps versions, to put back
	struct DeletedFilesView: View {
		/// As read once: a list can be long, and each read of a field of an engine object is a call into Go
		private struct DeletedFile: Identifiable, Sendable {
			let path: String
			let versions: Int
			let size: Int64
			let latest: Date?
			var id: String { path }
		}

		@Environment(\.dismiss) private var dismiss
		let folder: SushitrainFolder

		@State private var files: [DeletedFile] = []
		@State private var selection = Set<String>()
		@State private var isLoading = true
		@State private var isRestoring = false
		@State private var confirmRestore = false
		@State private var loadError: String? = nil
		@State private var searchText = ""
		@State private var failures: [SushitrainFailedItem] = []
		@State private var errorMessage: ErrorMessage? = nil

		private var shownFiles: [DeletedFile] {
			searchText.isEmpty ? files : files.filter { $0.path.localizedCaseInsensitiveContains(searchText) }
		}

		/// What is selected and can be seen: a search can hide what was selected before it
		private var filesToRestore: [String] {
			shownFiles.map(\.path).filter { selection.contains($0) }
		}

		var body: some View {
			NavigationStack {
				Group {
					if isLoading {
						ProgressView()
					}
					else if let loadError {
						ContentUnavailableView(
							"Versions are not available", systemImage: "exclamationmark.triangle", description: Text(loadError))
					}
					else if files.isEmpty {
						ContentUnavailableView(
							"No deleted files", systemImage: "trash.slash",
							description: Text("This folder keeps no versions of files that are gone.")
						)
					}
					else {
						self.table()
					}
				}
				.frame(minWidth: 620, minHeight: 400)
				.navigationTitle("Deleted Files")
				.toolbar {
					ToolbarItem(placement: .cancellationAction) {
						Button("Close", role: .cancel) { dismiss() }
					}
					ToolbarItem(placement: .confirmationAction) {
						Button("Restore") {
							confirmRestore = true
						}
						.disabled(filesToRestore.isEmpty || isRestoring)
					}
				}
			}
			.task {
				await self.load()
			}
			.confirmationDialog(
				"Restore the selected files?", isPresented: $confirmRestore, titleVisibility: .visible
			) {
				Button("Restore") {
					self.restore()
				}
			} message: {
				Text(
					"Each file comes back as its newest version. That is what was kept when it last changed on another device, so changes made on this device since then aren't in it."
				)
			}
			.alert(
				"Some files were not restored",
				isPresented: Binding(get: { !failures.isEmpty }, set: { if !$0 { failures = [] } })
			) {
				Button("OK") {}
			} message: {
				Text(failures.map { "\($0.path): \($0.error)" }.joined(separator: "\n"))
			}
			.errorAlert($errorMessage)
		}

		private func table() -> some View {
			Table(shownFiles, selection: $selection) {
				TableColumn("File") { (file: DeletedFile) in
					Text(verbatim: file.path)
				}
				TableColumn("Versions") { (file: DeletedFile) in
					Text(file.versions, format: .number)
				}
				.width(ideal: 60)
				TableColumn("Size") { (file: DeletedFile) in
					Text(ByteCountFormatter.string(fromByteCount: file.size, countStyle: .file))
				}
				.width(ideal: 80)
				TableColumn("Last archived") { (file: DeletedFile) in
					if let latest = file.latest {
						Text(latest.formatted(date: .abbreviated, time: .shortened))
					}
				}
				.width(ideal: 140)
			}
			.searchable(text: $searchText)
			.disabled(isRestoring)
		}

		private func load() async {
			do {
				// This walks all of the folder's versions
				files = try await goBlocking(returning: { [folder] in
					let list = try folder.deletedFiles()
					return (0..<list.count()).compactMap { index in
						list.item(at: index).map {
							DeletedFile(path: $0.path, versions: $0.versions, size: $0.size, latest: $0.latest()?.date())
						}
					}
				})
				selection = selection.intersection(files.map(\.path))
				loadError = nil
			}
			catch {
				loadError = error.localizedDescription
			}
			isLoading = false
		}

		private func restore() {
			let paths = SushitrainListOfStrings.from(filesToRestore)
			isRestoring = true
			Task {
				do {
					failures = try await goBlocking(returning: { [folder] in
						let list = try folder.restoreLatestVersions(paths)
						return (0..<list.count()).compactMap { list.item(at: $0) }
					})
				}
				catch {
					errorMessage = ErrorMessage(error)
				}
				isRestoring = false
				await self.load()
			}
		}
	}
#endif
