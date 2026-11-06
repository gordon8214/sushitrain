// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit

import ServiceManagement

struct ConfigurationSettingsView: View {
	@Environment(AppState.self) private var appState
	@State private var showHomeDirectorySelector = false
	@State private var currentPath: URL? = nil
	@State private var showRestartAlert: Bool = false
	@State private var containerFolders = ContainerFolders()
	@State private var showSyncthingTakeover = false

	var body: some View {
		Form {
			Section {
				HStack {
					if let p = currentPath {
						Text(p.path(percentEncoded: false))
							.frame(maxWidth: .infinity, alignment: .leading)
							.multilineTextAlignment(.leading)
						Button(
							openInFilesAppLabel,
							systemImage: "arrow.up.forward.app",
							action: {
								openURLInSystemFilesApp(url: p)
							}
						).labelStyle(.iconOnly)
					}
					else {
						Text("(Default location)")
							.frame(maxWidth: .infinity, alignment: .leading)
							.multilineTextAlignment(.leading)
						Button(
							openInFilesAppLabel,
							systemImage: "arrow.up.forward.app",
							action: {
								let url = URL(
									fileURLWithPath: self.appState.client
										.currentConfigDirectory())
								openURLInSystemFilesApp(url: url)
							}
						).labelStyle(.iconOnly)
					}
				}

				Button("Select configuration folder...") {
					self.showHomeDirectorySelector = true
				}
				.buttonStyle(.link)

				if currentPath != nil {
					Button("Use default configuration location") {
						self.choose(nil)
					}
					.buttonStyle(.link)
				}
			} header: {
				Text("Configuration folder location")
			} footer: {
				Text(
					"The configuration folder contains the settings for the app, as well as the keys to communicate with other devices and bookkeeping of synchronized folders. By default, the configuration folder is managed by the app."
				)
			}

			ContainerFoldersSection(model: containerFolders)

			// Not after a takeover: the configuration is Syncthing's already, or a copy with its identity
			if SyncthingInstallation.hasConfiguration && EngineSetup.choice != .importSyncthing
				&& appState.configDirectory.standardizedFileURL.path
					!= SyncthingInstallation.home.standardizedFileURL.path
			{
				Section {
					Button("Take over Syncthing's setup...") {
						showSyncthingTakeover = true
					}
					.buttonStyle(.link)
				} header: {
					Text("Syncthing")
				} footer: {
					Text(
						"Synctrain can use the configuration of Syncthing.app on this Mac, keeping its device ID, folders and devices."
					)
				}
			}
		}
		.sheet(isPresented: $showSyncthingTakeover) {
			NavigationStack {
				SyncthingTakeoverView(decision: .offerTakeover, fromSettings: true)
					.toolbar {
						ToolbarItem(placement: .confirmationAction) {
							Button("Done") {
								showSyncthingTakeover = false
							}
						}
					}
			}
			.frame(minWidth: 520, minHeight: 560)
		}
		.formStyle(.grouped)
		.navigationTitle("Configuration settings")
		.fileImporter(isPresented: $showHomeDirectorySelector, allowedContentTypes: [.directory]) {
			result in
			switch result {
			case .success(let url):
				self.choose(url)
			case .failure(let err):
				Log.info("Failed to select home dir: \(err.localizedDescription)")
			}
		}
		.alert(
			"Configuration folder changed", isPresented: $showRestartAlert,
			actions: {
				if AppRelauncher.canRelaunch {
					Button("Restart the app") {
						AppRelauncher.relaunch()
					}
				}
				else {
					Button("Close the app") {
						AppRelauncher.quit()
					}
				}
			},
			message: {
				if AppRelauncher.canRelaunch {
					Text("The configuration folder was changed. The app restarts to use it.")
				}
				else {
					Text(
						"The path to the configuration folder was changed and will be used when the app is restarted. The app will now close."
					)
				}
			}
		)
		.task {
			self.updatePath()
			await containerFolders.update(appState)
		}
	}

	/// Remembers the chosen folder (nil for the default location): outside the sandbox by its path, in the sandbox
	/// with a security-scoped bookmark
	private func choose(_ url: URL?) {
		if FolderAccess.usesSecurityScope {
			var bookmark: Data? = nil
			if let url {
				_ = url.startAccessingSecurityScopedResource()
				guard let data = try? url.bookmarkData(options: [.withSecurityScope]) else {
					return
				}
				bookmark = data
				self.keepProxy(in: url)
			}
			UserDefaults.standard.setValue(bookmark, forKey: ConfigDirectorySetting.bookmarkKey)
		}
		else {
			self.keepProxy(in: url ?? EngineLocations.current.configDirectory)
			ConfigDirectorySetting.choose(url)
		}
		self.updatePath()
		self.showRestartAlert = true
	}

