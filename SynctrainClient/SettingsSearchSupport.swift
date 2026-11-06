// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import SwiftUI
import SynctrainHostKit

extension EnvironmentValues {
	@Entry var settingsSearchAnchor: String? = nil
	@Entry var settingsSearchScroll: ScrollViewProxy? = nil
}

struct SettingsSearchReveal<Content: View>: View {
	let anchor: String?
	@ViewBuilder var content: () -> Content
	var body: some View {
		ScrollViewReader { proxy in
			content().environment(\.settingsSearchAnchor, anchor).environment(\.settingsSearchScroll, proxy)
		}
	}
}

private struct SettingsSearchAnchor: ViewModifier {
	let anchor: String
	@Environment(\.settingsSearchAnchor) private var target
	@Environment(\.settingsSearchScroll) private var scroll
	@FocusState private var focused: Bool
	@AccessibilityFocusState private var accessibilityFocused: Bool
	func body(content: Content) -> some View {
		content.id(anchor).focused($focused).accessibilityFocused($accessibilityFocused)
			.accessibilityIdentifier("settings.control.\(anchor)")
			.task(id: target) {
				guard target?.lowercased() == anchor.lowercased() else { return }
				await Task.yield()
				guard !Task.isCancelled else { return }
				scroll?.scrollTo(anchor, anchor: .center)
				focused = true
				accessibilityFocused = true
			}
	}
}

extension View {
	func settingsAnchor(_ anchor: String) -> some View { modifier(SettingsSearchAnchor(anchor: anchor)) }
}

struct SettingsSearchResultLabel: View {
	let entry: SettingSearchEntry
	var body: some View {
		VStack(alignment: .leading, spacing: 3) {
			Text(entry.localizedTitle)
			Text(entry.localizedBreadcrumb).font(.caption).foregroundStyle(.secondary)
		}
		.accessibilityElement(children: .combine)
	}
}
