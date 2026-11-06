// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore
#if os(macOS)
	import SynctrainHostKit
#endif

private struct OverallDownloadProgressView: View {
	@Environment(AppState.self) private var appState
	@State private var lastProgress: (Date, SushitrainProgress)? = nil
	@State private var progress: (Date, SushitrainProgress)? = nil
	@State private var peerStatusText: String = ""

	var body: some View {
		Group {
			if let (date, progress) = progress {
				NavigationLink(destination: DownloadsView()) {
					ProgressView(value: progress.percentage, total: 1.0) {
						VStack {
							HStack {
								Label(
									"Receiving \(progress.filesTotal) files...",
									systemImage: "arrow.down"
								)
								.foregroundStyle(.green)
								.symbolEffect(.pulse, value: date)
								.badge(localFormattedPercentage(Double(progress.percentage)))
								.frame(maxWidth: .infinity)
								Spacer()
							}

							self.speeds
						}
					}
					.tint(.green)
				}
			}
			else {
				Label("Receiving files...", systemImage: "arrow.down")
					.foregroundStyle(.green)
					.badge(self.peerStatusText)
					.frame(maxWidth: .infinity)
			}
		}
		.task(id: self.appState.eventCounter) {
			await self.updateProgress()
		}
	}

	@ViewBuilder private var speeds: some View {
		// Download speed
		HStack {
			Spacer()
			if let (date, progress) = progress, let (lastDate, lastProgress) = self.lastProgress {
				let diffBytes = progress.bytesDone - lastProgress.bytesDone
				let diffTime = date.timeIntervalSince(lastDate)
				let speed = Int64(Double(diffBytes) / Double(diffTime))
				let formatter = ByteCountFormatter()

				if speed > 0 && diffTime > 0 {
					Text("\(formatter.string(fromByteCount: speed))/s")
						.foregroundStyle(.green)
					let secondsToGo = Duration(
						secondsComponent: (progress.bytesTotal - progress.bytesDone) / speed, attosecondsComponent: 0)
					let secondsToGoFormatted: String = secondsToGo.formatted(.time(pattern: .hourMinuteSecond(padHourToLength: 0)))
					Text(secondsToGoFormatted).foregroundStyle(.green)
				}
				else {
					Text("(Transfer speed unknown)").foregroundStyle(.gray)
				}
			}
		}
	}

	private func updateProgress() async {
		self.peerStatusText = "\(self.appState.client.connectedPeerCount())/\(await self.appState.peers().count - 1)"

		self.lastProgress = self.progress
		if let p = self.appState.client.getTotalDownloadProgress() {
			self.progress = (Date.now, p)
		}
		else {
			self.progress = nil
		}
	}
}

private struct NetworkStatusView: View {
	@Environment(AppState.self) private var appState
	// shouldDisableConnectivity reads this ObservableObject through AppState.
	// Observe it directly so settings changes also refresh this view.
	@ObservedObject var userSettings: AppUserSettings
	@State private var showInfo: Bool = false

