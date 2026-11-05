// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI

	/// Preserves SwiftUI's window delegate while guarding native close commands and the close button.
	struct SettingsWindowCloseGuard: NSViewRepresentable {
		let navigation: MacSettingsNavigation
		func makeCoordinator() -> Coordinator { Coordinator(navigation: navigation) }
		func makeNSView(context: Context) -> Observer {
			let view = Observer()
			view.attach = { [weak coordinator = context.coordinator] window in coordinator?.attach(window) }
			return view
		}
		func updateNSView(_ view: Observer, context: Context) { context.coordinator.navigation = navigation }

		final class Observer: NSView {
			var attach: ((NSWindow) -> Void)?
			override func viewDidMoveToWindow() {
				super.viewDidMoveToWindow()
				if let window { attach?(window) }
			}
		}

		final class Coordinator: NSObject, NSWindowDelegate {
			var navigation: MacSettingsNavigation
			private weak var previous: (any NSWindowDelegate)?
			init(navigation: MacSettingsNavigation) { self.navigation = navigation }
			func attach(_ window: NSWindow) {
				guard window.delegate !== self else { return }
				previous = window.delegate
				window.delegate = self
			}
			override func responds(to selector: Selector!) -> Bool {
				super.responds(to: selector) || previous?.responds(to: selector) == true
			}
			override func forwardingTarget(for selector: Selector!) -> Any? {
				previous?.responds(to: selector) == true ? previous : super.forwardingTarget(for: selector)
			}
			func windowShouldClose(_ sender: NSWindow) -> Bool {
				guard !navigation.isSaving else { return false }
				if navigation.hasChanges {
					let alert = NSAlert()
					alert.messageText = String(localized: "Discard unsaved changes?")
					alert.informativeText = String(localized: "The changes on this page have not been saved.")
					alert.addButton(withTitle: String(localized: "Keep Editing"))
					alert.addButton(withTitle: String(localized: "Discard Changes"))
					guard alert.runModal() == .alertSecondButtonReturn else { return false }
					navigation.discardForClosing()
				}
				return previous?.windowShouldClose?(sender) ?? true
			}
		}
	}
#endif
