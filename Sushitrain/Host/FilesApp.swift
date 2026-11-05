// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import AppIntents
	import SwiftUI
	import SynctrainFileProviderKit
	import SynctrainHostKit

	/// The app in Files in Finder mode, and at a first start until a mode is chosen. No engine runs: this keeps track of
	/// gateways for the File Provider extension, which browsing and opening files need nothing else for. Edits it could
	/// not finish wait for this app, which macOS gives no time in the background of its own, so it stays around in the
	/// menu bar or the Dock when its window closes. A plain `Window` scene would quit it instead.
	struct FilesApp: App {
		@NSApplicationDelegateAdaptor(FilesAppDelegate.self) private var delegate
		@Environment(\.scenePhase) private var scenePhase
		@Environment(\.openWindow) private var openWindow
		@AppStorage(AppPresence.storageKey) private var presence = AppPresence.standard
		@State private var mode: AppMode?
		@State private var model: AppModel

		/// The one window there is: opening a group by value shows the window that has it, where by id alone it adds one
		private static let window = "files"

		init() {
			AppLocations.prepareCurrent()
			LegacyPhotoBackupPreferences.remove(from: .standard)
			AppPresence.adoptHideInDock(in: .standard)
			CrashWatch.shared.logLastRun()
			let mode = AppMode.resolve()
			let model = AppModel()
			MCPIntegration.shared.start()
			_mode = State(initialValue: mode)
			_model = State(initialValue: model)
			if mode != nil {
				Task { await model.start() }
			}
			// The system may ask for the app's state before an action's own check (`AppMode.requireEngine`)
			AppDependencyManager.shared.add { () async throws -> AppState in
				throw EngineNotRunning()
			}
			let activationPolicy = self.presence.activationPolicy
			DispatchQueue.main.async {
				NSApp.setActivationPolicy(activationPolicy)
			}
		}

		var body: some Scene {
			WindowGroup(id: Self.window, for: String.self) { _ in
				FilesWindowContent(model: model, mode: $mode, presence: $presence)
			} defaultValue: {
				Self.window
			}
			.windowToolbarStyle(.unified)
			.defaultSize(width: 1000, height: 720)
			.windowResizability(.contentMinSize)
			.commandsRemoved()
			.commands {
				CommandGroup(replacing: .appInfo) {
					Button("About Synctrain") {
						openWindow(id: "about")
					}
				}
			}
			.restorationBehavior(.disabled)
			.defaultLaunchBehavior(presence.inDock ? .presented : .suppressed)
			.onChange(of: scenePhase, initial: true) { _, _ in
				// Retain the scene's action once, for the delegate
				if delegate.openWindow == nil {
					delegate.openWindow = { [openWindow] in openWindow(id: Self.window, value: Self.window) }
				}
			}
			.onChange(of: presence.activationPolicy) { _, policy in
				NSApp.setActivationPolicy(policy)
				NSApp.activate()
			}

			Window("About Synctrain", id: "about") {
				AboutView(showsIntroduction: false)
			}
			.windowResizability(.contentSize)
			.restorationBehavior(.disabled)
			.defaultLaunchBehavior(.suppressed)

			MenuBarExtra("Synctrain", systemImage: "folder", isInserted: $presence.inMenuBar) {
				CrashNoticeMenuItems()
				Button("Open Synctrain", systemImage: "macwindow") {
					AppWindowPresentation.open {
						openWindow(id: Self.window, value: Self.window)
					}
				}
				Button("Show in Finder", systemImage: "folder") {
					showLocation()
				}
				Button("About...", systemImage: "info.circle") {
					AppWindowPresentation.open {
						openWindow(id: "about")
					}
				}
				Divider()
				Toggle(isOn: $presence.inDock) {
					Label("Show in Dock", systemImage: "dock.rectangle")
				}
				Button("Quit Synctrain", systemImage: "multiply.circle") {
					NSApp.terminate(nil)
				}
			}
		}

		private func showLocation() {
			Task {
				if let url = await FilesDomain.shared.locationURL() {
					ClientLifecycle.show(url)
				}
			}
		}
	}

	/// Files reach the app from document types and the Share extension, for the folders of the engine
	@MainActor final class FilesAppDelegate: NSObject, NSApplicationDelegate {
		var openWindow: (() -> Void)?

		func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
			AppPresence.reopen(hasVisibleWindows: hasVisibleWindows) { openWindow?() }
		}

		func applicationDidResignActive(_ notification: Notification) {
			AppPresence.restoreActivationPolicy()
		}

		func applicationWillTerminate(_ notification: Notification) {
			CrashWatch.shared.stop()
		}

		func application(_ application: NSApplication, open urls: [URL]) {
			let alert = NSAlert()
			alert.messageText = String(localized: "Synctrain cannot import files in Files in Finder mode")
			alert.informativeText = String(localized: "Put them in a folder in Finder's Synctrain location instead.")
			alert.runModal()
			for handoff in Set(urls.compactMap({ AppLocations.current.shareHandoffDirectory(containing: $0) })) {
				try? FileManager.default.removeItem(at: handoff)
			}
		}
	}

	/// The app's window in this mode: choosing a mode, or the gateways and what goes with them
	private struct FilesWindowContent: View {
		let model: AppModel
		@Binding var mode: AppMode?
		@Binding var presence: AppPresence

		var body: some View {
			Group {
				if mode == nil {
					ModeChooserView(choose: choose)
				}
				else {
					switch model.phase {
					case .unavailable:
						ContentUnavailableView {
							Label("Synctrain cannot start", systemImage: "exclamationmark.triangle")
						} description: {
							Text("The storage it shares with Finder is not available.")
						} actions: {
							SwitchToSynchronizeButton(keepsLocation: true)
						}
					case .loading:
						ProgressView()
					case .loadFailed(let message):
						ContentUnavailableView {
							Label("Synctrain cannot start", systemImage: "exclamationmark.triangle")
						} description: {
							Text(message)
						} actions: {
							Button("Try Again") {
								Task { await model.start() }
							}
							SwitchToSynchronizeButton(keepsLocation: true)
						}
					case .needsGateway, .needsFolders, .ready:
						FilesSettingsView(presence: $presence)
					}
				}
			}
			.frame(minWidth: 480, minHeight: 420)
			.filesLeftoversAlert()
		}

		private func choose(_ chosen: AppMode) {
			AppMode.choose(chosen)
			if chosen == .filesInFinder {
				mode = chosen
				Task { await model.start() }
			}
			else {
				AppRelauncher.relaunch()
			}
		}

	}

	/// Leaves Files in Finder mode (`ModeSwitch`), having said what happens to changes that didn't reach Synctrain. With
	/// `keepsLocation` nothing is taken apart, as nothing can be read to do that by.
	struct SwitchToSynchronizeButton: View {
		let keepsLocation: Bool
		private var coordinator = RemoteCoordinator.shared
		@State private var isConfirming = false
		@State private var isWorking = false
		@State private var pendingItemCount: Int? = 0
		@State private var errorMessage: String? = nil

		init(keepsLocation: Bool = false) {
			self.keepsLocation = keepsLocation
		}

		var body: some View {
			Button("Switch to Synchronize Mode…") {
				Task {
					// Without a gateway there is no location to ask
					let hasNoLocation = keepsLocation || coordinator.gateways.isEmpty
					pendingItemCount = hasNoLocation ? 0 : await FilesDomain.shared.pendingItemCount()
					isConfirming = true
				}
			}
			.disabled(isWorking)
			.confirmationDialog(
				"Switch to Synchronize mode?", isPresented: $isConfirming, titleVisibility: .visible
			) {
				Button(pendingItemCount == 0 ? "Switch" : "Switch Anyway", role: .destructive) {
					leave()
				}
			} message: {
				if keepsLocation {
					Text(
						"Synctrain could not read its gateways, so Finder's Synctrain location stays as it is. Synctrain then restarts and synchronizes the folders of this Mac. Switch back to Files in Finder to remove the location."
					)
				}
				else if let count = pendingItemCount, count > 0 {
					Text(
						"Changes to \(count) items made in Finder have not reached Synctrain yet. macOS keeps them when the location goes, outside of Finder's Synctrain location."
					)
				}
				else if pendingItemCount == nil {
					Text(
						"Synctrain could not find out whether changes made in Finder have reached it. macOS keeps any that have not when the location goes, outside of Finder's Synctrain location."
					)
				}
				else {
					Text(
						"Every folder leaves Finder and stays on its gateway. Synctrain then restarts and synchronizes the folders of this Mac."
					)
				}
			}
			.alert("Could not switch", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage) { _ in
				Button("OK") {}
			} message: { message in
				Text(message)
			}
		}

		private func leave() {
			if keepsLocation {
				ModeSwitch.abandonFilesInFinder()
				return
			}
			isWorking = true
			Task {
				defer { isWorking = false }
				do {
					try await ModeSwitch.leaveFilesInFinder()
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}

	/// The first thing on a Mac that never ran the engine, as starting one would give this Mac a device identity and
	/// announce it, which Files in Finder has no use for
	struct ModeChooserView: View {
		let choose: (AppMode) -> Void

		var body: some View {
			VStack(spacing: 16) {
				Image("Logo")
				Text("How do you want to use Synctrain on this Mac?").font(.headline)
				choice(
					.synchronize, image: "arrow.triangle.2.circlepath", title: "Synchronize folders",
					description: "Every folder is kept in full on this Mac, and synchronized with your other devices.")
				choice(
					.filesInFinder, image: "folder.badge.gearshape", title: "Files in Finder",
					description:
						"Folders stay on a gateway on another system, which has all their files. Finder shows them, and keeps on this Mac what you open."
				)
				Text("You can change this later.").font(.footnote).foregroundStyle(.secondary)
			}
			.padding()
		}

		private func choice(
			_ mode: AppMode, image: String, title: LocalizedStringKey, description: LocalizedStringKey
		) -> some View {
			Button {
				choose(mode)
			} label: {
				HStack(alignment: .top, spacing: 12) {
					Image(systemName: image).font(.title).foregroundStyle(.tint).frame(width: 36)
					VStack(alignment: .leading, spacing: 4) {
						Text(title).font(.headline)
						Text(description).font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.leading)
					}
					Spacer(minLength: 0)
				}
				.padding(.vertical, 6)
				.contentShape(.rect)
			}
			.buttonStyle(.bordered)
		}
	}
#endif
