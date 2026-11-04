// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

/// Decides when a settings category can change, independently of the editor's validation and Save button.
public struct SettingsNavigationPolicy<Category: Hashable & Sendable>: Sendable {
	public private(set) var selection: Category
	public private(set) var pendingSelection: Category?
	private let categories: Set<Category>

	public init(savedSelection: Category?, categories: Set<Category>, defaultSelection: Category) {
		precondition(categories.contains(defaultSelection))
		self.categories = categories
		selection = savedSelection.flatMap { categories.contains($0) ? $0 : nil } ?? defaultSelection
	}

	/// Returns true only when navigation actually happens. Saving blocks navigation, including discard confirmation.
	@discardableResult public mutating func request(
		_ category: Category, hasUnsavedChanges: Bool, isSaving: Bool
	) -> Bool {
		guard categories.contains(category), category != selection, !isSaving else { return false }
		if hasUnsavedChanges {
			pendingSelection = category
			return false
		}
		selection = category
		pendingSelection = nil
		return true
	}

	@discardableResult public mutating func discardChanges(isSaving: Bool) -> Bool {
		guard !isSaving, let pendingSelection else { return false }
		selection = pendingSelection
		self.pendingSelection = nil
		return true
	}

	public mutating func keepEditing() {
		pendingSelection = nil
	}
}
