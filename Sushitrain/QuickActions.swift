// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import UIKit
#elseif os(macOS)
	import AppKit
	import UserNotifications
	import os
#endif

import SwiftUI
@preconcurrency import SushitrainCore
import SynctrainHostKit

/// What the app's actions ask to see. They run outside any window, and can run before there is one, so a route waits here
/// until a window takes it.
@MainActor @Observable
class QuickActionService {
	static let shared = QuickActionService()
	var action: Route?
	private var finderActions = FinderActionQueue()
	private var finderWindowActions = FinderActionQueue()
	var finderRequest: FinderActionRequest? { finderActions.next }
	var finderWindowRequest: FinderActionRequest? { finderWindowActions.next }
	var presentedFinderWindowRequest: FinderActionRequest?
	func enqueueFinderActions(_ actions: [FinderAction]) {
		finderActions.enqueue(actions.filter { $0.windowAction == nil })
		finderWindowActions.enqueue(actions.compactMap(\.windowAction))
	}
	func claimFinderWindowRequest(_ id: UUID) -> Bool { finderWindowActions.claim(id) }
	func finishFinderWindowRequest(_ id: UUID) { finderWindowActions.finish(id) }
	func claimFinderRequest(_ id: UUID) -> Bool { finderActions.claim(id) }
	func finishFinderRequest(_ id: UUID) { finderActions.finish(id) }

	#if os(macOS)
		/// The windows that can show a route, also while the engine starts and they can't do so yet
		private let windows = NSHashTable<NSWindow>.weakObjects()

		func add(_ window: NSWindow) {
			windows.add(window)
		}

		func remove(_ window: NSWindow) {
			windows.remove(window)
		}

		/// The one window that takes an action: the frontmost, which is the one the user was in last. A window with a sheet
		/// is passed over, as moving away from what opened the sheet would discard what is being edited in it.
		var target: NSWindow? {
			NSApp.orderedWindows.first { windows.contains($0) && $0.attachedSheet == nil }
		}

		/// A notification was clicked. The connection requests have a window of their own, so this is no route.
		var wantsConnectionRequests = false
		var hasPresentedFinderWindow = false
	#endif
}

