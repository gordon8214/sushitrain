// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore

struct ShareFolderWithDeviceDetailsView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.dismiss) private var dismiss

	let folder: SushitrainFolder
	let deviceID: String

	@State private var newPassword: String = ""
	@FocusState private var passwordFieldFocus: Bool
	@State private var error: String? = nil
	@State private var device: SushitrainPeer? = nil

	private func update() async {
		self.device = appState.client.peer(withID: deviceID)
	}

	var body: some View {
		NavigationStack {
			Form {
				if let device = device {
					Section("Share with device") {
						DeviceIDView(device: device)
					}

					Section {
						TextField("Password", text: $newPassword)
							.textContentType(.password)
							.monospaced()
							.focused($passwordFieldFocus)
					} header: {
						Text("Encryption password")
					} footer: {
						Text(
							"If you leave the password empty, files will not be encrypted on the other device, and therefore can be read by anyone who has access to the other device. If you set an encryption password, ensure that all devices that synchronize the same folder with the other device are using the same password."
						).multilineTextAlignment(.leading)
							.lineLimit(nil)
							.fixedSize(horizontal: false, vertical: true)
					}
				}
			}
			.task {
				await self.update()
			}
			.formStyle(.grouped)
			.onAppear {
				self.newPassword = folder.encryptionPassword(for: deviceID)
				passwordFieldFocus = true
			}
			.navigationTitle("Share folder '\(folder.displayName)'")
			.toolbar {
				SheetButton(role: .save) {
					do {
						try folder.share(
							withDevice: self.deviceID, toggle: true,
							encryptionPassword: newPassword)
						dismiss()
					}
					catch let error {
						self.error = error.localizedDescription
					}
				}

				SheetButton(role: .cancel) {
					dismiss()
				}
			}
		}
		.alert(isPresented: Binding.isNotNil($error)) {
			Alert(title: Text("Could not set encryption key"), message: Text(self.error!))
		}
	}
}

struct FolderStatusDescription {
	var text: String
	var systemImage: String
	var color: Color
	var badge: String
	var additionalText: String?

	init(_ folder: SushitrainFolder) {
		self.badge = ""
		self.additionalText = nil
		if !folder.exists() {
			(self.text, self.systemImage, self.color) = (String(localized: "Folder does not exist"), "trash", .red)
		}
		else if folder.isPaused() {
			(self.text, self.systemImage, self.color) = (String(localized: "Synchronization paused"), "pause.circle", .gray)
		}
		else if folder.isStoppedOnError {
			// Before anything about other devices: a folder that stopped has this to say whether or not they are connected
			(self.text, self.systemImage, self.color, self.additionalText) = Self.describeError(of: folder)
		}
		else {
			let peerCount = (folder.sharedWithDeviceIDs()?.count() ?? 1) - 1
			let peerStatusText = "\(folder.connectedPeerCount())/\(peerCount)"

			if folder.connectedPeerCount() == 0 {
				(self.text, self.systemImage, self.color) = (String(localized: "Not connected"), "network.slash", .gray)
				self.badge = peerStatusText
			}
			else {
				switch folder.state() {
				case "idle", "sync-waiting", "scan-waiting", "clean-waiting":
					// A folder that is done with what it could do isn't necessarily done
					if folder.hasUnresolvedItems() {
						(self.text, self.systemImage, self.color) = (
							String(localized: "Out of sync"), "exclamationmark.triangle.fill", .orange
						)
					}
					else {
						(self.text, self.systemImage, self.color) = (
							String(localized: "Synchronized"), "checkmark.circle.fill", .green
						)
					}
					self.badge = peerStatusText

				case "syncing":
					(self.text, self.systemImage, self.color) = (
						String(localized: "Synchronizing..."), "bolt.horizontal.circle", .orange
					)

				case "scanning":
					(self.text, self.systemImage, self.color) = (String(localized: "Scanning..."), "bolt.horizontal.circle", .orange)

				case "sync-preparing":
					(self.text, self.systemImage, self.color) = (
						String(localized: "Preparing to synchronize..."), "bolt.horizontal.circle", .orange
					)

				case "cleaning":
					(self.text, self.systemImage, self.color) = (
						String(localized: "Cleaning up..."), "bolt.horizontal.circle", .orange
					)

				default:
					(self.text, self.systemImage, self.color) = (
						String(localized: "Unknown state"), "exclamationmark.triangle.fill", .red
					)
				}
			}
		}
	}

	/// Two errors have an explanation of their own. Anything else is best told in the engine's words.
	private static func describeError(of folder: SushitrainFolder) -> (String, String, Color, String?) {
		let message = folder.stateError()
		if !folder.isDiskSpaceSufficient() {
			return (String(localized: "Insufficient free storage space"), "exclamationmark.triangle.fill", .red, nil)
		}
		// Error message is "fcntl /private/...: too many open files"
		if folder.isWatcherEnabled() && message.contains("too many open files") {
			return (
				String(localized: "Folder too large for watching"), "exclamationmark.triangle.fill", .red,
				String(localized: "Disabling 'watch for changes' for this folder may resolve the issue.")
			)
		}
		return (String(localized: "Error"), "exclamationmark.triangle.fill", .red, message)
	}

