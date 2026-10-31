// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import SwiftUI

	extension View {
		/// Tells the user what the Synctrain location keeps on this Mac because it cannot synchronize it, after a folder
		/// left Finder or came back to it (`RemoteCoordinator.leftovers`). The settings window carries this once: two of
		/// them would both try to show the same report.
		func filesLeftoversAlert() -> some View {
			modifier(FilesLeftoversAlert())
		}
	}

	private struct FilesLeftoversAlert: ViewModifier {
		private var coordinator = RemoteCoordinator.shared

		func body(content: Content) -> some View {
			content.alert(
				"Some files stay on this Mac", isPresented: isPresented, presenting: coordinator.leftovers.first
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
			case .staying(let count, let example, let isComplete):
				if isComplete {
					Text(
						"Synctrain cannot synchronize \(count) of the files in \(report.folder), such as \(example). They stay in Finder's Synctrain location, in a folder of the same name, where you can move or remove them."
					)
				}
				else {
					Text(
						"Synctrain cannot synchronize more than \(count) of the files in \(report.folder), such as \(example). They stay in Finder's Synctrain location, in a folder of the same name, where you can move or remove them."
					)
				}
			case .movedAside(let name):
				Text(
					"Files of \(report.folder) that Synctrain cannot synchronize were still in Finder's Synctrain location. They are now in \(name), next to the folder, where you can move or remove them."
				)
			}
		}
	}
#endif
