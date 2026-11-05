// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	struct DesktopEntryEdit: Identifiable {
		enum Kind: String { case rename, move, delete }
		let id = UUID()
		let entry: SushitrainEntry
		let kind: Kind
		let folderID, path, name, identity: String
		init(entry: SushitrainEntry, kind: Kind) {
			self.entry = entry
			self.kind = kind
			folderID = entry.folder?.folderID ?? ""
			path = entry.path()
			name = entry.name()
			identity = entry.desktopIdentity()
		}
	}

	@MainActor func deleteDesktopEntry(_ target: DesktopEntryEdit) async throws {
		guard !target.identity.isEmpty else { throw MCPAppError("This item changed. Refresh before deleting it.") }
		let data = try MCPIntegration.json([
			"Action": "deleteFile", "Folder": target.folderID, "Path": target.path, "Token": target.identity,
		])
		_ = try await MCPIntegration.shared.nativeEngineCall("engine.action", data: data)
	}

	struct DesktopSyncFileMenu<Extra: View>: View {
		let entry: SushitrainEntry
		let info, versions, delete: () -> Void
		let edit: (DesktopEntryEdit) -> Void
		@ViewBuilder let extra: () -> Extra
		@Environment(\.openURL) private var openURL
		@Environment(\.openWindow) private var openWindow
		@Environment(\.navigateTo) private var navigateTo
		private var canEdit: Bool {
			entry.isLocallyPresent() && !entry.isSymlink() && entry.folder?.canRestoreDownloadedVersion() == true
		}
		var body: some View {
			ForEach(DesktopUICatalog.desktop.detailDescriptors["fileActions"] ?? []) { action in
				switch action.id {
				case "open":
					Button("Open") {
						if entry.isDirectory(), let folder = entry.folder {
							navigateTo(.folder(folderID: folder.folderID, prefix: entry.path() + "/"))
						}
						else if let url = entry.localNativeFileURL {
							openURL(url)
						}
						else {
							preview()
						}
					}
				case "preview": if !entry.isDirectory() { Button("Preview") { preview() }.disabled(!entry.canAttemptPreview) }
				case "offline":
					Button(entry.isLocallyPresent() ? "Available Offline" : "Waiting for Synchronization") {}.disabled(true)
				case "renameMove":
					Button("Rename…") { edit(DesktopEntryEdit(entry: entry, kind: .rename)) }.disabled(!canEdit)
					Button("Move…") { edit(DesktopEntryEdit(entry: entry, kind: .move)) }.disabled(!canEdit)
				case "previousVersions": if entry.canShowVersions { Button("Previous Versions", action: versions) }
				case "information":
					Button("Information", action: info)
					extra()
				case "delete":
					if entry.isSelected() && entry.isLocallyPresent() && entry.folder?.folderType() == SushitrainFolderTypeSendReceive
					{
						Button("Delete…", role: .destructive, action: delete)
					}
				default: EmptyView()
				}
			}
		}
		private func preview() {
			if let folder = entry.folder {
				openWindow(id: "preview", value: Preview(folderID: folder.folderID, path: entry.path()))
			}
		}
	}

	struct DesktopEntryMutationSheet: View {
		let edit: DesktopEntryEdit
		@State private var value: String
		@State private var working = false
		@State private var problem: String?
		@Environment(\.dismiss) private var dismiss
		init(edit: DesktopEntryEdit) {
			self.edit = edit
			_value = State(initialValue: edit.kind == .rename ? edit.name : edit.path)
		}
		var body: some View {
			NavigationStack {
				Form {
					TextField(edit.kind == .rename ? "New name" : "Destination relative to this synchronized folder", text: $value)
					if let problem { Text(problem).foregroundStyle(.red) }
				}.padding().disabled(working)
					.navigationTitle(edit.kind == .rename ? "Rename" : "Move")
					.settingsEditor(hasChanges: true, isSaving: working)
					.toolbar {
						ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(working) }
						ToolbarItem(placement: .confirmationAction) { Button("Save") { save() }.disabled(working || value.isEmpty) }
					}
			}.frame(minWidth: 480, minHeight: 160).interactiveDismissDisabled()
		}
		private func save() {
			guard !working, !edit.folderID.isEmpty else { return }
			working = true
			Task {
				defer { working = false }
				do {
					let data = try MCPIntegration.json([
						"Folder": edit.folderID, "Path": edit.path, "Name": value, "Token": edit.identity,
					])
					_ = try await MCPIntegration.shared.nativeEngineCall("engine." + edit.kind.rawValue, data: data)
					dismiss()
				}
				catch { problem = error.localizedDescription }
			}
		}
	}
#endif