	private var isSlowConnection: Bool {
		if #available(iOS 26, macOS 26, *) {
			return appState.currentNetworkPath?.linkQuality == .minimal
		}
		return false
	}

	private var isLimitedConnection: Bool {
		guard let path = appState.currentNetworkPath else { return false }
		if path.isConstrained || path.isExpensive { return true }
		if #available(iOS 26, macOS 26, *) {
			return path.isUltraConstrained
		}
		return false
	}

	var body: some View {
		Group {
			if appState.shouldDisableConnectivity {
				Label("Network connectivity temporarily disabled", systemImage: "antenna.radiowaves.left.and.right")
					.foregroundStyle(.orange)
			}
			else if let path = appState.currentNetworkPath {
				if path.status == .satisfied {
					if isSlowConnection {
						Label("Slow internet connection", systemImage: "antenna.radiowaves.left.and.right").foregroundStyle(.orange)
					}
					else if isLimitedConnection {
						Label("Limited internet connection", systemImage: "antenna.radiowaves.left.and.right").foregroundStyle(.orange)
					}
				}
				else if path.status == .unsatisfied {
					Label("No internet connection available", systemImage: "antenna.radiowaves.left.and.right.slash").foregroundStyle(
						.red)
				}
			}
		}.onTapGesture {
			self.showInfo = true
		}
		.alert(isPresented: $showInfo) {
			if appState.shouldDisableConnectivity {
				Alert(
					title: Text("Network connectivity temporarily disabled"),
					message: Text(
						"Connections to other devices have been temporarily disabled, because you have configured Synctrain to not use the type of network connection that is currently available, and/or to disable connectivity when in low power mode."
					)
				)
			}
			else {
				Alert(
					title: Text("Internet connection quality"),
					message: Text(
						"The internet connection status is determined by the system. When the internet connection is limited or degraded, file synchronization and streaming over the internet may be slower than usual, or not work at all. File streaming and synchronization with devices that are on the same local network is expected to work as usual."
					))
			}
		}
	}
}

struct OverallStatusView: View {
	@Environment(AppState.self) private var appState
	@State private var connectedPeerCount = 0
	@State private var peerCount = 0
	@State private var isDownloading = false
	@State private var isUploading = false

	var body: some View {
		Group {
			if self.connectedPeerCount > 0 {
				if isDownloading || isUploading {
					if isDownloading {
						OverallDownloadProgressView()
					}

					// Uploads
					if isUploading {
						NavigationLink(destination: UploadsView()) {
							OverallUploadStatusView()
						}
					}
				}
				else {
					Label("Connected", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
						.badge(Text(peerStatusText))
				}
			}
			else {
				Label("Not connected", systemImage: "network.slash")
					.badge(Text(self.peerStatusText))
					.foregroundColor(.gray)
			}
		}
		.task(id: appState.eventCounter) {
			await self.update()
		}
	}

	private var peerStatusText: String {
		if peerCount > 0 {
			return "\(connectedPeerCount)/\(peerCount - 1)"
		}
		return ""
	}

	private func update() async {
		self.isDownloading = self.appState.client.isDownloading()
		self.isUploading = self.appState.client.isUploading()
		self.connectedPeerCount = self.appState.client.connectedPeerCount()
		self.peerCount = await self.appState.peers().count
	}
}

private struct OverallUploadStatusView: View {
	@Environment(AppState.self) private var appState

	@State private var progress: SushitrainProgress? = nil

	var body: some View {
		ZStack {
			if let progress = progress {
				ProgressView(value: progress.percentage, total: 1.0) {
					Label(
						"Sending \(progress.filesTotal) files...",
						systemImage: "arrow.up"
					)
					.foregroundStyle(.green)
					.symbolEffect(.pulse, value: progress.percentage)
					.badge(localFormattedPercentage(Double(progress.percentage)))
					.frame(maxWidth: .infinity)
				}.tint(.green)
			}
			else {
				Label("Sending files...", systemImage: "arrow.up")
					.foregroundStyle(.green)
					.frame(maxWidth: .infinity)
			}
		}.task {
			await self.update()
		}
	}

	private func update() async {
		let client = appState.client
		self.progress = await Task.detached {
			return client.getTotalUploadProgress()
		}.value
	}
}

struct StartView: View {
	@Environment(AppState.self) private var appState
	@Environment(\.navigateTo) private var navigateTo
	@Binding var topLevelRoute: Route?

	@ObservedObject var userSettings: AppUserSettings

	@Environment(\.openWindow) private var openWindow

	@State private var qrCodeShown = false

	@State private var showAddresses = false
	@State private var showAddFolderSheet = false

	@State private var addingFolder = AddFolder()
	@State private var addingDeviceID = ""
	@State private var addingDevice = false