	var fullText: String {
		var text = self.text
		if !self.badge.isEmpty {
			text += " (\(self.badge))"
		}
		return text
	}
}

struct FolderStatusView: View {
	@Environment(AppState.self) private var appState
	var folder: SushitrainFolder

	@State private var statistics: SushitrainFolderStats? = nil
	@State private var status: String? = nil
	@State private var folderStatusDescription: FolderStatusDescription? = nil

	var body: some View {
		VStack {
			if let status = status {
				if status == "syncing" {
					if let statistics = self.statistics, statistics.global!.bytes > 0 {
						let formatter = ByteCountFormatter()
						if let globalBytes = statistics.global?.bytes, let localBytes = statistics.local?.bytes {
							let remainingText = formatter.string(fromByteCount: (globalBytes - localBytes))

							ProgressView(
								value: Double(localBytes) / Double(globalBytes),
								total: 1.0
							) {
								Label(
									"Synchronizing...", systemImage: "bolt.horizontal.circle"
								)
								.foregroundStyle(.orange)
								.badge(Text(remainingText))
							}.tint(.orange)
						}
						else {
							self.statusLabel()
						}
					}
					else {
						self.statusLabel()
					}
				}
				else {
					self.statusLabel()
				}

				if let folderStatus = self.folderStatusDescription, let txt = folderStatus.additionalText {
					Text(txt).foregroundStyle(.red)
				}
			}
		}.task(id: appState.eventCounter) {
			await self.update()
		}
	}

	@concurrent private func update() async {
		let status = folder.state()
		let statistics = try? folder.statistics()
		let folderStatusDescription = FolderStatusDescription(folder)

		Task { @MainActor in
			self.status = status
			self.statistics = statistics
			self.folderStatusDescription = folderStatusDescription
		}
	}

	@ViewBuilder private func statusLabel() -> some View {
		// Worked out in update, as it asks the engine for more than a view's body should wait for
		if let folderStatus = self.folderStatusDescription {
			Label(folderStatus.fullText, systemImage: folderStatus.systemImage)
				.foregroundStyle(folderStatus.color)
		}
	}
}

struct FolderDirectionPicker: View {
	@Environment(AppState.self) private var appState
	var folder: SushitrainFolder
	@State private var changeProhibited: Bool = true
	@State private var folderType: String? = nil

	var body: some View {
		if folder.exists() {
			Picker("Direction", selection: $folderType) {
				Text("Send and receive")
					.help(
						"Changes from other devices will be accepted, and changes made on this device will be sent to other devices."
					)
					.tag(SushitrainFolderTypeSendReceive as String?)

				Text("Receive only")
					.help(
						"Changes from other devices will be accepted. Changes made on this device will not be sent to other devices."
					)
					.tag(SushitrainFolderTypeReceiveOnly as String?)

				Text("Send only").tag(SushitrainFolderTypeSendOnly as String?)
					.help(
						"Changes made on this device will be sent to other devices. Changes from other devices will not be accepted."
					)

				if folderType == nil {
					Text("(Unknown)").tag(nil as String?)
						.disabled(true)
						.selectionDisabled()
				}

				if folderType == SushitrainFolderTypeReceiveEncrypted {
					Text("Receive encrypted")
						.tag(SushitrainFolderTypeReceiveEncrypted as String?)
						.disabled(true)
						.selectionDisabled()
				}
			}
			.pickerStyle(.menu)
			.disabled(changeProhibited || folderType == SushitrainFolderTypeReceiveOnly)
			.onAppear {
				self.update()
			}
			.onChange(of: folderType) { ov, nv in
				if let nv = nv, ov != nil && nv != ov {
					Log.info("changing folder type to \(nv) for folder \(self.folder.folderID)")
					try? self.folder.setFolderType(nv)
				}
			}
		}
	}

	private func update() {
		self.folderType = self.folder.folderType()
		// Only allow changes to the direction when the folder is idle
		changeProhibited = !folder.isIdleOrSyncing
	}
}

private struct PhotoFolderSectionView: View {
	var body: some View {
		Label("Photo folder", systemImage: "camera.fill").foregroundStyle(.cyan)
	}
}

private struct ExternalFolderSectionView: View {
	@Environment(AppState.self) private var appState
	var folder: SushitrainFolder
	@State private var showPathSelector: Bool = false
	@State private var errorText: String? = nil

