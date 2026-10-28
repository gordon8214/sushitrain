// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	/// Shows a pairing code for a device to scan, then lets the user approve the device and choose its folders
	struct GatewayPairingView: View {
		private var controller = GatewayController.shared
		@Environment(\.dismiss) private var dismiss
		/// Incremented to start pairing again with a new code
		@State private var attempt = 0
		@State private var session: GatewayPairingSession? = nil
		@State private var request: GatewayPendingPairing? = nil
		@State private var grants: [String: GatewayAccess] = [:]
		/// The name of the device that was just paired
		@State private var pairedName: String? = nil
		@State private var errorMessage: String? = nil
		@State private var isWorking = false
		@State private var now = Date.now

		private static let pollInterval: Duration = .seconds(1)

		private var exportedFolders: [GatewayFolder] {
			controller.folders.filter { controller.exports[$0.id] != nil }
		}

		var body: some View {
			NavigationStack {
				Form {
					if let pairedName {
						pairedSection(pairedName)
					}
					else if let request {
						requestSections(request)
					}
					else if let session {
						codeSections(session)
					}
					else if errorMessage == nil {
						ProgressView()
							.frame(maxWidth: .infinity)
					}

					if let errorMessage {
						Section {
							Text(errorMessage).foregroundStyle(.red)
						}
					}
				}
				.formStyle(.grouped)
				.navigationTitle("Pair a Device")
				.toolbar {
					SheetButton(role: .done) {
						dismiss()
					}
				}
			}
			.frame(minWidth: 460, minHeight: 560)
			.task(id: attempt) {
				await run()
			}
		}

		@ViewBuilder private func codeSections(_ session: GatewayPairingSession) -> some View {
			Section {
				VStack(spacing: 12) {
					Text("In Synctrain on your iPhone or iPad, open Settings › Remote folders › Pair and scan this code.")
						.multilineTextAlignment(.center)
						.fixedSize(horizontal: false, vertical: true)

					QRCodeImage(text: session.url)
						.frame(width: 220, height: 220)

					if session.expires > now {
						HStack {
							ProgressView().controlSize(.small)
							Text("Waiting for the device (\(Text(timerInterval: now...session.expires, countsDown: true)))")
						}
						.foregroundStyle(.secondary)
					}
					else {
						Text("This code has expired.")
							.foregroundStyle(.secondary)
						Button("New Code") {
							attempt += 1
						}
					}
				}
				.frame(maxWidth: .infinity)
			}

			Section {
				Text(verbatim: session.url)
					.font(.caption.monospaced())
					.textSelection(.enabled)
				HStack {
					Spacer()
					Button("Copy Link") {
						writeTextToPasteboard(session.url)
					}
				}
			} header: {
				Text("Link")
			} footer: {
				Text(
					"The code works once. It contains a secret that lets a device ask for access, so only share it with your own devices."
				)
			}
		}

		@ViewBuilder private func requestSections(_ request: GatewayPendingPairing) -> some View {
			Section {
				LabeledContent("Device", value: request.displayName)
				if !request.platform.isEmpty {
					LabeledContent("Platform", value: request.platform)
				}
				LabeledContent("Requested") {
					Text(request.requested, format: .dateTime.hour().minute().second())
				}
			} header: {
				Text("Pairing request")
			} footer: {
				Text("Only approve the request if it comes from the device you are pairing.")
			}

			Section("Access") {
				if exportedFolders.isEmpty {
					Text("No folders are exported. You can give the device access after exporting folders.")
						.foregroundStyle(.secondary)
				}
				ForEach(exportedFolders) { folder in
					GatewayAccessPicker(
						title: folder.name,
						maximum: GatewayAccess(export: controller.exports[folder.id]),
						selection: Binding(
							get: { grants[folder.id] ?? .none },
							set: { grants[folder.id] = $0 }))
				}
			}

			Section {
				HStack {
					Spacer()
					Button("Deny", role: .destructive) {
						deny(request)
					}
					Button("Approve") {
						approve(request)
					}
					.keyboardShortcut(.defaultAction)
				}
				.disabled(isWorking)
			}
		}

		@ViewBuilder private func pairedSection(_ name: String) -> some View {
			Section {
				VStack(spacing: 12) {
					Label("\(name) is paired.", systemImage: "checkmark.circle.fill")
						.foregroundStyle(.green)
					Button("Pair Another Device") {
						attempt += 1
					}
				}
				.frame(maxWidth: .infinity)
			}
		}

		/// Starts a pairing session, then waits for a device to ask for access
		private func run() async {
			session = nil
			request = nil
			pairedName = nil
			errorMessage = nil
			do {
				session = try await controller.startPairing()
			}
			catch {
				errorMessage = error.localizedDescription
				return
			}
			while !Task.isCancelled {
				now = .now
				if pairedName == nil {
					await poll()
				}
				try? await Task.sleep(for: Self.pollInterval)
			}
		}

		private func poll() async {
			guard let session else {
				return
			}
			do {
				// Only requests for the code on screen, not for an earlier one that is still valid
				let pending = try await controller.pendingPairings().filter { $0.sessionID == session.sessionID }
				if let request, !pending.contains(where: { $0.requestID == request.requestID }) {
					// The request expired, or was handled elsewhere
					self.request = nil
				}
				if request == nil, let next = pending.first {
					request = next
					grants = Dictionary(
						controller.exports.values.map { ($0.folderID, GatewayAccess(export: $0)) },
						uniquingKeysWith: { _, last in last })
				}
				errorMessage = nil
			}
			catch {
				errorMessage = error.localizedDescription
			}
		}

		private func approve(_ request: GatewayPendingPairing) {
			let grants = self.grants
			isWorking = true
			Task {
				defer { isWorking = false }
				do {
					try await controller.approvePairing(requestID: request.requestID, grants: grants)
					pairedName = request.displayName
					self.request = nil
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}

		private func deny(_ request: GatewayPendingPairing) {
			isWorking = true
			Task {
				defer { isWorking = false }
				do {
					try await controller.denyPairing(requestID: request.requestID)
					// A code works once; show a new one
					attempt += 1
				}
				catch {
					errorMessage = error.localizedDescription
				}
			}
		}
	}
#endif