	@State private var showNoPeersEnabledWarning = false
	@State private var peers: [SushitrainPeer]? = nil
	@State private var folders: [SushitrainFolder]? = nil

	@State private var inaccessibleExternalFolders: [SushitrainFolder] = []
	@State private var foldersWithIssues: [SushitrainFolder] = []
	@State private var fixingInaccessibleExternalFolder: SushitrainFolder? = nil
	@State private var isDiskSpaceSufficient = true
	@State private var longTimeNotSeenDevices: [SushitrainPeer] = []
	@State private var changesCount = 0
	@State private var showMaintenance = false
	@State private var lastDatabaseMaintenance: Date? = nil
	#if os(macOS)
		@State private var totals: SushitrainFolderStats?
	#endif

	var body: some View {
		Form {
			#if os(macOS)
				ForEach(DesktopUICatalog.desktop.destinations.first { $0.id == "overview" }?.sections ?? [], id: \.self) { id in
					desktopSection(id)
				}
			#else

				Section {
					OverallStatusView()
						.contextMenu {
							Button("Statistics...") {
								openWindow(id: "stats")
							}
						}

					NetworkStatusView(userSettings: userSettings)
				}

				#if os(macOS)
					CrashNoticeSection()
				#endif

				Section(header: Text("This device's identifier")) {
					DeviceIDView(device: self.appState.client.peer(withID: self.appState.localDeviceID)!)
				}

				// Disk space warning
				if !isDiskSpaceSufficient {
					Section {
						DiskSpaceWarningView()
					}
				}

				// Getting started, device issues
				if let p = peers, p.isEmpty {
					self.gettingStartedDevices()
				}
				self.deviceIssuesSection()

				// Getting started, folder issues
				if let f = folders, f.isEmpty {
					self.gettingStartedFolders()
				}
				self.folderIssuesSection()

				self.maintenanceIssuesSection()

				Section("Manage files and folders") {
					NavigationLink(destination: ChangesView(userSettings: appState.userSettings)) {
						Label("Recent changes", systemImage: "clock.arrow.2.circlepath").badge(changesCount)
					}.disabled(changesCount == 0)
				}

				self.bookmarksSection()
			#endif
		}
		.formStyle(.grouped)
		.navigationTitle("Overview")
		.sheet(isPresented: Binding.isNotNil($fixingInaccessibleExternalFolder)) {
			if let fe = self.fixingInaccessibleExternalFolder {
				NavigationStack {
					ExternalFolderInaccessibleView(folder: fe)
						.navigationTitle(fe.displayName)
						.frame(minHeight: 300)
						.toolbar {
							SheetButton(role: .done) {
								self.fixingInaccessibleExternalFolder = nil
							}
						}
				}
			}
		}
		.sheet(isPresented: $showMaintenance) {
			MaintenanceView()
		}
		.task(id: appState.eventCounter) {
			await self.update()
		}
		.onChange(of: appState.userSettings.ignoreLongTimeNoSeeDevices.count) { _, _ in
			Task {
				await self.update()
			}
		}
		.onChange(of: showMaintenance) {
			Task {
				await self.update()
			}
		}
	}