	/// The proxy is kept with the configuration, and must not end because the configuration moved
	private func keepProxy(in folder: URL) {
		do {
			try appState.client.copyProxySettings(folder.path(percentEncoded: false))
		}
		catch {
			Log.warn("Could not copy the proxy settings to \(folder): \(error.localizedDescription)")
		}
	}

	private func updatePath() {
		if !FolderAccess.usesSecurityScope {
			self.currentPath = ConfigDirectorySetting.path.map { URL(fileURLWithPath: $0, isDirectory: true) }
			return
		}
		var isStale: Bool = false
		if let bd = UserDefaults.standard.data(forKey: ConfigDirectorySetting.bookmarkKey),
			let url = try? URL(
				resolvingBookmarkData: bd, options: [.withSecurityScope, .withoutUI],
				bookmarkDataIsStale: &isStale), !isStale
		{
			self.currentPath = url
		}
		else {
			self.currentPath = nil
		}
	}
}

struct AsyncAddressesView: View {
	@Environment(AppState.self) var appState

	let addressType: AddressType

	@State private var loading = true
	@State private var addresses: [String] = []

	var body: some View {
		ZStack {
			if self.loading {
				ProgressView()
			}
			else {
				AddressesView(
					addresses: addresses,
					onChange: {
						if !self.loading {
							self.addresses = $0
							self.write()
						}
					},
					addressType: self.addressType
				)
			}
		}
		.task {
			await self.update()
		}
		.onDisappear {
			self.write()
		}
	}

	private func update() async {
		self.loading = true
		let client = self.appState.client
		let addressType = self.addressType
		self.addresses = await Task.detached {
			switch addressType {
			case .discovery:
				return client.discoveryAddresses()?.asArray() ?? []
			case .listening:
				return client.listenAddresses()?.asArray() ?? []
			case .device:
				// not supported
				return []
			case .stun:
				return client.stunAddresses()?.asArray() ?? []
			}
		}.value
		self.addresses.sort()
		Log.info("Loaded addresses \(self.addresses)")
		self.loading = false
	}

	private func write() {
		let addresses = self.addresses
		let appState = self.appState
		Task.detached {
			do {
				switch addressType {
				case .discovery:
					try appState.client.setDiscoveryAddresses(
						SushitrainListOfStrings.from(addresses))
				case .listening:
					Log.info("Writing listening addresses: \(addresses)")
					try appState.client.setListenAddresses(SushitrainListOfStrings.from(addresses))
				case .device:
					// not supported
					abort()
				case .stun:
					try appState.client.setStunAddresses(
						SushitrainListOfStrings.from(addresses))
				}
			}
			catch {
				Log.warn("Failed to write \(self.addressType) addresses: \(error)")
			}
		}
	}
}

struct AutoStartToggleView: View {
	@State private var status = SMAppService.Status.notRegistered
	@State private var errorMessage: ErrorMessage? = nil

	var body: some View {
		Toggle(isOn: Binding(get: { status == .enabled }, set: { self.set($0) })) {
			Label("Launch at login", systemImage: "autostartstop")
		}
		.settingsAnchor("launchAtLogin")
		.onAppear {
			self.update()
		}
		// System Settings tells the app nothing when the user changes this there
		.onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
			self.update()
		}
		.errorAlert($errorMessage)

		if status == .requiresApproval {
			SettingsWarning(
				text: "macOS keeps Synctrain from opening at login. Allow it in System Settings, under Login Items & Extensions.")
			Button("Open System Settings…") {
				SMAppService.openSystemSettingsLoginItems()
			}
		}
	}

	private func set(_ enabled: Bool) {
		do {
			if enabled {
				try SMAppService.mainApp.register()
			}
			else {
				try SMAppService.mainApp.unregister()
			}
		}
		catch {
			Log.warn("Could not change Launch at login to \(enabled): \(error)")
			// Only the user can change this one, and the warning says how
			if SMAppService.mainApp.status != .requiresApproval {
				self.errorMessage = ErrorMessage(error)
			}
		}
		self.update()
	}

	private func update() {
		self.status = SMAppService.mainApp.status
		CrashWatch.shared.update()
	}
}
