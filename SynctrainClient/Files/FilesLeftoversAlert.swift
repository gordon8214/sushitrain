// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	extension View {
		/// Tells the user what the Synctrain location keeps on this Mac because it cannot synchronize it, after a folder
		/// left Finder or came back to it (`RemoteCoordinator.leftovers`). One window carries this: two of them would both
		/// try to show the same report.
		func filesLeftoversAlert() -> some View {
			modifier(FilesLeftoversAlert())
		}
	}

	private struct FilesLeftoversAlert: ViewModifier {
		private var coordinator = RemoteCoordinator.shared

		func body(content: Content) -> some View {
			content.alert(
				"Some files could not be synchronized", isPresented: isPresented, presenting: coordinator.leftovers.first
			) { report in
				Button("Show in Finder") {
					ClientLifecycle.show(report.url)
				}
				Button("OK", role: .cancel) {}
			} message: { report in
				message(report)
			}
		}

		/// Shown while a report is waiting; taking it when the alert closes brings up the next one
		private var isPresented: Binding<Bool> {
			Binding(
				get: { !coordinator.leftovers.isEmpty },
				set: { shown in
					if !shown, !coordinator.leftovers.isEmpty {
						coordinator.leftovers.removeFirst()
					}
				})
		}

		@ViewBuilder private func message(_ report: FilesLeftovers.Report) -> some View {
			switch report.kind {
			case .movedOut(let count, let example, let isComplete, let place):
				if isComplete {
					Text(
						"Synctrain cannot synchronize \(count) of the files in \(report.folder), such as \(example), so they never reached another device. They are now in \(place), where you can move or remove them."
					)
				}
				else {
					Text(
						"Synctrain cannot synchronize more than \(count) of the files in \(report.folder), such as \(example), so they never reached another device. They are now in \(place), where you can move or remove them."
					)
				}
			case .partlyMovedOut(let moved, let count, let example, let place):
				Text(
					"Synctrain cannot synchronize \(count) of the files in \(report.folder), such as \(example), so they never reached another device. \(moved) of them are now in \(place). The others are still in Finder's Synctrain location, so the folder stays until they are moved."
				)
			case .staying(let count, let example):
				Text(
					"Synctrain cannot synchronize \(count) of the files in \(report.folder), such as \(example), so they never reached another device. They are still in Finder's Synctrain location, so the folder stays until they are moved."
				)
			case .movedAside(let name):
				Text(
					"Files of \(report.folder) that Synctrain cannot synchronize were still in Finder's Synctrain location. They are now in \(name), next to the folder, where you can move or remove them."
				)
			case .keptBySystem:
				Text(
					"macOS kept files from Finder's Synctrain location that never reached a gateway. They are in \((report.url.path(percentEncoded: false) as NSString).abbreviatingWithTildeInPath), where you can move or remove them."
				)
			}
		}
	}
#endif