	#if os(macOS)
		@ViewBuilder private func desktopSection(_ id: String) -> some View {
			switch id {
			case "status":
				Section("Status") {
					OverallStatusView().contextMenu { Button("Statistics…") { openWindow(id: "stats") } }
					NetworkStatusView(userSettings: userSettings)
					DisclosureGroup("This Device’s Identifier") {
						if let device = appState.client.peer(withID: appState.localDeviceID) { DeviceIDView(device: device) }
					}
				}
			case "attention":
				Section("Needs Attention") {
					if isDiskSpaceSufficient && inaccessibleExternalFolders.isEmpty && foldersWithIssues.isEmpty
						&& longTimeNotSeenDevices.isEmpty && !showNoPeersEnabledWarning
					{
						Text("No synchronization problems reported.")
					}
					if !isDiskSpaceSufficient { DiskSpaceWarningView() }
				}
				CrashNoticeSection()
				if let peers, peers.isEmpty { gettingStartedDevices() }
				deviceIssuesSection()
				if let folders, folders.isEmpty { gettingStartedFolders() }
				folderIssuesSection()
				maintenanceIssuesSection()
			case "summary":
				Section("Synchronization") {
					ForEach(folders ?? [], id: \.folderID) { folder in
						if let url = folder.localNativeURL {
							Button("Open \(folder.displayName) in Finder", systemImage: "folder") { openURLInSystemFilesApp(url: url) }
						}
					}
					Text("\(folders?.count ?? 0) folders · \(peers?.filter { !$0.isSelf() }.count ?? 0) devices")
					NavigationLink("Downloads") { DownloadsView() }
					NavigationLink("Uploads") { UploadsView() }
					NavigationLink(destination: ChangesView(userSettings: appState.userSettings)) {
						Label("Recent Changes", systemImage: "clock.arrow.2.circlepath").badge(changesCount)
					}.disabled(changesCount == 0)
				}
				bookmarksSection()
			case "storage":
				Section("Storage") {
					if let local = totals?.local {
						LabeledContent("Local files", value: local.files.formatted())
						LabeledContent("Local storage", value: local.bytes.formatted(.byteCount(style: .file)))
					}
					else {
						Text("Storage information is unavailable while the engine is starting.")
					}
				}
			default: EmptyView()
			}
		}
	#endif

	@ViewBuilder private func bookmarksSection() -> some View {
		if !self.userSettings.bookmarkedRoutes.isEmpty {
			Section("Bookmarks") {
				ForEach(self.appState.bookmarkedRoutesAsRoute, id: \.url.absoluteString) { bookmark in
					Button(bookmark.localizedTitle, systemImage: "bookmark.fill") {
						self.navigateTo(bookmark)
					}.contextMenu {
						if case .folder(folderID: let folderID, prefix: let prefix) = bookmark {
							// Rescan (sub)directory
							Button("Rescan subdirectory") {
								if let folder = self.appState.client.folder(withID: folderID) {
									try? folder.rescanSubdirectory(prefix)
								}
							}

							Divider()
						}

						Button("Remove bookmark", role: .destructive) {
							self.userSettings.bookmarkedRoutes.removeAll(where: { bookmarkedURL in
								guard let bookmarkedRoute = Route(url: bookmarkedURL) else {
									return true
								}

								return bookmarkedRoute == bookmark
							})
						}
					}
					.buttonStyle(.link)
				}
			}
		}
	}

	@ViewBuilder private func gettingStartedFolders() -> some View {
		Section("Getting started") {
			VStack(alignment: .leading, spacing: 10) {
				Label("Add your first folder", systemImage: "folder.badge.plus").bold()
				Text(
					"To synchronize files, add a folder. Folders that have the same folder ID on multiple devices will be synchronized with eachother."
				)
				Button("Add folder...", systemImage: "plus") {
					addingFolder = AddFolder()
					showAddFolderSheet = true
				}
				.padding([.bottom], 5)
				.buttonStyle(.link)
			}
			.sheet(
				isPresented: $showAddFolderSheet,
				onDismiss: {
					Task {
						await self.update()
					}
				}
			) {
				NavigationStack {
					AddFolderView(adding: addingFolder, shown: $showAddFolderSheet)
				}
			}
		}
	}

	@ViewBuilder private func gettingStartedDevices() -> some View {
		Section("Getting started") {
			VStack(alignment: .leading, spacing: 10) {
				Label("Add your first device", systemImage: "externaldrive.badge.plus")
					.bold()
				Text(
					"To synchronize files, first add a remote device. Either select a device from the list below, or add manually using the device ID."
				)
				Button("Add device...", systemImage: "plus") {
					addingDeviceID = ""
					addingDevice = true
				}
				.padding([.bottom], 5)
				.buttonStyle(.link)
			}
			.sheet(
				isPresented: $addingDevice,
				onDismiss: {
					Task {
						await self.update()
					}
				}
			) {
				NavigationStack {
					AddDeviceView(suggestedDeviceID: $addingDeviceID, shown: $addingDevice)
				}
			}
		}
	}

