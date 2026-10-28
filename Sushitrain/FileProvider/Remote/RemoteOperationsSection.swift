// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import SwiftUI
	import SynctrainFileProviderKit

	/// Edits of folders on gateways that are being sent, were kept as conflict copies, were rejected, or whose outcome
	/// is unknown (of one folder, or of all). Edits that wait for review can be discarded, and edited contents that
	/// only exist on this device exported.
	struct RemoteOperationsSection: View {
		/// The local folder to list edits of (nil: all folders)
		let folderID: String?
		private var coordinator = RemoteCoordinator.shared
		@State private var confirmDiscard: RemoteOperationSummary? = nil
		@State private var exportURL: URL? = nil
		@State private var errorMessage: String? = nil

		init(folderID: String? = nil) {
			self.folderID = folderID
		}

		private var operations: [RemoteOperationSummary] {
			folderID.map { coordinator.operations(folderID: $0) } ?? coordinator.operations
		}

		var body: some View {
			if !operations.isEmpty {
				Section {
					ForEach(operations) { operation in
						RemoteOperationRow(operation: operation, showsFolder: folderID == nil)
							.contextMenu {
								buttons(operation)
							}
							.swipeActions {
								buttons(operation)
							}
					}
				} header: {
					Text("Edits on gateways")
				} footer: {
					Text(
						"Edits are sent when the gateway can be reached. Rejected edits and edits whose outcome is unknown stay on this device until you export or discard them; discarding shows the gateway's version again."
					)
				}
				.confirmationDialog(
					"Discard this edit?", isPresented: Binding.isNotNil($confirmDiscard), titleVisibility: .visible,
					presenting: confirmDiscard
				) { operation in
					Button("Discard", role: .destructive) {
						discard(operation)
					}
				} message: { operation in
					if operation.canExport {
						Text("The edited contents of \(operation.name) are removed from this device. Export them first to keep them.")
					}
					else {
						Text("\(operation.name) shows the gateway's version again.")
					}
				}
				.fileMover(isPresented: Binding.isNotNil($exportURL), file: exportURL) { result in
					if case .failure(let error) = result {
						errorMessage = error.localizedDescription
					}
					exportURL = nil
				}
				.alert(
					"Could not change the edit", isPresented: Binding.isNotNil($errorMessage), presenting: errorMessage
				) { _ in
					Button("OK") {}
				} message: { message in
					Text(message)
				}
			}
		}

		@ViewBuilder private func buttons(_ operation: RemoteOperationSummary) -> some View {
			if operation.canExport {
				Button("Export…", systemImage: "square.and.arrow.up") {
					export(operation)
				}
				.tint(.blue)
			}
			if operation.canDiscard {
				Button("Discard", systemImage: "trash", role: .destructive) {
					confirmDiscard = operation
				}
			}
		}

		private func export(_ operation: RemoteOperationSummary) {
			Task {
				do {
					exportURL = try await coordinator.exportCopy(of: operation)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func discard(_ operation: RemoteOperationSummary) {
			Task {
				do throws(RemoteSwitchError) {
					try await coordinator.discard(operationID: operation.id)
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}

	private struct RemoteOperationRow: View {
		let operation: RemoteOperationSummary
		let showsFolder: Bool

		var body: some View {
			VStack(alignment: .leading, spacing: 2) {
				HStack {
					Image(systemName: symbol).foregroundStyle(color)
					Text(operation.name).lineLimit(1)
				}
				Text(details).font(.caption).foregroundStyle(.secondary)
				if operation.needsAttention, let error = operation.lastError, !error.isEmpty {
					Text(error).font(.caption).foregroundStyle(.red).lineLimit(3)
				}
			}
		}

		private var details: String {
			let parts = [showsFolder ? operation.folderLabel : nil, operation.kindDescription, operation.stateDescription]
			return parts.compactMap { $0 }.joined(separator: " · ")
		}

		private var symbol: String {
			switch operation.state {
			case .failed, .outcomeUnknown: return "exclamationmark.triangle.fill"
			case .conflict: return "doc.on.doc"
			default: return "arrow.up.circle"
			}
		}

		private var color: Color {
			switch operation.state {
			case .failed, .outcomeUnknown: return .orange
			case .conflict: return .yellow
			default: return .accentColor
			}
		}
	}
#endif
