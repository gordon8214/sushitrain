// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import OSLog
	import SynctrainFileProviderKit

	struct DesktopFilesActivity: View {
		@Environment(DesktopFilesNavigation.self) private var navigation
		var body: some View {
			Form {
				RemoteTransfersSection(store: navigation.environment?.store)
				Section("Pending Changes") {
					Text("\(RemoteCoordinator.shared.operations.count) edits waiting for a gateway or review")
				}
				RemoteOperationsSection()
				Section("Errors and Recovery") {
					Text("Rejected edits and interrupted transfers remain available for review above.")
				}
				CrashNoticeSection()
				Section("Activity Log") { NavigationLink("Activity Log") { DesktopFilesLogView() } }
			}
		}
	}

	private struct DesktopFilesLogView: View {
		@State private var messages: [String] = []
		@State private var problem: String?
		var body: some View {
			List {
				Text("Messages from this app session. File Provider also records diagnostics in Console.").foregroundStyle(
					.secondary)
				if let problem { Text(problem).foregroundStyle(.red) }
				ForEach(messages.indices, id: \.self) { index in
					Text(messages[index]).font(.body.monospaced()).textSelection(.enabled)
				}
				if messages.isEmpty && problem == nil { Text("No log messages") }
			}
			.navigationTitle("Activity Log")
			.toolbar { Button("Refresh", systemImage: "arrow.clockwise") { Task { await load() } } }
			.task { await load() }
		}
		private func load() async {
			let subsystem = Bundle.main.bundleIdentifier ?? "Synctrain"
			let result = await Task.detached {
				Result { () throws -> [String] in
					let store = try OSLogStore(scope: .currentProcessIdentifier)
					let entries = try store.getEntries(
						at: store.position(date: Date().addingTimeInterval(-1800)),
						matching: NSPredicate(format: "subsystem == %@", subsystem))
					return entries.compactMap { entry -> String? in
						guard let line = entry as? OSLogEntryLog else { return nil }
						return line.date.formatted(date: .omitted, time: .standard) + " " + line.composedMessage
					}.suffix(500).map { $0 }
				}
			}.value
			guard !Task.isCancelled else { return }
			switch result {
			case .success(let rows):
				messages = rows
				problem = nil
			case .failure(let error): problem = error.localizedDescription
			}
		}
	}
#endif
