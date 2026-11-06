// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
/// A window's recently used directory listings, bounded by both listing and item counts.
/// The newest listing is retained even when it alone exceeds the item budget.
public struct RetainedListingCache<Key: Hashable, Value> {
	private struct Entry {
		var value: Value
		var cost: Int
	}
	private var entries: [Key: Entry] = [:]
	private var recency: [Key] = []
	private let maximumCount: Int
	private let maximumCost: Int
	public private(set) var totalCost = 0
	public var count: Int { entries.count }

	public init(maximumCount: Int = 8, maximumCost: Int = 20_000) {
		precondition(maximumCount > 0 && maximumCost > 0)
		self.maximumCount = maximumCount
		self.maximumCost = maximumCost
	}

	public mutating func value(for key: Key) -> Value? {
		guard let entry = entries[key] else { return nil }
		recency.removeAll { $0 == key }
		recency.append(key)
		return entry.value
	}

	/// Returns evicted values so owners can release metadata held in observable view state.
	@discardableResult public mutating func insert(_ value: Value, for key: Key, cost: Int) -> [Value] {
		totalCost -= entries[key]?.cost ?? 0
		entries[key] = Entry(value: value, cost: max(0, cost))
		totalCost += max(0, cost)
		recency.removeAll { $0 == key }
		recency.append(key)
		var evicted: [Value] = []
		while entries.count > maximumCount || (totalCost > maximumCost && entries.count > 1) {
			let oldest = recency.removeFirst()
			if let entry = entries.removeValue(forKey: oldest) {
				totalCost -= entry.cost
				evicted.append(entry.value)
			}
		}
		return evicted
	}

	public mutating func removeAll() -> [Value] {
		let values = entries.values.map(\.value)
		entries.removeAll()
		recency.removeAll()
		totalCost = 0
		return values
	}
}
