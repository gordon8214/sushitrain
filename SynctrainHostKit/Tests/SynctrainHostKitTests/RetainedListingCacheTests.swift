// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Testing
@testable import SynctrainHostKit

@Test func listingCacheEvictsLeastRecentlyUsedMetadata() {
	var cache = RetainedListingCache<String, [Int]>(maximumCount: 2, maximumCost: 10)
	cache.insert([1], for: "first", cost: 1)
	cache.insert([2], for: "second", cost: 1)
	#expect(cache.value(for: "first") == [1])
	#expect(cache.insert([3], for: "third", cost: 1) == [[2]])
	#expect(cache.value(for: "second") == nil)
	#expect(cache.value(for: "first") == [1])
	#expect(cache.count == 2)
}

@Test func listingCacheBoundsItemsAcrossDirectoriesAndReplacesCosts() {
	var cache = RetainedListingCache<String, String>(maximumCount: 8, maximumCost: 10)
	cache.insert("a", for: "a", cost: 6)
	#expect(cache.insert("b", for: "b", cost: 6) == ["a"])
	cache.insert("updated", for: "b", cost: 2)
	#expect(cache.totalCost == 2)
	cache.insert("c", for: "c", cost: 7)
	#expect(cache.totalCost == 9)
	#expect(cache.count == 2)
	// One exceptionally large directory is still usable; older metadata is released.
	#expect(Set(cache.insert("large", for: "large", cost: 30)) == Set(["updated", "c"]))
	#expect(cache.count == 1)
	#expect(cache.value(for: "large") == "large")
	#expect(cache.totalCost == 30)
}

@Test func listingCacheReleasesMetadataUnderMemoryPressure() {
	var cache = RetainedListingCache<Int, String>()
	cache.insert("contents", for: 1, cost: 500)
	#expect(cache.removeAll() == ["contents"])
	#expect(cache.count == 0)
	#expect(cache.totalCost == 0)
	#expect(cache.value(for: 1) == nil)
	cache.insert("reloaded", for: 1, cost: 3)
	#expect(cache.value(for: 1) == "reloaded")
}