	var body: some View {
		let isAccessible = BookmarkManager.shared.hasBookmarkFor(folderID: folder.folderID)
		Section {
			ZStack {
				if isAccessible {
					Label("External folder", systemImage: "app.badge.checkmark").foregroundStyle(.pink)
						.onLongPressGesture {
							self.showPathSelector = true
						}
				}
				else {
					Label("Inaccessible external folder", systemImage: "xmark.app").foregroundStyle(.red).onTapGesture {
						self.tryFixBookmark()
					}
				}
			}
			// Folder selector for fixing external folders
			.fileImporter(
				isPresented: $showPathSelector, allowedContentTypes: [.folder],
				onCompletion: { result in
					switch result {
					case .success(let url):
						if appState.isInsideDocumentsFolder(url) {
							// Check if the folder path is or is inside our regular folder path - that is not allowed
							self.errorText = String(
								localized:
									"The folder you have selected is inside the app folder. Only folders outside the app folder can be selected."
							)
							return
						}

						// Attempt to create a new bookmark
						do {
							try? self.folder.setPaused(true)
							try BookmarkManager.shared.saveBookmark(folderID: self.folder.folderID, url: url)
							try folder.setPath(url.path(percentEncoded: false))
							try? self.folder.setPaused(false)
						}
						catch {
							self.errorText = String(localized: "Could not re-link folder: \(error.localizedDescription)")
							return
						}

					case .failure(let e):
						Log.warn("Failed to select folder: \(e.localizedDescription)")
					}
				}
			)
			.alert(isPresented: Binding.isNotNil($errorText)) {
				Alert(title: Text("Could not relink folder"), message: Text(errorText ?? ""), dismissButton: .default(Text("OK")))
			}
		} footer: {
			if isAccessible {
				Text("This folder is not in the default location, and may belong to another app.")
			}
			else {
				Text(
					"This folder is external to this app, and cannot be accessed anymore. To resolve this issue, unlink the folder and re-add it."
				)
			}
		}
	}

	private func tryFixBookmark() {
		// Pause the folder
		try? self.folder.setPaused(true)

		if BookmarkManager.shared.hasBookmarkFor(folderID: self.folder.folderID) {
			Log.info("We already have a bookmark for folder \(self.folder.folderID)")
		}

		if let u = self.folder.localNativeURL {
			do {
				try BookmarkManager.shared.saveBookmark(folderID: self.folder.folderID, url: u)
			}
			catch {
				Log.warn("while attempting bookmark recreation: \(error.localizedDescription)")
			}
		}

		// Do we have a bookmark now?
		if BookmarkManager.shared.hasBookmarkFor(folderID: self.folder.folderID) {
			Log.info("We now have a bookmark for folder \(self.folder.folderID)")
			return
		}

		self.showPathSelector = true
	}
}

struct ExternalFolderInaccessibleView: View {
	@Environment(AppState.self) private var appState

	var folder: SushitrainFolder
	@State private var showPathSelector: Bool = false
	@State private var errorText: String? = nil

	var body: some View {
		let isAccessible = BookmarkManager.shared.hasBookmarkFor(folderID: folder.folderID)

		if isAccessible {
			ContentUnavailableView(
				"External folder",
				systemImage: "app.badge.checkmark",
				description: Text("The folder is accessible.")
			)
		}
		else {
			ContentUnavailableView(
				label: {
					Label("Inaccessible folder", systemImage: "xmark.app")
				},
				description: {
					Text(
						"Synctrain cannot access this folder anymore. Please click the button below to re-select the folder on your system. This should re-grant Synctrain access to the folder."
					)
				},
				actions: {
					Button("Re-link folder...", systemImage: "link") {
						self.tryFixBookmark()
					}
				}
			)
			// Folder selector for fixing external folders
			.fileImporter(
				isPresented: $showPathSelector, allowedContentTypes: [.folder],
				onCompletion: { result in
					switch result {
					case .success(let url):
						if appState.isInsideDocumentsFolder(url) {
							// Check if the folder path is or is inside our regular folder path - that is not allowed
							self.errorText = String(
								localized:
									"The folder you have selected is inside the app folder. Only folders outside the app folder can be selected."
							)
							return
						}

						// Attempt to create a new bookmark
						do {
							try? self.folder.setPaused(true)
							try BookmarkManager.shared.saveBookmark(folderID: self.folder.folderID, url: url)
							try folder.setPath(url.path(percentEncoded: false))
							try? self.folder.setPaused(false)
						}
						catch {
							self.errorText = String(localized: "Could not re-link folder: \(error.localizedDescription)")
							return
						}

					case .failure(let e):
						Log.warn("Failed to select folder: \(e.localizedDescription)")
					}
				}
			)
			.alert(isPresented: Binding.isNotNil($errorText)) {
				Alert(title: Text("Could not relink folder"), message: Text(errorText ?? ""), dismissButton: .default(Text("OK")))
			}
		}
	}

	private func tryFixBookmark() {
		// Pause the folder
		try? self.folder.setPaused(true)

		if BookmarkManager.shared.hasBookmarkFor(folderID: self.folder.folderID) {
			Log.info("We already have a bookmark for folder \(self.folder.folderID)")
		}

		if let u = self.folder.localNativeURL {
			do {
				try BookmarkManager.shared.saveBookmark(folderID: self.folder.folderID, url: u)
			}
			catch {
				Log.warn("while attempting bookmark recreation: \(error.localizedDescription)")
			}
		}

		// Do we have a bookmark now?
		if BookmarkManager.shared.hasBookmarkFor(folderID: self.folder.folderID) {
			Log.info("We now have a bookmark for folder \(self.folder.folderID)")
			return
		}

		self.showPathSelector = true
	}
}

struct FolderView: View {
	var folder: SushitrainFolder
	@Environment(AppState.self) private var appState
	@Environment(\.dismiss) private var dismiss

	private enum FolderViewTab: String, CaseIterable, Identifiable {
		case general = "general"
		case sharing = "sharing"
		case photo = "photo"
		case advanced = "advanced"
		var id: Self { self }
	}

