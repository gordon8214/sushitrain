// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	/// The folders paired devices can be given access to
	struct GatewayExportsSection: View {
		private var controller = GatewayController.shared

		/// Exports of folders that are no longer on this Mac
		private var orphanedExports: [String] {
			let folderIDs = Set(controller.folders.map(\.id))
			return controller.exports.keys.filter { !folderIDs.contains($0) }.sorted()
		}

		var body: some View {
			Section {
				if controller.folders.isEmpty && orphanedExports.isEmpty {
					Text("There are no folders that can be exported.")
						.foregroundStyle(.secondary)
				}
				ForEach(controller.folders) { folder in
					GatewayExportRow(folder: folder, export: controller.exports[folder.id])
				}
				ForEach(orphanedExports, id: \.self) { folderID in
					GatewayExportRow(
						folder: GatewayFolder(
							id: folderID, name: folderID,
							writeRestriction: String(localized: "This folder is no longer on this Mac.")),
						export: controller.exports[folderID])
				}
			} header: {
				Text("Exported folders")
			} footer: {
				Text(
					"Paired devices can use exported folders they are given access to. Changes can be allowed in send-receive folders that are kept on this Mac in full."
				)
			}
			.disabled(controller.state != .running)
		}
	}

	private struct GatewayExportRow: View {
		private var controller = GatewayController.shared
		let folder: GatewayFolder
		let export: GatewayExport?
		@State private var errorMessage: String? = nil

		init(folder: GatewayFolder, export: GatewayExport?) {
			self.folder = folder
			self.export = export
		}

		/// Finder reaches this folder through its export, which it needs to stay as it is
		private var isInFinder: Bool {
			FinderAccess.shared.localFolderID(folder.id) != nil
		}

		var body: some View {
			VStack(alignment: .leading) {
				Toggle(
					isOn: Binding(
						get: { export != nil },
						set: { exported in update(exported: exported, allowWrite: false) })
				) {
					Text(folder.name)
				}
				.disabled(isInFinder)

				if let export {
					Toggle(
						"Allow changes",
						isOn: Binding(
							get: { export.allowWrite },
							set: { allowWrite in update(exported: true, allowWrite: allowWrite) })
					)
					// Changes can always be turned off, even when the folder no longer allows them
					.disabled(isInFinder || !export.allowWrite && folder.writeRestriction != nil)
					.padding(.leading)

					if let restriction = folder.writeRestriction {
						Text(restriction)
							.font(.caption)
							.foregroundStyle(.secondary)
							.padding(.leading)
					}
				}
			}
			.alert(
				"Could not change the export", isPresented: Binding.isNotNil($errorMessage),
				actions: {
					Button("OK") {}
				},
				message: {
					Text(errorMessage ?? "")
				})
		}

		private func update(exported: Bool, allowWrite: Bool) {
			let folderID = folder.id
			Task {
				do {
					try await controller.setExport(folderID: folderID, exported: exported, allowWrite: allowWrite)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}
#endif
