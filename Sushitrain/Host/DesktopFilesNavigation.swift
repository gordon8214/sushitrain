// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Observation
	import SwiftUI
	import SynctrainFileProviderKit

	struct DesktopFilesDirectory: Hashable {
		let folderID: String
		let parentID: Int64
		let title: String
	}

	@Observable @MainActor final class DesktopFilesNavigation {
		let environment: RemoteEnvironment?
		init(environment: RemoteEnvironment? = .shared) { self.environment = environment }
		var paths: [String: [DesktopFilesDirectory]] = [:]
		@ObservationIgnored private var browsers: [Int64: DesktopFilesBrowserState] = [:]
		func browser(parentID: Int64) -> DesktopFilesBrowserState {
			if let state = browsers[parentID] { return state }
			let state = DesktopFilesBrowserState()
			browsers[parentID] = state
			return state
		}
	}

	@Observable @MainActor final class DesktopFilesBrowserState {
		var items: [IndexItem] = []
		var selection: Int64?
		var filter = ""
		var loading = false
		var hasMore = false
		var lastID: Int64 = 0
		var problem: String?
		var generation = 0
		var loadedRevision: Int?
		var scrollItem: Int64?
	}
#endif