	@State private var advancedExpanded = false
	@State private var sharedPeers: [SushitrainPeer] = []
	@State private var otherPeers: [SushitrainPeer] = []
	@State private var unsupportedDataProtection = false
	@State private var selectedTab: FolderViewTab = .general
	@State private var isExternal: Bool? = nil
	@State private var isPhotoFolder = false
	@State private var editSharingWith: SushitrainPeer? = nil

	@concurrent func update() async {
		let possiblePeers = await appState.peers().sorted().filter({ d in !d.isSelf() })
		let folderID = folder.folderID
		let sharedPeers = possiblePeers.filter { $0.isFolderShared(folderID) }
		let otherPeers = possiblePeers.filter { !$0.isFolderShared(folderID) }

		Task { @MainActor in
			self.isExternal = folder.isExternal
			self.isPhotoFolder = folder.isPhotoFolder
			self.sharedPeers = sharedPeers
			self.otherPeers = otherPeers
			self.unsupportedDataProtection =
				self.folder.isRegularFolder && URL(fileURLWithPath: self.folder.path()).hasUnsupportedProtection()
		}
	}

	var body: some View {
		if folder.exists() {
			VStack {
				self.tabSwitcher()

				Form {
					switch selectedTab {
					case .general:
						self.generalSection()
					case .sharing:
						self.sharingSection()
					case .photo:
						PhotoFolderSettingsView(folder: self.folder)
					case .advanced:
						AdvancedFolderSettingsView(folder: self.folder)
					}
				}
				.formStyle(.grouped)
			}
			.navigationTitle(folder.displayName)
			.task {
				await self.update()
			}
			.sheet(
				item: $editSharingWith,
				onDismiss: {
					Task {
						await self.update()
					}
				}
			) { withDevice in
				NavigationStack {
					ShareFolderWithDeviceDetailsView(
						folder: self.folder,
						deviceID: withDevice.deviceID()
					)
				}
			}
		}
	}

	@ViewBuilder private func tabSwitcher() -> some View {
		Picker("Page", selection: $selectedTab) {
			Text("General").tag(FolderViewTab.general)
			Text("Sharing").tag(FolderViewTab.sharing)
			if isPhotoFolder {
				Label("Albums", systemImage: "photo.on.rectangle.angled").tag(FolderViewTab.photo)
			}
			if folder.isRegularFolder || folder.isPhotoFolder || folder.isReceiveEncryptedFolder {
				Label("Advanced", systemImage: "gear").tag(FolderViewTab.advanced)
			}
		}
		.pickerStyle(.segmented)
		.controlSize(.large)
		.dynamicTypeSize(.large)
		.listRowBackground(Color.clear)
		.background(Color.clear)
		.labelsHidden()
		.scrollContentBackground(.hidden)
		.listRowInsets(
			EdgeInsets(
				top: 0,
				leading: 0,
				bottom: 8,
				trailing: 0
			)
		)
		.padding(.top, 24)
	}

	@ViewBuilder private func sharingSection() -> some View {
		if !sharedPeers.isEmpty {
			Section(header: Text("Shared with")) {
				ForEach(self.sharedPeers, id: \.self.id) { (addr: SushitrainPeer) in
					ShareWithDeviceToggleView(
						peer: addr,
						folder: folder,
						showFolderName: false,
						details: { self.editSharingWith = addr }
					).id(addr.id)
				}
			}
		}

		if !otherPeers.isEmpty {
			Section {
				ForEach(self.otherPeers, id: \.self.id) { (addr: SushitrainPeer) in
					ShareWithDeviceToggleView(
						peer: addr,
						folder: folder,
						showFolderName: false,
						details: { self.editSharingWith = addr }
					).id(addr.id)
				}
			}
		}
	}

	@ViewBuilder private func generalSection() -> some View {
		FolderStatusSection(folder: self.folder)

		if self.isPhotoFolder {
			PhotoFolderSectionView()
		}
		else if self.isExternal == true {
			ExternalFolderSectionView(folder: folder)
		}

		if self.unsupportedDataProtection {
			Section {
				Label("Limited access", systemImage: "xmark.circle")
					.foregroundStyle(.red)
			} footer: {
				Text("The selected folder is protected, and therefore cannot be accessed while the device is locked.")
			}
		}

		Section {
			Text("Folder ID").badge(Text(folder.folderID))

			LabeledContent {
				TextField(
					"",
					text: Binding(get: { folder.label() }, set: { lbl in try? folder.setLabel(lbl) }),
					prompt: Text(folder.folderID)
				)
				.multilineTextAlignment(.trailing)
			} label: {
				Text("Display name")
			}

			FolderDirectionPicker(folder: folder).disabled(isPhotoFolder || folder.isReceiveEncryptedFolder).id(
				appState.eventCounter)
		} header: {
			Text("Folder settings")
		} footer: {
			if isPhotoFolder {
				Text("Photo folders can only be send-only.")
			}
		}

		Section {
			Toggle(
				"Synchronize",
				isOn: Binding(
					get: { !folder.isPaused() },
					set: { active in try? folder.setPaused(!active) })
			)
		}

	}
}

struct ShareWithDeviceToggleView: View {
	@Environment(AppState.self) private var appState
	let peer: SushitrainPeer
	let folder: SushitrainFolder
	let showFolderName: Bool
	let details: () -> Void