	@ViewBuilder private func maintenanceIssuesSection() -> some View {
		if self.appState.maintenanceManager.isDatabaseMaintenanceRequired(warning: true) {
			Section {
				Button("Perform database maintenance", systemImage: "bubbles.and.sparkles") {
					showMaintenance = true
				}
				.buttonStyle(.link)
			} header: {
				Text("Database maintenance")
			} footer: {
				if let lastDatabaseMaintenance = lastDatabaseMaintenance {
					Text(
						"Every once in a while, Synctrain needs to clean up its internal database. The last database maintenance was performed at \(lastDatabaseMaintenance.formatted(date: .abbreviated, time: .standard)). Tap the above button to start maintenance now."
					)
				}
				else {
					Text(
						"Every once in a while, Synctrain needs to clean up its internal database. No database maintenance has been performed yet. Tap the above button to start maintenance now."
					)
				}
			}
		}
	}

	@ViewBuilder private func deviceIssuesSection() -> some View {
		if showNoPeersEnabledWarning || !longTimeNotSeenDevices.isEmpty || !appState.pendingDevices.isEmpty {
			Section("Devices that need your attention") {
				// Devices that asked to connect
				// In a window of its own: this row goes as soon as the last request does, which would close what it opened
				if !appState.pendingDevices.isEmpty {
					Button {
						openWindow(id: "connectionRequests")
					} label: {
						Label(
							"Devices that want to connect: \(appState.pendingDevices.count)",
							systemImage: "person.crop.circle.badge.plus"
						)
						.foregroundStyle(.orange)
					}
					.buttonStyle(.link)
				}

				// All devices are disabled
				if showNoPeersEnabledWarning {
					VStack(alignment: .leading, spacing: 5) {
						Label(
							"All devices are paused",
							systemImage: "exclamationmark.triangle.fill"
						)
						.bold()
						.foregroundStyle(.orange)
						Text(
							"Synchronization is disabled for all associated devices. To restart synchronization, re-enable synchronization on the 'devices' page, or tap here to enable all devices."
						)
						.foregroundStyle(.orange)
					}
					.onTapGesture {
						Task {
							Log.info("Attempting to fix 'no devices are connected'")
							self.appState.userSettings.userPausedDevices.removeAll()
							Log.info("Updating device suspension")
							await self.appState.updateDeviceSuspension()
							showNoPeersEnabledWarning = false
							Log.info("Done fixing 'no devices are connected'")
						}
					}
				}

				// Devices not seen for a while
				if !longTimeNotSeenDevices.isEmpty {
					ForEach(longTimeNotSeenDevices, id: \.id) { device in
						if let lastSeen = device.lastSeen()?.date() {
							NavigationLink(destination: DeviceView(device: device)) {
								Label(
									"Device '\(device.displayName)' has not connected since \(lastSeen.formatted())",
									systemImage: "exclamationmark.triangle.fill"
								)
								.foregroundStyle(.orange)
							}.contextMenu {
								Button("Ignore for this device") {
									appState.userSettings.ignoreLongTimeNoSeeDevices.insert(device.deviceID())
									Task {
										await self.update()
									}
								}
							}
						}
					}
				}
			}
		}
	}

