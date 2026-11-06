// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import SwiftUI
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	/// Browser sessions belong to the window, so preference navigation cannot discard their state.
	@MainActor final class DesktopBrowserNavigation {
		private var sessions: [Route: DesktopBrowserSession] = [:]
		private let listings = DesktopBrowserListings()
		func session(for route: Route) -> DesktopBrowserSession {
			if let session = sessions[route] { return session }
			let session = DesktopBrowserSession(listings: listings)
			sessions[route] = session
			return session
		}
		func deactivate() { for session in sessions.values { session.active = false } }
	}

	@Observable @MainActor final class DesktopBrowserSettings {
		var searchText = ""
		var viewStyle: BrowserViewStyle?
		var sort: BrowserViewSort?
		var recursive = false
		var searching = false
	}

	struct DesktopBrowserItems {
		let directories, files: [SushitrainEntry]
		let exists, paused: Bool
	}

	struct DesktopBrowserTableState {
		let selection: Set<SushitrainEntry.ID>
		let sort: [EntryComparator]
		let scroll: SushitrainEntry.ID?
	}

	@MainActor final class DesktopBrowserListings {
		var cache = RetainedListingCache<Route, DesktopBrowserItems>()
	}

	@Observable @MainActor final class DesktopBrowserSession {
		var path: [Route] = []
		var active = true
		@ObservationIgnored private var settings: [Route: DesktopBrowserSettings] = [:]
		func settings(for route: Route) -> DesktopBrowserSettings {
			if let state = settings[route] { return state }
			let state = DesktopBrowserSettings()
			settings[route] = state
			return state
		}
		@ObservationIgnored private let listings: DesktopBrowserListings
		init(listings: DesktopBrowserListings) { self.listings = listings }
		func items(for route: Route) -> DesktopBrowserItems? { listings.cache.value(for: route) }
		func retain(_ items: DesktopBrowserItems, for route: Route) {
			listings.cache.insert(items, for: route, cost: items.directories.count + items.files.count)
		}
		@ObservationIgnored var gridOffsets: [Route: CGFloat] = [:]
		@ObservationIgnored var tables: [Route: DesktopBrowserTableState] = [:]
	}

	struct BrowserDirectoryLink<Label: View>: View {
		let route: Route
		@ViewBuilder var label: () -> Label
		@Environment(\.desktopBrowserSession) private var session
		var body: some View {
			if session != nil {
				NavigationLink(value: route, label: label)
			}
			else {
				NavigationLink(destination: RouteView(route: route), label: label)
			}
		}
	}

	struct BrowserRouteAction {
		let call: (Route) -> Void
		func callAsFunction(_ route: Route) { call(route) }
	}

	extension EnvironmentValues {
		@Entry var desktopBrowserSession: DesktopBrowserSession? = nil
		@Entry var pushBrowserRoute: BrowserRouteAction? = nil
	}

	struct DesktopRetainedBrowser: View {
		let route: Route
		@State private var session: DesktopBrowserSession
		init(route: Route, navigation: DesktopBrowserNavigation) {
			self.route = route
			_session = State(initialValue: navigation.session(for: route))
		}
		var body: some View {
			NavigationStack(path: Binding(get: { session.path }, set: { if session.active { session.path = $0 } })) {
				RouteView(route: route)
					.navigationDestination(for: Route.self) { route in RouteView(route: route) }
			}
			.environment(\.desktopBrowserSession, session)
			.environment(\.pushBrowserRoute, BrowserRouteAction(call: { session.path.append($0) }))
			.onAppear { session.active = true }
		}
	}
#endif