	@State private var editEncryptionPasswordDeviceID = ""
	@State private var isShared: Bool? = nil
	@State private var isPending: Bool = false
	@State private var isSharedEncrypted: Bool = false

	private func share(_ shared: Bool) {
		do {
			if shared && peer.isUntrusted() {
				details()
			}
			else {
				try folder.share(withDevice: peer.deviceID(), toggle: shared, encryptionPassword: "")
			}
		}
		catch let error {
			Log.warn("Error sharing folder: " + error.localizedDescription)
		}
		Task {
			self.update()
		}
	}

	var body: some View {
		HStack {
			let isSharedBinding = Binding(
				get: {
					return self.isShared ?? false
				},
				set: { nv in
					share(nv)
				})

			if showFolderName {
				Toggle(folder.displayName, systemImage: "folder.fill", isOn: isSharedBinding)
					.bold(isPending)
					.disabled(self.isShared == nil)
			}
			else {
				Toggle(peer.displayName, systemImage: peer.systemImage, isOn: isSharedBinding)
					.bold(isPending)
					.disabled(self.isShared == nil)
			}

			Button("Encryption password", systemImage: isSharedEncrypted ? "lock" : "lock.open") {
				self.details()
			}
			.labelStyle(.iconOnly)
			.disabled(self.isShared == nil || self.folder.isReceiveEncryptedFolder)
		}
		.task {
			self.update()
		}
	}

	private func update() {
		self.isShared = folder.isShared(withDeviceID: peer.deviceID())

		let sharedEncrypted = folder.sharedEncryptedWithDeviceIDs()?.asArray() ?? []
		self.isSharedEncrypted = sharedEncrypted.contains(peer.deviceID())

		let pendingPeerIDs = Set(
			(try? appState.client.devicesPendingFolder(self.folder.folderID))?.asArray() ?? [])
		self.isPending = pendingPeerIDs.contains(self.peer.deviceID())
	}
}

private struct FolderThumbnailSettingsView: View {
	@Environment(AppState.self) private var appState
	let folder: SushitrainFolder

	@State private var showGenerateThumbnails = false
	@State private var showGenerateThumbnailsConfirm = false
	@State private var showClearThumbnailsConfirm = false
	@State private var diskCacheSizeBytes: UInt? = nil
	@State private var deletingFromSharedCache = false
	@State private var deletedFromSharedCache = false
	@State private var settings = ThumbnailGeneration.disabled

	private var insidePathBinding: Binding<String> {
		return Binding(
			get: {
				let tg = FolderSettingsManager.shared.settingsFor(folderID: folder.folderID).thumbnailGeneration
				if case ThumbnailGeneration.inside(path: let path) = tg {
					return path
				}
				return ThumbnailGeneration.defaultInsideFolderThumbnailPath
			},
			set: { newPath in
				FolderSettingsManager.shared.mutateSettingsFor(folderID: folder.folderID) { fs in
					fs.thumbnailGeneration = .inside(path: newPath)
				}
			})
	}

	private var insideChoice: ThumbnailGeneration {
		switch settings {
		case .inside(let path): return .inside(path: path)
		default: return .inside(path: ThumbnailGeneration.defaultInsideFolderThumbnailPath)
		}
	}

	private func updateSize() async {
		self.diskCacheSizeBytes = nil
		switch self.settings {
		case .inside(_), .deviceLocal:
			let ic = ImageCache.forFolder(self.folder)
			if ic !== ImageCache.shared {
				self.diskCacheSizeBytes = try? await ic.diskCacheSizeBytes()
			}
		case .disabled, .global: return
		}
	}

	var canClear: Bool {
		switch self.settings {
		case .inside(path: let p): return !p.isEmpty
		case .deviceLocal: return true
		case .disabled, .global: return false
		}
	}

