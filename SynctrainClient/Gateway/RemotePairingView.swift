// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
#if os(iOS)
	import VisionKit
#else
	import CoreImage
	import UniformTypeIdentifiers
#endif

/// Pairs this device with a gateway: scan the code the gateway shows (or paste its link), approve the request on the
/// gateway, then choose which of its folders to open in Files
struct RemotePairingView: View {
	@Environment(\.dismiss) private var dismiss
	@State private var pairing = RemotePairing()
	@State private var link = ""
	@State private var scannedText = ""
	@State private var showScanner = false
	@State private var now = Date.now
	@AppStorage(DeviceName.storageKey) private var deviceName = ""

	#if os(iOS)
		private var canScan: Bool {
			DataScannerViewController.isSupported && DataScannerViewController.isAvailable
		}
	#else
		@State private var showImageChooser = false
		@State private var imageProblem: String? = nil
	#endif

	var body: some View {
		NavigationStack {
			Form {
				switch pairing.phase {
				case .entering, .failed:
					entrySections
				case .requesting:
					Section {
						HStack {
							ProgressView()
							Text("Asking the gateway for access…")
						}
					}
				case .waiting(let gateway, let expires):
					waitingSection(gateway: gateway, expires: expires)
				case .paired(let serviceID, let name):
					Section {
						Label("Paired with \(name).", systemImage: "checkmark.circle.fill")
							.foregroundStyle(.green)
					} footer: {
						Text("Choose the folders of the gateway to open on this device.")
					}
					RemoteGatewayFoldersSection(serviceID: serviceID)
				}
			}
			.navigationTitle("Pair with a Gateway")
			#if os(iOS)
				.navigationBarTitleDisplayMode(.inline)
			#else
				.formStyle(.grouped)
			#endif
			.toolbar {
				if case .paired = pairing.phase {
					SheetButton(role: .done) {
						dismiss()
					}
				}
				else {
					SheetButton(role: .cancel) {
						pairing.cancel()
						dismiss()
					}
				}
			}
			#if os(iOS)
				.sheet(isPresented: $showScanner) {
					scannerSheet
				}
			#else
				.fileImporter(isPresented: $showImageChooser, allowedContentTypes: [.image]) { result in
					readCode(from: result)
				}
			#endif
			.onChange(of: scannedText) { _, text in
				guard !text.isEmpty else {
					return
				}
				link = text
				scannedText = ""
				pairing.start(link: text)
			}
			.task(id: pairing.phase) {
				// The countdown updates itself; the view only has to know when the request expired
				guard case .waiting(_, let expires) = pairing.phase else {
					return
				}
				now = .now
				if expires > now {
					try? await Task.sleep(for: .seconds(expires.timeIntervalSinceNow))
					now = .now
				}
			}
		}
		.onDisappear {
			pairing.cancel()
		}
	}

	@ViewBuilder private var entrySections: some View {
		Section {
			TextField(DeviceName.systemName, text: $deviceName)
		} header: {
			Text("Device name")
		} footer: {
			Text("The gateway lists this device under this name.")
		}

		Section {
			#if os(iOS)
				if canScan {
					Button("Scan Pairing Code…", systemImage: "qrcode.viewfinder") {
						showScanner = true
					}
				}
				TextField("synctrain+pair://…", text: $link, axis: .vertical)
					.textInputAutocapitalization(.never)
					.autocorrectionDisabled()
					.keyboardType(.URL)
					.font(.body.monospaced())
			#else
				Button("Choose an Image of the Code…", systemImage: "qrcode") {
					showImageChooser = true
				}
				TextField("synctrain+pair://…", text: $link, axis: .vertical)
					.autocorrectionDisabled()
					.font(.body.monospaced())
				if let imageProblem {
					Text(imageProblem).foregroundStyle(.red)
				}
			#endif
			Button("Pair") {
				pairing.start(link: link)
			}
			.disabled(!RemotePairing.isPairingLink(link))
		} footer: {
			#if os(iOS)
				Text(
					"Start pairing on the gateway: in Synctrain on a Mac, under Settings › Remote file service; on a server, with synctrain-gateway pair. Then scan the code it shows, or paste its link here."
				)
			#else
				Text(
					"Start pairing on the gateway: in Synctrain on a Mac, under Settings › Remote file service; on a server, with synctrain-gateway pair. Then paste its link here, or choose an image of the code it shows."
				)
			#endif
		}

		if case .failed(let message) = pairing.phase {
			Section {
				Text(message).foregroundStyle(.red)
			}
		}
	}

	@ViewBuilder private func waitingSection(gateway: String, expires: Date) -> some View {
		Section {
			VStack(alignment: .leading, spacing: 12) {
				Label("Approve this device on \(gateway).", systemImage: "hand.raised")
					.font(.headline)
				Text(
					"The gateway lists this device as \(DeviceName.current). Choose the folders it may access there."
				)
				.foregroundStyle(.secondary)
				if expires > now {
					HStack {
						ProgressView()
						Text("Waiting for approval (\(Text(timerInterval: now...expires, countsDown: true)))")
					}
					.foregroundStyle(.secondary)
				}
			}
			.padding(.vertical, 4)
		}
	}

	#if os(iOS)
		@ViewBuilder private var scannerSheet: some View {
			if canScan {
				NavigationStack {
					QRScannerViewRepresentable(
						scannedText: $scannedText, shouldStartScanning: $showScanner,
						dataToScanFor: [.barcode(symbologies: [.qr])]
					)
					.navigationTitle("Scan the pairing code")
					.navigationBarTitleDisplayMode(.inline)
					.toolbar {
						SheetButton(role: .cancel) {
							showScanner = false
						}
					}
				}
			}
		}
	#else
		/// Reads the pairing link from an image of the code, such as a screenshot of another Mac's screen
		private func readCode(from result: Result<URL, any Error>) {
			imageProblem = nil
			guard case .success(let url) = result else {
				imageProblem = String(localized: "This image has no pairing code in it.")
				return
			}
			let didAccess = url.startAccessingSecurityScopedResource()
			defer {
				if didAccess {
					url.stopAccessingSecurityScopedResource()
				}
			}
			let detector = CIDetector(
				ofType: CIDetectorTypeQRCode, context: nil, options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])
			let codes = CIImage(contentsOf: url).flatMap { detector?.features(in: $0) } ?? []
			let links = codes.compactMap { ($0 as? CIQRCodeFeature)?.messageString }
			guard let found = links.first(where: RemotePairing.isPairingLink) else {
				imageProblem = String(localized: "This image has no pairing code in it.")
				return
			}
			scannedText = found
		}
	#endif
}
