// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Native SwiftUI checks; requires an unlocked macOS desktop and briefly changes focus.
// Compile with Sushitrain/Host/AppWindowPresentation.swift using swiftc -parse-as-library.
// Expected runtime: under 2 seconds; hard budget: 20 seconds.
import AppKit
import SwiftUI

@main struct WindowFocusChecks: App {
	var body: some Scene {
		Window("Focus check host", id: "host") {
			FocusCheckView()
		}
		Window("Focus check target", id: "target") {
			Text("Target").padding()
		}
		.defaultLaunchBehavior(.suppressed)
		WindowGroup("Folder", id: "folder", for: String.self) { value in
			Text(value.wrappedValue ?? "").padding().navigationTitle(value.wrappedValue ?? "Folder")
		}
		.defaultLaunchBehavior(.suppressed)
	}
}

struct FocusCheckView: View {
	@Environment(\.openWindow) private var openWindow
	var body: some View {
		Text("Running focus checks").padding().onAppear {
			Task { await FocusChecks.run(openWindow) }
		}
	}
}

@MainActor enum FocusChecks {
	static var started = false
	static func require(_ condition: @autoclosure () -> Bool, _ message: String) {
		guard condition() else {
			print("FAIL: \(message)")
			print(
				"active=\(NSApp.isActive) hidden=\(NSApp.isHidden) key=\(NSApp.keyWindow?.title ?? "nil") windows=\(NSApp.windows.map { "\($0.title): visible=\($0.isVisible) key=\($0.isKeyWindow) id=\($0.identifier?.rawValue ?? "nil")" })"
			)
			exit(1)
		}
		print("PASS: \(message)")
	}
	static func wait(_ condition: () -> Bool) async {
		let deadline = Date().addingTimeInterval(3)
		while !condition(), Date() < deadline {
			try? await Task.sleep(for: .milliseconds(10))
		}
	}
	static func trackMenu() {
		let timer = Timer(timeInterval: 0.02, repeats: false) { _ in }
		RunLoop.main.add(timer, forMode: .eventTracking)
		_ = RunLoop.main.run(mode: .eventTracking, before: Date().addingTimeInterval(0.03))
	}
	static func run(_ openWindow: OpenWindowAction) async {
		guard !started else { return }
		started = true
		DispatchQueue.global().asyncAfter(deadline: .now() + 20) {
			FileHandle.standardError.write(Data("Focus checks exceeded their 20-second budget\n".utf8))
			exit(1)
		}
		for policy in [NSApplication.ActivationPolicy.regular, .accessory] {
			NSApp.setActivationPolicy(policy)
			var presented = false
			AppWindowPresentation.open {
				presented = true
				openWindow(id: "target")
			}
			require(!presented, "\(policy): opening waits until the menu action returns")
			trackMenu()
			require(!presented, "\(policy): opening waits until menu tracking ends")
			await wait { presented && NSApp.isActive && NSApp.keyWindow?.title == "Focus check target" }
			require(
				presented && NSApp.isActive && NSApp.keyWindow?.title == "Focus check target",
				"\(policy): new or reused SwiftUI window receives focus")
			let target = NSApp.keyWindow
			NSApp.hide(nil)
			await wait { NSApp.isHidden }
			require(NSApp.isHidden, "\(policy): hidden-app setup")
			AppWindowPresentation.open { openWindow(id: "target") }
			await wait { !NSApp.isHidden && NSApp.isActive && NSApp.keyWindow === target }
			require(
				!NSApp.isHidden && NSApp.isActive && NSApp.keyWindow === target,
				"\(policy): reopening unhides and focuses the same window")
			AppWindowPresentation.open { openWindow(id: "folder", value: "Folder one") }
			await wait { NSApp.keyWindow?.title == "Folder one" }
			require(NSApp.keyWindow?.title == "Folder one", "\(policy): folder value window receives focus")
			let folder = NSApp.keyWindow
			AppWindowPresentation.open { openWindow(id: "target") }
			await wait { NSApp.keyWindow === target }
			AppWindowPresentation.open { openWindow(id: "folder", value: "Folder one") }
			await wait { NSApp.keyWindow === folder }
			require(NSApp.keyWindow === folder, "\(policy): reopening a folder focuses its existing window")
		}
		print("All native SwiftUI focus checks passed")
		exit(0)
	}
}
