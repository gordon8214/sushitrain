// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import SwiftUI
import SynctrainHostKit
import SynctrainFileProviderKit

/// Browser state outlives the active page without keeping hidden navigation stacks in the hierarchy.
@Observable @MainActor final class MobileBrowserNavigation {
	var path: [ViewedDirectory] = []
	var searchPath: [ViewedDirectory] = []
	var listingRevision = 0
	var query = ""
	var searched = ""
	var results: [IndexItem] = []
	var folderScrollID: String?
	var searchScrollID: Int64?
	@ObservationIgnored private var listings = RetainedListingCache<Int64, MobileDirectoryState>()
	@ObservationIgnored private var directories: [Int64: MobileDirectoryState] = [:]

	func directory(_ id: Int64) -> MobileDirectoryState {
		if let state = directories[id] {
			_ = listings.value(for: id)
			return state
		}
		let state = MobileDirectoryState()
		directories[id] = state
		return state
	}

	func retainListing(for id: Int64) {
		guard let state = directories[id], let items = state.items else { return }
		for evicted in listings.insert(state, for: id, cost: items.count) { evicted.items = nil }
	}

	func releaseListings() {
		for state in listings.removeAll() { state.items = nil }
		listingRevision += 1
	}
}

@Observable @MainActor final class MobileDirectoryState {
	var items: [IndexItem]?
	var error: String?
	var scrollItem: Int64?
}
