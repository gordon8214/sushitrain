// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import QuickLook
import SwiftUI
import SynctrainFileProviderKit
#if os(macOS)
	import AppKit
#else
	import UIKit
#endif

struct HistoryBrowserView: View {
	@Bindable var model: HistoryBrowserModel
	var done: () -> Void

	var body: some View {
		NavigationStack {
			Group {
				#if os(macOS)
					Form {
						HistoryVersionsSection(model: model)
						HistorySourcesSection(model: model)
					}.formStyle(.grouped)
				#else
					List {
						HistoryVersionsSection(model: model)
						HistorySourcesSection(model: model)
					}
				#endif
			}
			.navigationTitle(model.fileName)
			#if os(macOS)
				.safeAreaInset(edge: .bottom) {
					HStack {
						Spacer()
						Button("Done", action: done).keyboardShortcut(.defaultAction)
					}.padding()
				}
			#else
				.toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done", action: done) } }
			#endif
			.overlay {
				if let activity = model.activity {
					VStack {
						ProgressView(activity, value: model.progress)
						Button("Cancel") { model.cancel() }
					}.padding().frame(width: 240).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
				}
			}
		}
		#if os(macOS)
			.frame(minWidth: 520, minHeight: 380)
		#endif
		.quickLookPreview($model.previewURL)
		.onChange(of: model.exportURL) { _, url in
			#if os(macOS)
				if let url {
					save(url)
					model.exportURL = nil
				}
			#endif
		}
		#if os(iOS)
			.sheet(isPresented: Binding(get: { model.exportURL != nil }, set: { if !$0 { model.exportURL = nil } })) {
				if let url = model.exportURL { HistoryShareView(url: url) }
			}
		#endif
		.alert(
			"Could Not Use This Version",
			isPresented: Binding(get: { model.problem != nil }, set: { if !$0 { model.problem = nil } })
		) {
			Button("OK") {}
		} message: {
			Text(model.problem ?? "")
		}
		.task { await model.load() }
		.onDisappear { model.finish() }
	}

	#if os(macOS)
		private func save(_ source: URL) {
			let panel = NSSavePanel()
			panel.nameFieldStringValue = model.fileName
			panel.canCreateDirectories = true
			guard panel.runModal() == .OK, let destination = panel.url else { return }
			do {
				try model.destination.validateExport(destination)
				let staging = destination.deletingLastPathComponent().appending(path: ".syncthing.history-\(UUID().uuidString).tmp")
				defer { try? FileManager.default.removeItem(at: staging) }
				try FileManager.default.copyItem(at: source, to: staging)
				if FileManager.default.fileExists(atPath: destination.path) {
					_ = try FileManager.default.replaceItemAt(destination, withItemAt: staging)
				}
				else {
					try FileManager.default.moveItem(at: staging, to: destination)
				}
			}
			catch { model.problem = error.localizedDescription }
		}
	#endif
}

private struct HistoryVersionsSection: View {
	let model: HistoryBrowserModel
	var body: some View {
		Section {
			if let notice = model.notice { Text(notice).foregroundStyle(.secondary) }
			ForEach(model.rows) { row in HistoryVersionRow(row: row, model: model) }
			if model.rows.isEmpty && !model.sources.contains(where: \.isLoading) {
				Text("No Previous Versions").foregroundStyle(.secondary)
			}
		} header: {
			Text("Previous Versions")
		} footer: {
			Text(model.restoreExplanation)
		}
	}
}

private struct HistorySourcesSection: View {
	let model: HistoryBrowserModel
	var body: some View {

		Section("Sources") {
			ForEach(model.sources) { source in
				HStack {
					VStack(alignment: .leading) {
						Text(source.name)
						if let message = source.alias ?? source.message { Text(message).font(.caption).foregroundStyle(.secondary) }
					}
					Spacer()
					if source.isLoading { ProgressView().controlSize(.small) }
				}
			}
			Button("Refresh") { Task { await model.retry() } }
				.disabled(model.activity != nil || model.isLoading)
		}
	}
}

private struct HistoryVersionRow: View {
	let row: HistoryRow
	let model: HistoryBrowserModel
	@State private var confirming = false
	var body: some View {
		VStack(alignment: .leading, spacing: 5) {
			Text(row.version.modificationDate, format: .dateTime.year().month().day().hour().minute())
			Text("\(row.sourceName) · \(row.version.size.formatted(.byteCount(style: .file)))").font(.caption).foregroundStyle(
				.secondary)
			Text("Archived \(row.version.versionTime.formatted(date: .abbreviated, time: .shortened))").font(.caption)
				.foregroundStyle(.secondary)
			HStack {
				Button("Open") { model.preview(row) }.accessibilityIdentifier("history.open.\(row.id)")
				Button("Save a Copy…") { model.export(row) }.accessibilityIdentifier("history.export.\(row.id)")
				if model.canRestore {
					Button("Restore…") { confirming = true }.accessibilityIdentifier("history.restore.\(row.id)")
				}
			}.buttonStyle(.borderless)
		}
		.accessibilityElement(children: .contain)
		.accessibilityActions {
			Button("Open") { model.preview(row) }
			Button("Save a Copy…") { model.export(row) }
			if model.canRestore { Button("Restore…") { confirming = true } }
		}
		.disabled(model.activity != nil)
		.confirmationDialog("Restore This Version?", isPresented: $confirming, titleVisibility: .visible) {
			Button("Restore") { model.restore(row) }
		} message: {
			Text(model.restoreExplanation)
		}
	}
}

#if os(iOS)
	private struct HistoryShareView: UIViewControllerRepresentable {
		let url: URL
		func makeUIViewController(context: Context) -> UIActivityViewController {
			UIActivityViewController(activityItems: [url], applicationActivities: nil)
		}
		func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
	}
#endif