	@ViewBuilder private func folderIssuesSection() -> some View {
		if !foldersWithIssues.isEmpty || !inaccessibleExternalFolders.isEmpty {
			Section("Folders that need your attention") {
				// External folders that have become inaccessible
				ForEach(inaccessibleExternalFolders, id: \.folderID) { folder in
					Button(action: {
						self.fixingInaccessibleExternalFolder = folder
					}) {
						Label(
							"Folder '\(folder.displayName)' is not accessible anymore",
							systemImage: "xmark.app"
						)
						.foregroundStyle(.red)
					}
					.buttonStyle(.link)
				}

				// Folders with other configuration issues
				ForEach(foldersWithIssues, id: \.folderID) { folder in
					NavigationLink(destination: {
						FolderView(folder: folder)
					}) {
						let issue = folder.issue ?? String(localized: "unknown error")
						Label(
							"Folder '\(folder.displayName)' has an issue: \(issue)",
							systemImage: "exclamationmark.triangle.fill"
						)
						.foregroundStyle(.red)
					}
				}
			}
		}
	}

	private func update() async {
		#if os(macOS)
			let client = appState.client
			totals = await Task.detached(priority: .utility) { try? client.statistics() }.value
		#endif
		self.lastDatabaseMaintenance = appState.client.lastMaintenanceTime()?.date()
		await self.updateFoldersWithIssues()

		// Check to see if there are peers connected
		let p = await self.appState.peers()
		self.peers = p
		self.folders = await self.appState.folders().sorted()
		self.longTimeNotSeenDevices = await self.appState.getPeersNotSeenForALongTime()

		isDiskSpaceSufficient = appState.client.isDiskSpaceSufficient()

		// Determine number of changes
		self.changesCount = appState.lastChanges.count(where: { change in
			guard let folder = appState.client.folder(withID: change.folderID) else {
				return false
			}

			if appState.userSettings.hideHiddenFolders && folder.isHidden == true {
				return false
			}

			guard let entry = try? folder.getFileInformation(change.path) else {
				return false
			}
			return !entry.isDirectory()
		})

		await self.appState.updateBadge()
		let enabledPeerCount = p.count { !$0.isSelf() && !appState.userSettings.userPausedDevices.contains($0.deviceID()) }
		showNoPeersEnabledWarning = p.count > 1 && enabledPeerCount == 0
	}

	private func updateFoldersWithIssues() async {
		let folders = await self.appState.folders()

		// Find external folders that have become inaccessible
		self.inaccessibleExternalFolders = folders.filter { folder in
			if folder.isPaused() {
				return false
			}

			return folder.isExternal == true
				&& !BookmarkManager.shared.hasBookmarkFor(folderID: folder.folderID)
		}

		// Find folders with other issues
		self.foldersWithIssues = folders.filter { folder in
			if folder.isPaused() {
				return false
			}
			if !SushitrainFolder.knownGoodStates.contains(folder.state()) {
				// Do not list inaccessible external folders twice
				return !self.inaccessibleExternalFolders.contains(where: { $0.folderID == folder.folderID })
			}
			return false
		}
	}
}

private struct DiskSpaceWarningView: View {
	@State private var diskSpaceFree: Int64? = nil
	@State private var formatter = ByteCountFormatter()

	var body: some View {
		VStack(alignment: .leading, spacing: 5) {
			Label("Insufficient storage space", systemImage: "externaldrive.fill.badge.exclamationmark")
				.bold()
				.foregroundStyle(.red)
			if let b = diskSpaceFree {
				Text("There is only \(formatter.string(fromByteCount: b)) of free storage space left on this device.")
					.foregroundStyle(.red)
			}
			else {
				Text("There is little to no free storage space left on this device.")
					.foregroundStyle(.red)
			}
			Text(
				"To prevent issues, synchronization is temporarily disabled. To resume synchronization, free up space on the device by removing files. If there is space available, empty the trash can, restart the app and the device."
			)
			.foregroundStyle(.red)
		}.task {
			self.update()
		}
	}

	private func update() {
		self.diskSpaceFree = Int64(SushitrainGetFreeDiskSpaceMegaBytes()) * 1024 * 1024
	}
}
