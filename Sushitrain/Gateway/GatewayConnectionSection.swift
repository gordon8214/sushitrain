// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	/// How devices reach the service: port, host name, name, certificate and cache size. Changes apply when saved.
	struct GatewayConnectionSection: View {
		private var controller = GatewayController.shared
		@State private var draft = GatewayController.shared.settings
		@State private var hasCertificateFiles = GatewayCertificateStore.hasFiles
		@State private var showCertificateImporter = false
		@State private var errorMessage: String? = nil

		private static let cacheSizesMiB = [1024, 2048, 4096, 8192, 16384, 32768, 65536]

		private var canApply: Bool {
			draft != controller.settings && draft.isValid
				&& (draft.certificateMode == .selfSigned || hasCertificateFiles)
		}

		var body: some View {
			Group {
				Section {
					TextField("Port", value: $draft.port, format: .number.grouping(.never))
					if !draft.isValid {
						Text("Choose a port between 1 and 65535.")
							.foregroundStyle(.red)
					}
					TextField(
						"Host name", text: $draft.publicHost, prompt: Text(verbatim: GatewaySettings.localHostName))
					TextField(
						"Service name", text: $draft.serviceName, prompt: Text(verbatim: GatewaySettings.computerName))
					Picker("Content cache", selection: $draft.contentCacheMiB) {
						ForEach(cacheSizes, id: \.self) { size in
							Text((Int64(size) << 20).formatted(.byteCount(style: .memory))).tag(size)
						}
					}
				} header: {
					Text("Connection")
				} footer: {
					Text(
						"Devices connect to this host name and port; pairing codes contain them. This Mac's local name only works on the same network: elsewhere, use a VPN or an address you set up to reach this Mac. Paired devices show the service name. The content cache holds files fetched from other devices and changes being received."
					)
				}

				Section {
					Picker("Certificate", selection: $draft.certificateMode) {
						Text("Self-signed").tag(GatewaySettings.CertificateMode.selfSigned)
						Text("Certificate files").tag(GatewaySettings.CertificateMode.files)
					}
					if draft.certificateMode == .files {
						LabeledContent("Certificate and private key") {
							HStack {
								Text(hasCertificateFiles ? "Chosen" : "Not chosen")
									.foregroundStyle(hasCertificateFiles ? Color.secondary : Color.red)
								Button("Choose Files…") {
									showCertificateImporter = true
								}
								.fileImporter(
									isPresented: $showCertificateImporter, allowedContentTypes: [.data],
									allowsMultipleSelection: true
								) { result in
									importCertificate(result)
								}
								.alert(
									"Could not use the certificate", isPresented: Binding.isNotNil($errorMessage),
									actions: {
										Button("OK") {}
									},
									message: {
										Text(errorMessage ?? "")
									})
							}
						}
					}
				} header: {
					Text("Certificate")
				} footer: {
					if draft.certificateMode == .selfSigned {
						Text(
							"Synctrain creates a certificate for this Mac. Pairing codes contain its public key, so devices accept exactly this certificate."
						)
					}
					else {
						Text(
							"Choose PEM files with a certificate that devices trust, such as one from Let's Encrypt, and its private key (one file with both, or two files). Synctrain keeps copies, and copies the files again when they change."
						)
					}
				}

				Section {
					HStack {
						Spacer()
						Button("Revert") {
							draft = controller.settings
						}
						.disabled(draft == controller.settings)
						Button(controller.isEnabled ? "Save and Restart" : "Save") {
							controller.apply(draft)
						}
						.keyboardShortcut(.defaultAction)
						.disabled(!canApply)
					}
				}
			}
		}

		/// The sizes to choose from, including the current one
		private var cacheSizes: [Int] {
			Set(Self.cacheSizesMiB + [draft.contentCacheMiB]).sorted()
		}

		private func importCertificate(_ result: Result<[URL], any Error>) {
			Task {
				do {
					try await controller.importCertificate(from: try result.get())
				}
				catch CocoaError.userCancelled {
					return
				}
				catch {
					errorMessage = error.localizedDescription
				}
				hasCertificateFiles = GatewayCertificateStore.hasFiles
			}
		}
	}
#endif
