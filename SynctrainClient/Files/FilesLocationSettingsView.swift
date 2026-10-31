// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
#if os(macOS)
	import AppKit
#endif

/// The Synctrain location in the Files app: whether it is on, and setting it up again
struct FilesLocationSettingsView: View {
	private var domain = FilesDomain.shared
	private var remote = RemoteCoordinator.shared
	@State private var showRebuildConfirmation = false
	@State private var showRebuildRefusal = false
	@State private var pendingItemCount = 0
	@State private var showPendingItemsConfirmation = false

	var body: some View {
		Section {
			LabeledContent("Synctrain location") {
				switch domain.isEnabled {
				case .some(true): Text("On")
				case .some(false): Text("Off")
				case .none: ProgressView()
				}
			}
			if let error = domain.lastError {
				Text(error).foregroundStyle(.red)
			}
			#if os(macOS)
				if domain.isEnabled == false {
					Button("Open System Settings…") {
						ClientLifecycle.showLocationSettings()
					}
				}
			#endif
			Button("Rebuild Synctrain location", role: .destructive) {
				if remote.hasUnresolvedOperations {
					showRebuildRefusal = true
				}
				else {
					showRebuildConfirmation = true
				}
			}
			.disabled(domain.isRebuilding)
		} header: {
			#if os(macOS)
				Text("Finder")
			#else
				Text("Files app")
			#endif
		} footer: {
			#if os(macOS)
				if domain.isEnabled != true {
					Text(
						"macOS asks you to allow this. To see your folders in Finder, turn on Synctrain in System Settings, under General › Login Items & Extensions › File Providers."
					)
				}
				else {
					Text(
						"Rebuilding removes the Synctrain location from Finder and sets it up again. Your files are not affected. Edits of folders are kept."
					)
				}
			#else
				if domain.isEnabled != true {
					Text(
						"To open your folders in the Files app, open Files, tap Browse, then tap Synctrain under Locations and turn it on."
					)
				}
				else {
					Text(
						"Rebuilding removes the Synctrain location from the Files app and sets it up again. Your files are not affected, but you will need to turn the location on again in the Files app. Edits of folders on gateways are kept."
					)
				}
			#endif
		}
		.task {
			await domain.refreshState()
		}
		#if os(macOS)
			.onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
				// Back from System Settings
				Task { await domain.refreshState() }
			}
		#endif
		.alert("The location cannot be rebuilt now", isPresented: $showRebuildRefusal) {
			Button("OK") {}
		} message: {
			Text(FilesDomain.unresolvedEditsMessage)
		}
		.confirmationDialog(
			"Rebuild the Synctrain location?", isPresented: $showRebuildConfirmation, titleVisibility: .visible
		) {
			Button("Rebuild", role: .destructive) {
				Task {
					// Changes the system has not handed to Synctrain yet would be lost
					pendingItemCount = await domain.pendingItemCount()
					if pendingItemCount > 0 {
						showPendingItemsConfirmation = true
					}
					else {
						await domain.rebuild()
					}
				}
			}
		}
		.confirmationDialog(
			"Discard changes that were not saved?", isPresented: $showPendingItemsConfirmation,
			titleVisibility: .visible
		) {
			Button("Rebuild Anyway", role: .destructive) {
				Task {
					await domain.rebuild()
				}
			}
		} message: {
			#if os(macOS)
				Text(
					"Changes to \(pendingItemCount) items made in Finder have not reached Synctrain yet. Rebuilding the location discards them."
				)
			#else
				Text(
					"Changes to \(pendingItemCount) items made in the Files app have not reached Synctrain yet. Rebuilding the location discards them."
				)
			#endif
		}
	}
}
