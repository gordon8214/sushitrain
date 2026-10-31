// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import QuickLook
import SwiftUI
import SynctrainFileProviderKit

/// The previous versions of a file: tap one to preview it, or restore it or save a copy of it from its menu
struct VersionsView: View {
	@Bindable var model: VersionsModel

	var body: some View {
		NavigationStack {
			content
				.disabled(model.activity != nil)
				.navigationTitle(model.fileName.isEmpty ? String(localized: "Previous Versions") : model.fileName)
				#if os(iOS)
					.navigationBarTitleDisplayMode(.inline)
				#endif
				#if os(macOS)
					// Finder's sheet has no toolbar to put this in
					.safeAreaInset(edge: .bottom) {
						HStack {
							Spacer()
							Button("Done") { model.finish() }
							.keyboardShortcut(.defaultAction)
						}
						.padding()
					}
				#else
					.toolbar {
						// Stays available while the gateway works: what it accepted, it carries out anyway
						ToolbarItem(placement: .confirmationAction) {
							Button("Done") { model.finish() }
						}
					}
				#endif
		}
		.overlay {
			if let activity = model.activity {
				ProgressView(activity)
					.padding()
					.background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
			}
		}
		.onChange(of: model.activity) { _, activity in
			if let activity {
				AccessibilityNotification.Announcement(activity).post()
			}
		}
		.quickLookPreview($model.previewURL)
		.alert(model.actionProblem?.title ?? "", isPresented: problemShown, presenting: model.actionProblem) { _ in
			Button("OK") {}
		} message: { problem in
			Text(problem.message)
		}
		.task { await model.loadIfNeeded() }
	}

	private var problemShown: Binding<Bool> {
		Binding(
			get: { model.actionProblem != nil },
			set: { if !$0 { model.actionProblem = nil } })
	}

	@ViewBuilder private var content: some View {
		switch model.phase {
		case .loading:
			ProgressView()
		case .failed(let problem):
			ContentUnavailableView {
				Label(problem.title, systemImage: "exclamationmark.triangle")
			} description: {
				Text(problem.message)
			} actions: {
				if problem.canRetry {
					Button("Try Again") {
						Task { await model.load() }
					}
				}
			}
		case .loaded where model.versions.isEmpty:
			ContentUnavailableView {
				Label("No Previous Versions", systemImage: "clock.arrow.circlepath")
			} description: {
				Text(emptyExplanation)
			}
		case .loaded:
			list
		}
	}

	private var emptyExplanation: String {
		switch model.versioning {
		case .none:
			return String(
				localized:
					"File versioning is off for this folder on \(model.gatewayName). Turn it on in the folder's settings there to keep previous versions."
			)
		case .external:
			return String(
				localized:
					"This folder on \(model.gatewayName) keeps versions with an external command, which Synctrain can't show.")
		case .customPath:
			return String(
				localized:
					"This folder on \(model.gatewayName) keeps versions in a custom location, which the gateway doesn't show. Its versioning must use the default location."
			)
		default:
			if model.hiddenCount > 0 {
				return String(localized: "The previous versions of this file have the same contents as the current one.")
			}
			return String(
				localized: "\(model.gatewayName) keeps a previous version of this file when it is changed or replaced.")
		}
	}

	private var list: some View {
		List {
			Section("Current") {
				VStack(alignment: .leading, spacing: 2) {
					if let modified = model.currentModified {
						Text(modified, format: .dateTime.year().month().day().hour().minute())
					}
					Text(model.currentSize, format: .byteCount(style: .file))
						.font(.subheadline)
						.foregroundStyle(.secondary)
				}
			}
			Section {
				ForEach(model.versions) { version in
					row(version)
				}
			} header: {
				Text("Previous Versions")
			} footer: {
				if !model.canWrite {
					Text("This device may only read this folder, so versions can only be previewed.")
				}
				else if model.hasPendingChange {
					Text("You can restore a version once your changes to this file are uploaded.")
				}
			}
		}
	}

	private func row(_ version: RemoteVersion) -> some View {
		Button {
			model.preview(version)
		} label: {
			HStack {
				VStack(alignment: .leading, spacing: 2) {
					Text(version.modificationDate, format: .dateTime.year().month().day().hour().minute())
						.foregroundStyle(.primary)
					Text(
						"\(version.size.formatted(.byteCount(style: .file))) · Replaced \(version.versionTime.formatted(.relative(presentation: .named)))"
					)
					.font(.subheadline)
					.foregroundStyle(.secondary)
				}
				Spacer()
				if model.downloadingID == version.id {
					ProgressView()
				}
			}
		}
		.accessibilityHint("Shows a preview of this version")
		.contextMenu {
			Button("Preview", systemImage: "eye") {
				model.preview(version)
			}
			if model.canWrite {
				Button("Restore", systemImage: "clock.arrow.circlepath") {
					model.restoreCandidate = version
				}
				.disabled(model.hasPendingChange)
				Button("Save a Copy", systemImage: "plus.square.on.square") {
					model.saveCopy(version)
				}
			}
		}
		.swipeActions {
			if model.canWrite && !model.hasPendingChange {
				Button("Restore") {
					model.restoreCandidate = version
				}
				.tint(.accentColor)
			}
		}
		// Attached to the row, so that on iPad it points at the version
		.confirmationDialog(
			"Restore This Version?", isPresented: restoreConfirmation(of: version), titleVisibility: .visible
		) {
			Button("Restore") {
				model.restore(version)
			}
		} message: {
			Text(
				"The file is replaced by this version on \(model.gatewayName). Its current contents are kept as a previous version."
			)
		}
	}

	private func restoreConfirmation(of version: RemoteVersion) -> Binding<Bool> {
		Binding(
			get: { model.restoreCandidate?.id == version.id },
			set: { if !$0 { model.restoreCandidate = nil } })
	}
}

/// An error the Files app asks the extension to explain: a gateway that no longer accepts this device
struct ErrorView: View {
	let message: String
	let done: () -> Void

	var body: some View {
		NavigationStack {
			ContentUnavailableView {
				Label("Device Not Accepted", systemImage: "exclamationmark.triangle")
			} description: {
				Text(message)
			}
			#if os(macOS)
				.safeAreaInset(edge: .bottom) {
					HStack {
						Spacer()
						Button("Done", action: done)
						.keyboardShortcut(.defaultAction)
					}
					.padding()
				}
			#else
				.toolbar {
					ToolbarItem(placement: .confirmationAction) {
						Button("Done", action: done)
					}
				}
			#endif
		}
	}
}
