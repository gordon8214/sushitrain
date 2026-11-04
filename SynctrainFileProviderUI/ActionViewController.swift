// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
@preconcurrency import FileProviderUI
import SwiftUI
import SynctrainFileProviderKit
import os

let log = Logger(subsystem: "nl.t-shaped.Sushitrain.FileProvider", category: "Versions")

/// Presents the Files app's actions that need a user interface (Previous Versions, for files of gateway folders), and
/// explains the authentication errors the File Provider reports
final class ActionViewController: FPUIActionExtensionViewController {
	private var isPrepared = false

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
		guard !isPrepared else { return }
		isPrepared = true
		Task {
			guard let identifier = itemIdentifiers.first, let domain = context.domainIdentifier,
				let itemID = await Self.itemID(of: identifier, in: domain)
			else {
				context.cancelRequest(withError: Self.error(.failed))
				return
			}
			host(GatewayHistoryView(itemID: itemID) { context.completeRequest() })
		}
	}

	override func prepare(forError error: any Error) {
		// The File Provider reports that a gateway no longer accepts this device (it is not paired anymore): its
		// message says what to do
		let context = extensionContext
		host(ErrorView(message: error.localizedDescription) { context.cancelRequest(withError: Self.error(.userCancelled)) })
	}

	@concurrent private nonisolated static func itemID(
		of identifier: NSFileProviderItemIdentifier, in domainIdentifier: NSFileProviderDomainIdentifier
	) async -> Int64? {
		if let itemID = ItemIdentifiers.itemID(from: identifier.rawValue) {
			return itemID
		}
		do {
			guard
				let domain = try await NSFileProviderManager.domains().first(where: { $0.identifier == domainIdentifier }),
				let manager = NSFileProviderManager(for: domain)
			else {
				return nil
			}
			let url = try await manager.getUserVisibleURL(for: identifier)
			let (resolved, _) = try await NSFileProviderManager.identifierForUserVisibleFile(at: url)
			return ItemIdentifiers.itemID(from: resolved.rawValue)
		}
		catch {
			log.warning(
				"Could not resolve \(identifier.rawValue, privacy: .public): \(error.localizedDescription, privacy: .public)")
			return nil
		}
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
