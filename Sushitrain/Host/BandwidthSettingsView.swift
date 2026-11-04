// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import SwiftUI
import SushitrainCore
import SynctrainHostKit

struct BandwidthSettingsView: View {
	@Environment(AppState.self) private var appState
	@ObservedObject var userSettings: AppUserSettings

	@State private var downLimitMbitsPerSec: Int = 0
	@State private var upLimitMbitsPerSec: Int = 0
	@State private var isNetworkTrafficLowPriority = false

	private func update() {
		self.downLimitMbitsPerSec = appState.client.getBandwidthLimitDownMbitsPerSec()
		self.upLimitMbitsPerSec = appState.client.getBandwidthLimitUpMbitsPerSec()
		self.isNetworkTrafficLowPriority = appState.client.isNetworkTrafficLowPriority()
	}

	private func save() {
		// Fails while the engine restarts
		do {
			try appState.client.setBandwidthLimitsMbitsPerSec(downLimitMbitsPerSec, up: upLimitMbitsPerSec)
			try appState.client.setNetworkTrafficLowPriority(self.isNetworkTrafficLowPriority)
		}
		catch {
			Log.warn("Could not save the bandwidth settings: \(error.localizedDescription)")
		}
	}

	var body: some View {
		Form {
			Section("Limit file transfer bandwidth") {
				// Global down
				Toggle(
					"Limit receiving bandwidth",
					isOn: Binding(
						get: {
							return self.downLimitMbitsPerSec > 0
						},
						set: { self.downLimitMbitsPerSec = $0 ? 10 : 0 }
					))

				if downLimitMbitsPerSec > 0 {
					Stepper(
						"\(downLimitMbitsPerSec) Mbit/s",
						value: $downLimitMbitsPerSec, in: 1...1000)
				}

				// Global up
				Toggle(
					"Limit sending bandwidth",
					isOn: Binding(
						get: {
							return self.upLimitMbitsPerSec > 0
						},
						set: { self.upLimitMbitsPerSec = $0 ? 10 : 0 }
					))

				if upLimitMbitsPerSec > 0 {
					Stepper(
						"\(upLimitMbitsPerSec) Mbit/s",
						value: $upLimitMbitsPerSec, in: 1...1000)
				}

				// LAN bandwidth limit
				if upLimitMbitsPerSec > 0 || downLimitMbitsPerSec > 0 {
					Toggle(
						"Also limit in local networks",
						isOn: Binding(
							get: {
								return appState.client.isBandwidthLimitedInLAN()
							},
							set: { nv in
								try? appState.client.setBandwidthLimitedInLAN(nv)
							}))
				}
			}.task {
				self.update()
			}
			.onChange(of: [self.downLimitMbitsPerSec, self.upLimitMbitsPerSec]) { _, _ in
				self.save()
			}

			Section("Limit streaming") {
				Toggle(
					"Limit streaming bandwidth",
					isOn: Binding(
						get: {
							userSettings.streamingLimitMbitsPerSec > 0
						},
						set: { nv in
							if nv {
								userSettings.streamingLimitMbitsPerSec = 15
							}
							else {
								userSettings.streamingLimitMbitsPerSec = 0
							}
						}))

				if userSettings.streamingLimitMbitsPerSec > 0 {
					Stepper(
						"\(userSettings.streamingLimitMbitsPerSec) Mbit/s",
						value: userSettings.$streamingLimitMbitsPerSec, in: 1...100)
				}
			}

			Section {
				Toggle("Low priority traffic", isOn: $isNetworkTrafficLowPriority).onChange(of: self.isNetworkTrafficLowPriority) {
					_, _ in
					self.save()
				}
			} footer: {
				Text(
					"When this is enabled, other network traffic may be prioritized over this app's traffic by supported networks. This may help on congested networks."
				)
			}
		}
		.onChange(of: userSettings.streamingLimitMbitsPerSec) { _, _ in appState.applySettings() }
		.navigationTitle("Bandwidth limitations")
		.formStyle(.grouped)
	}
}
