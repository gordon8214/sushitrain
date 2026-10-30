// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import AVFoundation
import QuickLook
import SwiftUI
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// Opens files in the app: audio and video in the player, anything else in Quick Look
@Observable @MainActor final class FileViewer {
	/// The file being fetched
	private(set) var opening: IndexItem? = nil
	/// How much of it is here (nil while unknown)
	private(set) var fraction: Double? = nil
	var previewURL: URL? = nil
	var playing: MediaPlayback? = nil
	var errorMessage: String? = nil
	/// The file that was shown, when it changed while it was (it can be opened again, as it is now)
	var changedFile: IndexItem? = nil

	@ObservationIgnored private var task: Task<Void, Never>? = nil
	/// The version of the file being shown
	@ObservationIgnored private var shown: IndexItem? = nil

	/// Smaller files are simply downloaded first: that takes moments, and then they play from this device
	private static let streamingThreshold: Int64 = 64 << 20

	func open(_ item: IndexItem) {
		guard let environment = RemoteEnvironment.shared else {
			return
		}
		task?.cancel()
		opening = item
		fraction = nil
		task = Task {
			defer {
				if opening == item {
					opening = nil
				}
			}
			if Self.isMedia(item), item.size >= Self.streamingThreshold,
				let playback = await Self.streamingPlayback(of: item, environment: environment)
			{
				shown = item
				playing = playback
				return
			}
			do throws(ContentError) {
				let url = try await ContentOpener.viewingCopy(of: item, environment: environment) { verified, total in
					let fraction = total > 0 ? Double(verified) / Double(total) : nil
					Task { @MainActor in self.fraction = fraction }
				}
				if Task.isCancelled {
					return
				}
				shown = item
				if Self.isMedia(item) {
					playing = await Self.playback(of: url)
					if playing == nil {
						errorMessage = String(
							localized: "This format cannot be played here. Open the file in the Files app to use another app.")
					}
				}
				else {
					previewURL = url
				}
			}
			catch {
				if case .remote(.canceled) = error {
					return
				}
				errorMessage = error.localizedDescription
			}
		}
	}

	func cancel() {
		task?.cancel()
		opening = nil
	}

	/// Call when the file is no longer shown. What is shown cannot be replaced under the viewer, so this is when a newer
	/// version is offered.
	func viewerClosed() {
		guard let shown else {
			return
		}
		self.shown = nil
		let current = try? RemoteEnvironment.shared?.store.item(id: shown.id)
		if let current, current.contentRevision != shown.contentRevision {
			changedFile = current
		}
	}

	private static func isMedia(_ item: IndexItem) -> Bool {
		guard let type = UTType(filenameExtension: (item.name as NSString).pathExtension) else {
			return false
		}
		return type.conforms(to: .audiovisualContent)
	}

	private static func playback(of url: URL) async -> MediaPlayback? {
		let asset = AVURLAsset(url: url)
		guard (try? await asset.load(.isPlayable)) == true else {
			return nil
		}
		return MediaPlayback(asset: asset)
	}

	/// Playback from the gateway while the file stays there, for a large file that is not on this device and has no unsent
	/// edit. Nil when it cannot be played that way: downloading it is what is left then.
	private static func streamingPlayback(of item: IndexItem, environment: RemoteEnvironment) async -> MediaPlayback? {
		let store = environment.store
		guard case .remote(let state)? = try? store.contentSource(of: item, storage: environment.storage),
			state.contentID.flatMap({ environment.storage.cachedContentURL($0) }).map({
				!FileManager.default.fileExists(atPath: $0.path(percentEncoded: false))
			}) == true,
			let request = RemoteDownloader.Request(state),
			let binding = try? store.remoteBinding(folderID: item.folderID),
			let client = environment.client(serviceID: binding.serviceID),
			let type = UTType(filenameExtension: (item.name as NSString).pathExtension)
		else {
			return nil
		}
		let stream = RemoteContentStream(client: client, scheduler: environment.scheduler, request: request)
		guard let size = try? await stream.open() else {
			return nil
		}
		let loader = GatewayMediaLoader(stream: stream, size: size, contentType: type.identifier)
		guard let asset = loader.makeAsset(), (try? await asset.load(.isPlayable)) == true else {
			return nil
		}
		return MediaPlayback(asset: asset, loader: loader)
	}
}

/// What a `FileViewer` shows
struct FileViewerPresentation: ViewModifier {
	@Bindable var viewer: FileViewer

	func body(content: Content) -> some View {
		content
			.quickLookPreview($viewer.previewURL)
			.fullScreenCover(item: $viewer.playing) { playback in
				MediaPlayerView(playback: playback).ignoresSafeArea()
			}
			.onChange(of: viewer.previewURL) { _, url in
				if url == nil {
					viewer.viewerClosed()
				}
			}
			.onChange(of: viewer.playing?.id) { _, id in
				if id == nil {
					viewer.viewerClosed()
				}
			}
			.alert("Could not open the file", isPresented: Binding.isNotNil($viewer.errorMessage)) {
				Button("OK") {}
			} message: {
				Text(viewer.errorMessage ?? "")
			}
			.alert(
				"This file changed while you viewed it", isPresented: Binding.isNotNil($viewer.changedFile),
				presenting: viewer.changedFile
			) { file in
				Button("Reload") {
					viewer.open(file)
				}
				Button("OK", role: .cancel) {}
			} message: { file in
				Text("Reload to see the latest version of \(file.name).")
			}
			.safeAreaInset(edge: .bottom) {
				if let item = viewer.opening {
					OpeningBar(name: item.name, fraction: viewer.fraction) {
						viewer.cancel()
					}
				}
			}
	}
}

/// A file being fetched, for as long as that takes
private struct OpeningBar: View {
	let name: String
	let fraction: Double?
	let cancel: () -> Void

	var body: some View {
		HStack {
			VStack(alignment: .leading, spacing: 4) {
				Text(name).lineLimit(1).font(.subheadline)
				if let fraction {
					ProgressView(value: fraction)
				}
				else {
					ProgressView().progressViewStyle(.linear)
				}
			}
			Button("Cancel", systemImage: "xmark.circle.fill", action: cancel)
				.labelStyle(.iconOnly)
		}
		.padding(12)
		.background(.regularMaterial, in: .rect(cornerRadius: 12))
		.padding(.horizontal)
		.padding(.bottom, 8)
	}
}
