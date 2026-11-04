// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI
	@preconcurrency import SushitrainCore
	import SynctrainHostKit

	/// Finder info, folder settings, and history requests open independently of the browser, including at launch.
	struct FinderItemWindow: View {
		static let id = "finderItem"
		static let isLaunch =
			ProcessInfo.processInfo.environment["SYNCTRAIN_HISTORY_BRIDGE"] == "1"
			|| ProcessInfo.processInfo.environment[FinderAction.windowLaunchEnvironmentKey] == "1"
			|| ProcessInfo.processInfo.environment[FinderAction.infoLaunchEnvironmentKey] == "1"

		private struct LoadRequest: Equatable {
			let configurationGeneration: Int
			let deliveryID: UUID
		}

		let action: FinderAction
		@Environment(AppState.self) private var appState
		@Environment(\.dismiss) private var dismiss
		@State private var entry: SushitrainEntry?
		@State private var folder: SushitrainFolder?
		@State private var unavailable = false
		@State private var deliveryID = UUID()
		@State private var loadedDeliveryID: UUID?

		var body: some View {
			Group {
				if action.kind == .versions, let folder {
					FileVersionsView(folder: folder, path: action.item.path.precomposedStringWithCanonicalMapping)
				}
				else {
					NavigationStack {
						if action.kind == .settings, let folder {
							FolderView(folder: folder)
								.toolbar { Button("Done") { dismiss() } }
						}
						else if let entry {
							EntryInfoView(entry: entry, showPath: true, siblings: nil)
								.id(loadedDeliveryID)
						}
						else if unavailable {
							ContentUnavailableView {
								Label("Cannot Open Finder Item", systemImage: "exclamationmark.triangle")
							} description: {
								Text("The requested item is unavailable or belongs to a different configuration.")
							}
						}
						else {
							ProgressView().controlSize(.large)
						}
					}
				}
			}
			.frame(minWidth: 600, minHeight: 500)
			.background(WindowConfiguration())
			.showsToast()
			.onAppear { QuickActionService.shared.hasPresentedFinderWindow = true }
			.onChange(of: QuickActionService.shared.presentedFinderWindowRequest, initial: true) { _, request in
				guard let request, request.action == action else { return }
				deliveryID = request.id
			}
			.task(id: LoadRequest(configurationGeneration: appState.finderConfigurationGeneration, deliveryID: deliveryID)) {
				await load(deliveryID: deliveryID)
			}
		}

		private func load(deliveryID: UUID) async {
			// Preserve open sheets and navigation while a configuration save is revalidated.
			guard action.windowAction != nil,
				await FinderActionValidator.status(for: action, appState: appState) != nil,
				let folder = appState.client.folder(withID: action.item.folderID)
			else {
				if !Task.isCancelled {
					entry = nil
					self.folder = nil
					unavailable = true
				}
				return
			}
			guard !Task.isCancelled else { return }
			if action.kind == .settings || action.kind == .versions {
				self.folder = folder
			}
			else if let item = try? folder.getFileInformation(action.item.path.precomposedStringWithCanonicalMapping) {
				entry = item
			}
			else {
				entry = nil
				unavailable = true
				return
			}
			// Each new delivery replaces EntryInfoView's stored metadata snapshot in the same window.
			loadedDeliveryID = deliveryID
			unavailable = false
		}

		/// These utility windows stay independent even when macOS prefers tabs for document windows.
		private struct WindowConfiguration: NSViewRepresentable {
			final class ContentView: NSView {
				override func viewDidMoveToWindow() {
					super.viewDidMoveToWindow()
					window?.tabbingMode = .disallowed
				}
			}

			func makeNSView(context: Context) -> ContentView { ContentView() }
			func updateNSView(_ nsView: ContentView, context: Context) {}
		}
	}

	@MainActor enum FinderActionValidator {
		static func status(for action: FinderAction, appState: AppState) async -> FinderItemStatus? {
			let client = appState.client
			let configDirectory = appState.configDirectory
			let generation = appState.finderConfigurationGeneration
			let result: FinderItemStatus? = await Task.detached(priority: .userInitiated) {
				guard let rootsData = try? client.finderRootsJSON(),
					let roots = try? JSONDecoder().decode([FinderRoot].self, from: rootsData),
					action.isValid(
						in: FinderRegistry(
							configuration: FinderIntegration.configurationID(configDirectory, deviceID: client.deviceID(), roots: roots),
							running: true, roots: roots)),
					let request = try? JSONEncoder().encode([action.item]), let data = try? client.finderStatusJSON(request)
				else { return nil }
				// A folder can be repointed while a slow filesystem query is in flight.
				guard let currentData = try? client.finderRootsJSON(),
					let currentRoots = try? JSONDecoder().decode([FinderRoot].self, from: currentData),
					FinderIntegration.configurationID(configDirectory, deviceID: client.deviceID(), roots: currentRoots)
						== action.configuration
				else { return nil }
				return (try? JSONDecoder().decode([FinderItemStatus].self, from: data).first).map {
					HistoryAvailability.applying(to: $0)
				}
			}.value
			guard !Task.isCancelled, generation == appState.finderConfigurationGeneration,
				let result, action.kind == .settings || action.item.path.isEmpty || result.indexed,
				action.kind != .versions || result.versions
			else { return nil }
			return result
		}
	}
#endif