	var body: some View {
		Form {
			Section {
				Picker("Thumbnails", selection: $settings) {
					Text("Do not cache").tag(ThumbnailGeneration.disabled)
					Text("Use app-wide cache").tag(ThumbnailGeneration.global)
					Text("Cache inside folder").tag(insideChoice)
					Text("Cache on this device").tag(ThumbnailGeneration.deviceLocal)
				}
				.pickerStyle(.menu)

				if case ThumbnailGeneration.inside(_) = settings {
					LabeledContent {
						TextField("", text: insidePathBinding, prompt: Text(ThumbnailGeneration.defaultInsideFolderThumbnailPath))
							.multilineTextAlignment(.trailing)
					} label: {
						Text("Subdirectory")
					}
				}
			} footer: {
				if let bytes = self.diskCacheSizeBytes {
					let formatter = ByteCountFormatter()
					Text(
						"Currently the thumbnail cache is using \(formatter.string(fromByteCount: Int64(bytes))) of disk space."
					)
				}
			}

			if settings != .disabled {
				Section {
					Button("Generate thumbnails", systemImage: "photo.stack") {
						self.showGenerateThumbnailsConfirm = true
					}
					.disabled(settings == .disabled || settings == .global && !appState.userSettings.cacheThumbnailsToDisk)
					.buttonStyle(.link)
					.confirmationDialog(
						"Generating thumbnails may take a while and could use a lot of data. It is advisable to connect to a Wi-Fi network before proceeding. Are you sure you want to continue?",
						isPresented: $showGenerateThumbnailsConfirm, titleVisibility: .visible
					) {
						Button("Generate thumbnails") {
							self.showGenerateThumbnails = true
							self.diskCacheSizeBytes = nil
						}
					}
				}
			}

			if self.canClear {
				Section {
					// Open cache directory in Finder
					if case .inside(_) = settings {
						Button(openInFilesAppLabel, systemImage: "arrow.up.forward.app") {
							let ic = ImageCache.forFolder(self.folder)
							if let url = ic.customPersistentCache?.localPath {
								openURLInSystemFilesApp(url: url)
							}
						}
					}

					Button("Clear thumbnail cache", systemImage: "eraser.line.dashed.fill") {
						self.showClearThumbnailsConfirm = true
					}
					.buttonStyle(.link)
					.foregroundStyle(.red)
					.confirmationDialog(
						"This will delete all files from inside the configured subdirectory. Are you sure you want to continue?",
						isPresented: $showClearThumbnailsConfirm, titleVisibility: .visible
					) {
						Button("Delete thumbnails") {
							let ic = ImageCache.forFolder(self.folder)
							if ic !== ImageCache.shared {
								Task {
									await ic.clear()
									self.diskCacheSizeBytes = nil
								}
							}
						}
					}
				}
			}

			if appState.userSettings.cacheThumbnailsToDisk {
				Section {
					Button("Remove thumbnails from app-wide cache", systemImage: "eraser.line.dashed.fill") {
						deletedFromSharedCache = false
						deletingFromSharedCache = true
						Task.detached {
							do {
								try await self.deleteFromSharedCache(prefix: nil)
							}
							catch {
								Log.warn("Failed deleting from shared cache: \(error.localizedDescription)")
							}

							DispatchQueue.main.async {
								Task {
									self.diskCacheSizeBytes = nil
									await self.updateSize()
									deletingFromSharedCache = false
									deletedFromSharedCache = true
								}
							}
						}
					}
					.alert(isPresented: $deletedFromSharedCache) {
						Alert(
							title: Text("Thumbnails deleted"), message: Text("Thumbnails have been deleted from the app-wide cache"),
							dismissButton: .default(Text("OK")))
					}
					.disabled(deletingFromSharedCache)
					.foregroundStyle(.red)
					.buttonStyle(.link)
				} footer: {
					Text("Remove thumbnails that were generated for files in this folder from the app-wide thumbnail cache.")
				}
			}
		}
		.formStyle(.grouped)
		.navigationTitle("Thumbnails")
		.onChange(of: settings, initial: false) { _, nv in
			FolderSettingsManager.shared.mutateSettingsFor(folderID: folder.folderID) { fs in
				fs.thumbnailGeneration = nv
			}
		}
		.onAppear {
			self.settings = FolderSettingsManager.shared.settingsFor(folderID: folder.folderID).thumbnailGeneration
		}
		.task {
			await Task.detached {
				await self.updateSize()
			}.value
		}
		.sheet(isPresented: $showGenerateThumbnails) {
			NavigationStack {
				FolderGenerateThumbnailsView(isShown: $showGenerateThumbnails, folder: self.folder)
					.navigationTitle("Generate thumbnails")
					.toolbar {
						SheetButton(role: .cancel) {
							showGenerateThumbnails = false
						}
					}
			}
		}
	}

	private func deleteFromSharedCache(prefix: String?) async throws {
		// Iterate over this folder's entries
		let files = try self.folder.list(prefix, directories: false, recurse: false)

		for idx in 0..<files.count() {
			let filePath = files.item(at: idx)
			if Task.isCancelled {
				Log.info("Thumbnail generate task cancelled")
				return
			}

			let fullPath = (prefix ?? "") + "/" + filePath

			if let file = try? self.folder.getFileInformation(fullPath) {
				// Recurse into subdirectories (depth-first)
				if file.isDirectory() {
					try await self.deleteFromSharedCache(prefix: file.path())
				}

				try? await ImageCache.shared.remove(cacheKey: file.cacheKey)
			}
			else {
				Log.warn("Could not get file entry for path \(filePath)")
			}
		}
	}
}

private struct FolderGenerateThumbnailsView: View {
	@Environment(AppState.self) private var appState
	@Binding var isShown: Bool
	let folder: SushitrainFolder
	@State private var error: String? = nil
	@State private var totalFiles: Int = 0
	@State private var processedFiles: Int = 0
	@State private var lastThumbnail: AsyncImagePhase? = nil
	@State private var lastThumbnailTime: Date = Date.distantPast
	@State private var generatingTask: Task<Void, Never>? = nil

	private func startGenerating() {
		if self.generatingTask != nil {
			return
		}

		self.generatingTask = Task {
			Log.info("Start generating thumbnails")
			let ic = ImageCache.forFolder(self.folder)
			let diskHasSpace = await ic.diskHasSpace()
			if !ic.diskCacheEnabled || !diskHasSpace {
				self.error = String(
					localized:
						"There is very little disk space available. Free up some space and try again."
				)
				return
			}

			do {
				let folder = self.folder
				let stats = try await Task.detached(priority: .medium) {
					try folder.statistics()
				}.value
				self.totalFiles = stats.global?.files ?? 0
				self.processedFiles = 0
				try await self.generateFor(prefix: nil)
				self.isShown = false
			}
			catch {
				self.error = error.localizedDescription
			}

		}
	}

