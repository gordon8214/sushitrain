// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
@preconcurrency import FileProviderUI
import SwiftUI
import os

let log = Logger(subsystem: "nl.t-shaped.Sushitrain.FileProvider", category: "Versions")

/// Presents the Files app's actions that need a user interface (Previous Versions, for files of gateway folders), and
/// explains the authentication errors the File Provider reports
final class ActionViewController: FPUIActionExtensionViewController {
	private var model: VersionsModel?

	#if os(macOS)
		// An NSViewController has no view of its own without a nib
		override func loadView() {
			view = NSView()
		}
	#endif

	override func prepare(forAction actionIdentifier: String, itemIdentifiers: [NSFileProviderItemIdentifier]) {
		let context = extensionContext
		guard actionIdentifier.hasSuffix(".versions") else {
			context.cancelRequest(withError: Self.error(.failed))
			return
		}
		guard model == nil else {
			return
		}
		VersionsModel.removeLeftoverDownloads()
		let model = VersionsModel(
			itemIdentifier: itemIdentifiers.first, domainIdentifier: context.domainIdentifier
		) {
			context.completeRequest()
		}
		self.model = model
		host(VersionsView(model: model))
	}

	override func prepare(forError error: any Error) {
		// The File Provider reports that a gateway no longer accepts this device (it is not paired anymore): its
		// message says what to do
		let context = extensionContext
		host(ErrorView(message: error.localizedDescription) { context.cancelRequest(withError: Self.error(.userCancelled)) })
	}

	private static func error(_ code: FPUIExtensionErrorCode) -> NSError {
		NSError(domain: FPUIErrorDomain, code: Int(code.rawValue))
	}

	private func host(_ content: some View) {
		guard children.isEmpty else {
			return
		}
		#if os(macOS)
			let host = NSHostingController(rootView: content)
			addChild(host)
			host.view.frame = view.bounds
			host.view.autoresizingMask = [.width, .height]
			view.addSubview(host.view)
			// Finder sizes its sheet to this
			preferredContentSize = CGSize(width: 480, height: 420)
		#else
			let host = UIHostingController(rootView: content)
			addChild(host)
			host.view.frame = view.bounds
			host.view.autoresizingMask = [.flexibleWidth, .flexibleHeight]
			view.addSubview(host.view)
			host.didMove(toParent: self)
		#endif
	}
}
