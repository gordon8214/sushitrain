// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppKit
	@preconcurrency import SushitrainCore
	import SynctrainFileProviderKit
	import SwiftUI

	struct FileVersionsView: View {
		@Environment(\.dismiss) private var dismiss
		let folder: SushitrainFolder
		let path: String
		@State private var model: HistoryBrowserModel?
		@State private var problem: String?

		static func removeOpenedVersions() {
			HistoryBrowserModel.removeLeftovers()
			try? FileManager.default.removeItem(at: URL.temporaryDirectory.appending(path: "Opened versions"))
		}

		var body: some View {
			Group {
				if let model {
					HistoryBrowserView(model: model) { dismiss() }
				}
				else if let problem {
					ContentUnavailableView(
						"Versions Are Not Available", systemImage: "exclamationmark.triangle", description: Text(problem))
				}
				else {
					ProgressView()
				}
			}.task {
				guard model == nil else { return }
				do { model = try makeModel() }
				catch { problem = error.localizedDescription }
			}
		}

		private func makeModel() throws -> HistoryBrowserModel {
			guard let environment = RemoteEnvironment.shared else { throw HistoryError.sourceChanged }
			let folder = folder
			let path = path
			let histories = try environment.store.historyEndpoints().map { endpoint in
				RemoteHistory(
					store: environment.store, source: HistorySource(endpoint: endpoint, folderID: folder.folderID, path: path),
					client: environment.client(for: endpoint))
			}
			let local = LocalHistoryProvider(
				deviceID: folder.clientDeviceID(),
				load: {
					guard folder.canBrowseVersions() else { return [] }
					return try await goBlocking(returning: {
						let list = try folder.fileVersions(path)
						return (0..<list.count()).compactMap { index -> RemoteVersion? in
							guard let version = list.item(at: index), let archived = version.versionTime()?.date(),
								let modified = version.modTime()?.date()
							else { return nil }
							return RemoteVersion(
								id: "ver_\(Int64(archived.timeIntervalSince1970))", versionTime: archived,
								modifiedNs: Int64(modified.timeIntervalSince1970 * 1_000_000_000), size: version.size)
						}
					})
				},
				export: { selected, destination in
					try await goBlocking {
						let list = try folder.fileVersions(path)
						guard
							let version = (0..<list.count()).compactMap({ list.item(at: $0) }).first(where: {
								Int64($0.versionTime()?.date()?.timeIntervalSince1970 ?? 0) == Int64(selected.versionTime.timeIntervalSince1970)
							})
						else { throw HistoryError.invalidDownload }
						try folder.exportFileVersion(path, version: version, destination: destination.path(percentEncoded: false))
					}
				})
			let explanation =
				folder.versioningType().isEmpty
				? String(
					localized:
						"Restore replaces this file and synchronizes normally. Destination versioning is off, so the current contents will not be archived."
				)
				: String(
					localized:
						"Restore replaces this file, archives its current contents using this folder's versioning, and synchronizes normally. In a receive-only folder it counts as a local change."
				)
			let destination = HistoryDestination(
				name: String(localized: "This Mac"), canRestore: folder.canRestoreDownloadedVersion(), explanation: explanation,
				validateExport: { url in try folder.validateVersionExportDestination(url.path(percentEncoded: false)) },
				restoreInPlace: { row in
					guard row.sourceID == "local" else { return false }
					// Keep native restoration of platform metadata for archives on this Mac.
					try await goBlocking {
						let list = try folder.fileVersions(path)
						guard
							let version = (0..<list.count()).compactMap({ list.item(at: $0) }).first(where: {
								Int64($0.versionTime()?.date()?.timeIntervalSince1970 ?? 0)
									== Int64(row.version.versionTime.timeIntervalSince1970)
							})
						else { throw HistoryError.invalidDownload }
						try folder.restoreFileVersion(path, version: version)
					}
					return true
				},
				prepare: {
					let captured = try await goBlocking(returning: {
						var error: NSError?
						let token = folder.captureRestoreTarget(path, error: &error)
						if let error { throw error }
						return token
					})
					return { file, version in
						try await goBlocking {
							try folder.restoreDownloadedVersion(
								path, source: file.path(percentEncoded: false), expected: captured, modifiedNs: version.modifiedNs)
						}
					}
				})
			return HistoryBrowserModel(
				fileName: (path as NSString).lastPathComponent, store: environment.store, histories: histories, local: local,
				destination: destination)
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
