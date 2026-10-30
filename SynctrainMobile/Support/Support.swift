// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import UIKit

extension Binding where Value == Bool {
	static func isNotNil<T: Sendable>(_ underlying: Binding<T?>) -> Binding<Bool> {
		return Binding(
			get: { underlying.wrappedValue != nil }, set: { underlying.wrappedValue = $0 ? underlying.wrappedValue : nil })
	}
}

extension Bundle {
	var releaseVersionNumber: String? {
		return self.infoDictionary?["CFBundleShortVersionString"] as? String
	}

	var buildVersionNumber: String? {
		return self.infoDictionary?["CFBundleVersion"] as? String
	}
}

/// The name gateways show for this device. The system only tells apps a generic name, so the user can set one.
enum DeviceName {
	static let storageKey = "deviceName"

	static var systemName: String {
		UIDevice.current.name
	}

	static var current: String {
		let name = UserDefaults.standard.string(forKey: storageKey)?.trimmingCharacters(in: .whitespacesAndNewlines)
		return name?.isEmpty == false ? name! : systemName
	}
}

/// Shows a location in the Files app
@MainActor func openInFilesApp(url: URL) {
	let sharedURL = url.absoluteString.replacingOccurrences(of: "file://", with: "shareddocuments://")
	if let url = URL(string: sharedURL) {
		UIApplication.shared.open(url, options: [:], completionHandler: nil)
	}
}