#if os(macOS)
	@MainActor
	class AppDelegate: NSObject, NSApplicationDelegate {
		weak var appState: AppState?
		let incomingFiles = IncomingFiles()
		var openWindow: OpenWindowAction? {
			didSet { presentIncomingFiles() }
		}

		func applicationWillFinishLaunching(_ notification: Notification) {
			// Any later and the click that started the app is lost
			UNUserNotificationCenter.current().delegate = self
			// Handle URL events directly: SwiftUI otherwise also opens a browser scene for every Finder action.
			NSAppleEventManager.shared().setEventHandler(
				self, andSelector: #selector(receiveURL(_:withReplyEvent:)),
				forEventClass: AEEventClass(kInternetEventClass), andEventID: AEEventID(kAEGetURL))
		}

		@objc private func receiveURL(_ event: NSAppleEventDescriptor, withReplyEvent reply: NSAppleEventDescriptor) {
			guard let value = event.paramDescriptor(forKeyword: AEKeyword(keyDirectObject))?.stringValue,
				let url = URL(string: value)
			else { return }
			application(NSApplication.shared, open: [url])
		}

		func application(_ application: NSApplication, open urls: [URL]) {
			Log.info("Received \(urls.count) URLs for import")
			for url in urls {
				if let action = FinderAction(url: url) {
					QuickActionService.shared.enqueueFinderActions([action])
				}
				else if url.scheme == Route.urlScheme {
					QuickActionService.shared.action = Route(url: url)
				}
				else {
					incomingFiles.receive(url)
				}
			}
			presentIncomingFiles()
		}

		private func presentIncomingFiles() {
			guard !incomingFiles.urls.isEmpty, let openWindow else { return }
			openWindow(id: "import")
			NSApplication.shared.activate()
		}

		/// The longest the app waits for the engine to stop when quitting; what doesn't finish is recovered at the next start
		private static let engineStopTimeout: TimeInterval = 20
		private var isTerminating = false

		func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
			if isTerminating {
				return .terminateLater
			}
			isTerminating = true
			appState?.isQuitting = true
			// Not when the app has gone: quitting can't be undone from here on, and a crash while stopping the engine has
			// no business opening the app again
			CrashWatch.shared.stop()
			// The remote file service stops first, then the engine, off the main thread. Both are time-limited; what they
			// could not finish is recovered at the next start.
			let client = appState?.client
			Task {
				await FinderIntegration.shared.stop()
				await GatewayController.shared.stopBeforeQuitting()
				if let client {
					if !(await Self.stop(client, within: Self.engineStopTimeout)) {
						Log.warn("The engine did not stop in time")
					}
				}
				NSApplication.shared.reply(toApplicationShouldTerminate: true)
			}
			return .terminateLater
		}

		/// Stops the engine on a thread of its own, and reports whether it stopped within the time given
		@concurrent nonisolated private static func stop(_ client: SushitrainClient, within timeout: TimeInterval) async
			-> Bool
		{
			await withCheckedContinuation { continuation in
				let pending = OSAllocatedUnfairLock<CheckedContinuation<Bool, Never>?>(initialState: continuation)
				let finish: @Sendable (Bool) -> Void = { stopped in
					let waiting = pending.withLock { value in
						defer { value = nil }
						return value
					}
					waiting?.resume(returning: stopped)
				}
				DispatchQueue.global(qos: .userInitiated).async {
					client.stop()
					finish(true)
				}
				DispatchQueue.global(qos: .userInitiated).asyncAfter(deadline: .now() + timeout) {
					finish(false)
				}
			}
		}

		func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
			// A Finder window launch suppresses the browser, including SwiftUI's later Dock reopen behavior.
			if FinderItemWindow.isLaunch {
				if QuickActionService.shared.hasPresentedFinderWindow && !hasVisibleWindows,
					!NSApp.windows.contains(where: \.isMiniaturized)
				{
					openWindow?(id: "main")
					NSApp.activate()
				}
				return false
			}
			// This window shows why the engine didn't start, where the menu bar's own doesn't
			return AppPresence.reopen(hasVisibleWindows: hasVisibleWindows) { openWindow?(id: "main") }
		}

		func applicationDidResignActive(_ notification: Notification) {
			AppPresence.restoreActivationPolicy()
		}

		func applicationDockMenu(_ sender: NSApplication) -> NSMenu? {
			guard let routes = appState?.bookmarkedRoutesAsRoute, !routes.isEmpty else { return nil }
			let menu = NSMenu()
			for route in routes {
				let item = NSMenuItem(title: route.localizedTitle, action: #selector(selectBookmark(_:)), keyEquivalent: "")
				item.target = self
				item.representedObject = route.url
				item.toolTip = route.localizedSubtitle
				item.image = NSImage(systemSymbolName: "bookmark", accessibilityDescription: nil)
				menu.addItem(item)
			}
			return menu
		}

		@objc private func selectBookmark(_ sender: NSMenuItem) {
			guard let url = sender.representedObject as? URL else { return }
			openWindow?(id: "bookmark", value: url)
			NSApplication.shared.activate()
		}
	}

	extension AppDelegate: UNUserNotificationCenterDelegate {
		/// Without this, the system keeps a notification to itself while the app is in front
		nonisolated func userNotificationCenter(
			_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
			withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
		) {
			completionHandler([.banner, .list])
		}

		/// The app's only notifications are about connection requests
		nonisolated func userNotificationCenter(
			_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
			withCompletionHandler completionHandler: @escaping () -> Void
		) {
			Task { @MainActor in
				QuickActionService.shared.wantsConnectionRequests = true
			}
			completionHandler()
		}
	}
#elseif os(iOS)
	// Glue code for quick actions
	class AppDelegate: NSObject, UIApplicationDelegate {
		private let qaService = QuickActionService.shared

		func application(
			_ application: UIApplication, configurationForConnecting connectingSceneSession: UISceneSession,
			options: UIScene.ConnectionOptions
		) -> UISceneConfiguration {
			if let shortcutItem = options.shortcutItem, let url = URL(string: shortcutItem.type) {
				qaService.action = Route(url: url)
			}

			let configuration = UISceneConfiguration(
				name: connectingSceneSession.configuration.name,
				sessionRole: connectingSceneSession.role)
			configuration.delegateClass = SceneDelegate.self
			return configuration
		}

		/// Background transfers of folders on gateways finished while the app was not running: the app's own session,
		/// or the File Provider extension's (which the app recreates only to finish its events). The system's handler is
		/// called on the main thread, exactly once, after the events were handled.
		func application(
			_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String,
			completionHandler: @escaping () -> Void
		) {
			let completion: @MainActor @Sendable () -> Void = { completionHandler() }
			let handled = RemoteCoordinator.shared.handleBackgroundEvents(identifier: identifier, completion: completion)
			if !handled {
				Log.warn("Events for unknown background session \(identifier)")
				completionHandler()
			}
		}
	}

	class SceneDelegate: NSObject, UIWindowSceneDelegate {
		private let qaService = QuickActionService.shared

		func scene(
			_ scene: UIScene, willConnectTo session: UISceneSession,
			options connectionOptions: UIScene.ConnectionOptions
		) {
			if let shortcutItem = connectionOptions.shortcutItem, let url = URL(string: shortcutItem.type) {
				qaService.action = Route(url: url)
			}
		}

		func windowScene(
			_ windowScene: UIWindowScene, performActionFor shortcutItem: UIApplicationShortcutItem,
			completionHandler: @escaping (Bool) -> Void
		) {
			if let url = URL(string: shortcutItem.type) {
				qaService.action = Route(url: url)
			}
			completionHandler(true)
		}
	}
#endif
