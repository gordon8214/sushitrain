// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore
import QuickLook
import AVKit

struct FileViewerView: View {
	@Environment(AppState.self) private var appState

	@State var file: SushitrainEntry? = nil
	let siblings: [SushitrainEntry]?
	let inSheet: Bool
	@Binding var isShown: Bool

	@State private var selfIndex: Int? = nil
	@Environment(\.openWindow) private var openWindow

	var body: some View {
		NavigationStack {
			if let file = file {
				FileViewerContentView(file: file, isShown: $isShown, playFile: .constant(true))
					.navigationTitle(file.fileName())
					.toolbar {
						self.toolbarContent(file: file)
					}
			}
		}
		.onAppear {
			if let file = self.file {
				self.selfIndex = self.siblings?.firstIndex {
					$0.id == file.id
				}
			}
		}
		.presentationSizing(.fitted)
		.frame(minWidth: 640, minHeight: 480)
	}

	private func next(_ offset: Int) {
		if let siblings = siblings, let file = self.file, let idx = self.siblings?.firstIndex(where: { $0.id == file.id }) {
			let newIndex = idx + offset
			if newIndex >= 0 && newIndex < siblings.count {
				let newFile = siblings[newIndex]
				self.file = newFile
				selfIndex = siblings.firstIndex(of: newFile)
			}
		}
	}

	@ToolbarContentBuilder private func toolbarContent(file: SushitrainEntry) -> some ToolbarContent {
		if inSheet {
			SheetButton(role: .done) {
				isShown = false
			}

			ToolbarItemGroup(placement: .automatic) {
				Button(
					"Open in new window",
					systemImage: "macwindow.on.rectangle"
				) {
					if let folder = file.folder {
						openWindow(
							id: "preview",
							value: Preview(
								folderID: folder
									.folderID,
								path: file.path()
							))
						isShown = false
					}
				}
			}
		}

		if let siblings = siblings, let selfIndex = selfIndex {
			ToolbarItemGroup(placement: .automatic) {
				Button("Previous", systemImage: "chevron.up") {
					next(-1)
				}
				.disabled(selfIndex < 1)
				.keyboardShortcut(KeyEquivalent.upArrow)

				Button("Next", systemImage: "chevron.down") {
					next(1)
				}
				.disabled(selfIndex >= siblings.count - 1)
				.keyboardShortcut(KeyEquivalent.downArrow)
			}
		}
	}
}

private struct FileViewerContentView: View {
	@Environment(AppState.self) private var appState
	var file: SushitrainEntry
	@Binding var isShown: Bool
	@Binding var playFile: Bool
	@State private var error: (any Error)? = nil
	@State private var loading: Bool = true

	var body: some View {
		Group {
			if let e = error {
				ContentUnavailableView(
					"Cannot preview this file", systemImage: "exclamationmark.triangle.fill",
					description: Text(e.localizedDescription)
				)
				.onTapGesture {
					self.error = nil
				}
			}
			else if !playFile {
				// Placeholder is shown while this view is not yet fully scrolled into view
				ZStack {
					if file.isVideo {
						Rectangle().frame(maxWidth: .infinity, maxHeight: .infinity)
							.foregroundStyle(.black)
							.ignoresSafeArea()
					}
					ThumbnailView(
						file: file, showFileName: false, showErrorMessages: false, scaleToFill: false, generateOnDemand: true
					)
					.opacity(0.5)
					.blur(radius: 10.0)
				}
			}
			else {
				if file.isVideo || file.isAudio {
					FileMediaPlayer(file: file, visible: $isShown)
				}
				else if file.isWebPreviewable {
					ZStack {
						WebView(
							url: url, trustFingerprints: [], verticallyCenter: !file.isPDF, isLoading: self.$loading.animation(),
							error: self.$error
						)
						.id(url)
						if loading {
							ThumbnailView(
								file: file, showFileName: false, showErrorMessages: false, scaleToFill: false, generateOnDemand: false
							)
							.opacity(0.5)
							.blur(radius: 10.0)
							ProgressView()
								.progressViewStyle(.circular).controlSize(.extraLarge)
						}
					}
				}
				else {
					ContentUnavailableView(
						"Cannot preview this file", systemImage: "document",
						description: Text("Cannot show a preview for this type of file."))
				}
			}
		}
		.onChange(of: file) { (_, _) in
			self.error = nil
			self.loading = true
		}
	}

	private var url: URL {
		return file.localNativeFileURL ?? URL(string: self.file.onDemandURL())!
	}
}

private struct FileMediaPlayer: View {
	@Environment(AppState.self) private var appState
	var file: SushitrainEntry
	@Binding var visible: Bool

	@State private var player: AVPlayer?

	var body: some View {
		ZStack {
			if let player = self.player {
				MacMediaPlayerView(player: player, showsFullScreenToggleButton: file.isVideo)
			}
			else {
				Rectangle()
					.scaledToFill()
					.foregroundStyle(.black)
					.onTapGesture {
						visible = false
					}
			}
		}
		.background(.black)
		.onAppear {
			Task {
				await startPlayer()
			}
		}
		.onDisappear {
			stopPlayer()
		}
		.onChange(of: file) {
			Task {
				await startPlayer()
			}
		}
	}

	private func stopPlayer() {
		if let player = self.player {
			player.pause()
			self.player = nil
		}
	}

	private func startPlayer() async {
		self.stopPlayer()
		do {
			let url = file.localNativeFileURL ?? URL(string: self.file.onDemandURL())!
			let avAsset = AVURLAsset(url: url)
			Log.warn("StartPlayer file=\(file.fileName()) url=\(url)")
			if try await avAsset.load(.isPlayable) {
				let player = AVPlayer(playerItem: AVPlayerItem(asset: avAsset))
				// TODO: External playback requires us to use http://devicename.local:xxx/file/.. URLs rather than http://localhost.
				// Resolve using Bonjour perhaps?
				player.allowsExternalPlayback = false
				player.audiovisualBackgroundPlaybackPolicy = .automatic
				player.preventsDisplaySleepDuringVideoPlayback = !file.isAudio
				player.playImmediately(atRate: 1.0)
				self.player = player
			}
			else {
				self.visible = false
			}
		}
		catch {
			Log.warn("Error starting player: \(error.localizedDescription)")
		}
	}
}

private struct MacMediaPlayerView: NSViewRepresentable {
	let player: AVPlayer
	let showsFullScreenToggleButton: Bool

	func makeNSView(context: Context) -> AVPlayerView {
		let view = AVPlayerView()
		view.player = player
		view.controlsStyle = .inline
		view.showsFullScreenToggleButton = showsFullScreenToggleButton
		view.videoGravity = .resizeAspect
		view.wantsLayer = true
		view.layer?.backgroundColor = NSColor.black.cgColor
		return view
	}

	func updateNSView(_ nsView: AVPlayerView, context: Context) {
		nsView.player = player
		nsView.showsFullScreenToggleButton = showsFullScreenToggleButton
	}
}