	var body: some View {
		VStack {
			if let e = error {
				Text(e).foregroundStyle(.red)
			}
			else {
				if let img = self.lastThumbnail {
					switch img {
					case .success(let img):
						img.frame(maxWidth: 200, maxHeight: 200)
							.scaledToFit()
							.clipShape(.rect(cornerRadius: 10))
							.contentShape(.rect)
					default:
						Rectangle().frame(width: 200, height: 200)
							.foregroundStyle(.gray)
							.opacity(0.2)
							.clipShape(.rect(cornerRadius: 10))
							.contentShape(.rect)
					}
				}
				else {
					Rectangle().frame(width: 200, height: 200).foregroundStyle(.gray).opacity(0.2)
						.clipShape(.rect(cornerRadius: 10))
						.contentShape(.rect)
				}

				Text("Generating thumbnails... (\(self.processedFiles) / \(self.totalFiles))")
				if self.totalFiles > 0 {
					ProgressView(value: Float(self.processedFiles), total: Float(max(self.totalFiles, self.processedFiles)))
				}
			}
		}
		.padding(30)
		.alert(isPresented: Binding.isNotNil($error)) {
			Alert(
				title: Text("An error occurred"), message: Text(error!),
				dismissButton: .default(Text("OK")) {
					isShown = false
				})
		}
		.onAppear {
			self.startGenerating()
		}
		.onDisappear {
			self.generatingTask?.cancel()
		}
	}

	private static let thumbnailInterval: TimeInterval = 1.0

	private func generateFor(prefix: String?) async throws {
		let tg = FolderSettingsManager.shared.settingsFor(folderID: folder.folderID).thumbnailGeneration
		try await generateThumbnailsFor(
			folder: self.folder, prefix: prefix, userSettings: appState.userSettings, generation: tg,
			callback: { thumb in
				if (-lastThumbnailTime.timeIntervalSinceNow) > Self.thumbnailInterval {
					lastThumbnailTime = Date.now
					self.lastThumbnail = thumb
				}
				self.processedFiles += 1
			})
	}
}

private struct AdvancedFolderSettingsView: View {
	@Environment(AppState.self) private var appState
	let folder: SushitrainFolder

	@State private var showError: ErrorMessage? = nil

	@State private var showThumbnailsSettingsSheet: Bool = false
	@State private var showVersioningSettingsSheet: Bool = false
	@State private var showExternalSharingSettingsSheet: Bool = false
	@State private var showSyncSettingsSheet: Bool = false
	@State private var showMetadataSettingsSheet: Bool = false

	var body: some View {
		self.subSettingsSection()
		self.folderGroupSection()
		self.systemSettingsSection()
		self.trashSection()
		self.watchSection()
		self.conflictsSection()
		self.blockIndexSection()
	}

	@ViewBuilder private func subSettingsSection() -> some View {
		// Sub screens for editing advanced settings
		Section {
			Button("Synchronization options...", systemImage: "slider.horizontal.3") {
				showSyncSettingsSheet = true
			}
			.sheet(isPresented: $showSyncSettingsSheet) {
				NavigationStack {
					FolderSyncSettingsView(folder: folder)
				}
			}
			.buttonStyle(.link)

			Button("Metadata...", systemImage: "tag") {
				showMetadataSettingsSheet = true
			}
			.sheet(isPresented: $showMetadataSettingsSheet) {
				NavigationStack {
					FolderMetadataSettingsView(folder: folder)
				}
			}
			.buttonStyle(.link)

			// Thumbnail settings button
			if !folder.isReceiveEncryptedFolder {
				Button("Thumbnails...", systemImage: "photo.stack") {
					showThumbnailsSettingsSheet = true
				}
				.sheet(isPresented: $showThumbnailsSettingsSheet) {
					NavigationStack {
						FolderThumbnailSettingsView(folder: folder)
							.toolbar {
								SheetButton(role: .done) {
									showThumbnailsSettingsSheet = false
								}
							}
					}

				}
				.buttonStyle(.link)

				// File versioning settings button
				Button("File versioning...", systemImage: "clock.arrow.circlepath") {
					showVersioningSettingsSheet = true
				}
				.sheet(isPresented: $showVersioningSettingsSheet) {
					NavigationStack {
						FolderVersioningSettingsView(folder: folder)
					}
				}
				.buttonStyle(.link)

				// External sharing settings button
				Button("External sharing...", systemImage: "link.circle.fill") {
					showExternalSharingSettingsSheet = true
				}
				.sheet(isPresented: $showExternalSharingSettingsSheet) {
					NavigationStack {
						ExternalSharingSettingsView(folder: self.folder)
							.toolbar {
								SheetButton(role: .done) {
									showExternalSharingSettingsSheet = false
								}
							}
					}
				}
				.buttonStyle(.link)
			}
		}
	}

