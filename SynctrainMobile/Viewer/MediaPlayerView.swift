// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import AVFoundation
import AVKit
import SwiftUI

/// Audio or video to play
struct MediaPlayback: Identifiable, @unchecked Sendable {
	let id = UUID()
	let asset: AVURLAsset
	/// What feeds a file played while it is on the gateway, which the player does not keep itself
	var loader: GatewayMediaLoader? = nil
}

/// Plays a file, also on while the app is in the background
struct MediaPlayerView: UIViewControllerRepresentable {
	let playback: MediaPlayback

	func makeUIViewController(context: Context) -> AVPlayerViewController {
		try? AVAudioSession.sharedInstance().setCategory(.playback, mode: .moviePlayback)
		try? AVAudioSession.sharedInstance().setActive(true)
		let controller = AVPlayerViewController()
		controller.player = AVPlayer(playerItem: AVPlayerItem(asset: playback.asset))
		controller.player?.play()
		return controller
	}

	func updateUIViewController(_ controller: AVPlayerViewController, context: Context) {}

	static func dismantleUIViewController(_ controller: AVPlayerViewController, coordinator: ()) {
		controller.player?.pause()
		try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
	}
}
