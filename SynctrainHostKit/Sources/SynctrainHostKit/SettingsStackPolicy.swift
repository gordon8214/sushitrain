// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0

/// Protects the pages being removed while retaining drafts on their ancestors.
public struct SettingsStackPolicy<Page: Hashable & Sendable>: Sendable {
	public private(set) var path: [Page] = []
	public private(set) var pendingPath: [Page]?
	public init() {}

	@discardableResult public mutating func request(_ next: [Page], dirtyPages: Set<Page>, isSaving: Bool) -> Bool {
		guard next != path, !isSaving else { return false }
		let common = zip(path, next).prefix { pair in pair.0 == pair.1 }.count
		if path.dropFirst(common).contains(where: dirtyPages.contains) {
			pendingPath = next
			return false
		}
		path = next
		pendingPath = nil
		return true
	}

	@discardableResult public mutating func discardChanges(isSaving: Bool) -> Bool {
		guard !isSaving, let pendingPath else { return false }
		path = pendingPath
		self.pendingPath = nil
		return true
	}

	public mutating func keepEditing() { pendingPath = nil }
	public mutating func reset() {
		path = []
		pendingPath = nil
	}
}