	@ViewBuilder private func trashSection() -> some View {
		if folder.isNativeFilesystem() {
			Section {
				Toggle(
					"Move deleted files to system trash can",
					isOn: Binding(
						get: { folder.isTrashEnabled() },
						set: { enabled in
							do {
								try folder.setTrashEnabled(enabled)
							}
							catch {
								showError = ErrorMessage(error)
							}
						})
				)
				.errorAlert($showError)
			} footer: {
				Text(
					"When this is enabled, files synchronized on this device that are removed by remote peers, or are unpinned, will be moved to the system trash can instead of being deleted."
				)
			}
		}
	}

	@ViewBuilder private func folderGroupSection() -> some View {
		Section {
			LabeledContent {
				TextField(
					"",
					text: Binding(get: { folder.group() }, set: { lbl in try? folder.setGroup(lbl) }),
					prompt: Text("(Default group)")
				)
				.multilineTextAlignment(.trailing)
			} label: {
				Text("Folder group")
			}
		}
	}

	@ViewBuilder private func blockIndexSection() -> some View {
		Section {
			BlockIndexToggle(folder: folder)
				.errorAlert($showError)
		} footer: {
			Text(
				"When receiving files from other peers, the block index can be used to find pieces of a file that are already on the device, which reduced data transfer. However, the block index takes up space on the disk. It is recommended to only enable the block index when this folder contains files that are present in other folders as well, and/or appear multiple times."
			)
		}
	}

	@ViewBuilder private func conflictsSection() -> some View {
		if !folder.isReceiveEncryptedFolder && !folder.isPhotoFolder && !folder.isSendOnlyFolder {
			Section {
				Toggle(
					"Keep conflicting versions",
					isOn: Binding(
						get: {
							return folder.maxConflicts() != 0
						},
						set: { nv in
							try? folder.setMaxConflicts(nv ? -1 : 0)
						}))
			} footer: {
				Text(
					"When two or more devices change a file at the same time, one version will be chosen and kept. When this setting is enabled, the other versions will be stored alongside as conflict files. When disabled, only the chosen version remains on this device. See [the Syncthing documentation](https://docs.syncthing.net/users/config.html#config-option-folder.maxconflicts) for further information."
				)
			}
		}
	}

	@ViewBuilder private func watchSection() -> some View {
		if !folder.isReceiveEncryptedFolder {
			Section {
				LabeledContent {
					TextField(
						"",
						text: Binding(
							get: {
								let interval: Int = folder.rescanIntervalSeconds() / 60
								return "\(interval)"
							},
							set: { (lbl: String) in
								if !lbl.isEmpty {
									let interval = Int(lbl) ?? 0
									try? folder.setRescanInterval(interval * 60)
								}
							}), prompt: Text("")
					)
					.multilineTextAlignment(.trailing)
				} label: {
					Text("Rescan interval (minutes)")
				}
			} header: {
				Text("Change detection")
			} footer: {
				Text("Determines how often the app will look for changed files in synchronized folders on this device.")
			}
		}

		if !folder.isPhotoFolder && !folder.isReceiveOnlyFolder && !folder.isReceiveEncryptedFolder {
			Section {
				Toggle(
					"Watch for changes",
					isOn: Binding(get: { folder.isWatcherEnabled() }, set: { try? folder.setWatcherEnabled($0) }))

				if folder.isWatcherEnabled() {
					LabeledContent {
						TextField(
							"",
							text: Binding(
								get: {
									let interval: Int = folder.watcherDelaySeconds()
									return "\(interval)"
								},
								set: { (lbl: String) in
									if !lbl.isEmpty {
										let interval = Int(lbl) ?? 0
										try? folder.setWatcherDelaySeconds(interval)
									}
								}), prompt: Text("")
						)
						.multilineTextAlignment(.trailing)
					} label: {
						Text("Delay for processing changes (seconds)")
					}
				}
			}
		}
	}

	@ViewBuilder private func systemSettingsSection() -> some View {
		if !folder.isPhotoFolder {
			Section {
				Toggle(
					"Hide folder",
					isOn: Binding(
						get: {
							if let f = folder.isHidden { return f }
							return false
						},
						set: { nv in
							folder.isHidden = nv
						})
				).disabled(folder.isExternal != false)
			} header: {
				Text("System settings")
			} footer: {
				Text(
					"Hidden folders will not be visible by default in Finder, and will be hidden by default in the folders list in this app."
				)
			}
		}
	}
}

private struct BlockIndexToggle: View {
	let folder: SushitrainFolder

	@State private var indexEnabled: Bool? = nil
	@State private var showError: ErrorMessage? = nil

	var body: some View {
		Toggle(
			"Enable block index",
			isOn: Binding(
				get: {
					return self.indexEnabled == true
				},
				set: {
					setEnabled($0)
				})
		)
		.disabled(self.indexEnabled == nil)
		.errorAlert($showError)
		.task {
			self.update()
		}
	}

	private func setEnabled(_ enabled: Bool) {
		self.indexEnabled = enabled
		Task {
			do {
				try folder.setBlockIndexingEnabled(enabled)
				self.update()
			}
			catch {
				self.showError = ErrorMessage(error)
			}
		}
	}

	private func update() {
		self.indexEnabled = self.folder.isBlockIndexingEnabled()
	}
}
