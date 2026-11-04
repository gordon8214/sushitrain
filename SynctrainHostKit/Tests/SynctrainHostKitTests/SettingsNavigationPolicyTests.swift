// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Testing

@testable import SynctrainHostKit

private enum Category: Sendable {
	case general, network, gateways, finder
}

private func synchronizePolicy(saved: Category? = nil) -> SettingsNavigationPolicy<Category> {
	SettingsNavigationPolicy(savedSelection: saved, categories: [.general, .network], defaultSelection: .general)
}

@Test func settingsRestoreOnlyCategoriesAvailableInTheCurrentMode() {
	#expect(synchronizePolicy(saved: .network).selection == .network)
	#expect(synchronizePolicy(saved: .gateways).selection == .general)
	#expect(synchronizePolicy().selection == .general)
	let files = SettingsNavigationPolicy<Category>(
		savedSelection: .network, categories: [.general, .gateways, .finder], defaultSelection: .gateways)
	#expect(files.selection == .gateways)
}

@Test func cleanSettingsNavigateWithoutConfirmation() {
	var policy = synchronizePolicy()
	let didNavigate = policy.request(.network, hasUnsavedChanges: false, isSaving: false)
	#expect(didNavigate)
	#expect(policy.selection == .network)
	#expect(policy.pendingSelection == nil)
}

@Test func keepingEditsPreservesTheCurrentCategory() {
	var policy = synchronizePolicy()
	let didNavigate = policy.request(.network, hasUnsavedChanges: true, isSaving: false)
	#expect(!didNavigate)
	#expect(policy.selection == .general)
	#expect(policy.pendingSelection == .network)
	policy.keepEditing()
	#expect(policy.selection == .general)
	#expect(policy.pendingSelection == nil)
}

@Test func discardingAnInvalidDraftStillNavigatesToTheRequestedCategory() {
	// Validation is deliberately absent: even text that cannot be saved is an unsaved edit.
	var policy = synchronizePolicy()
	policy.request(.network, hasUnsavedChanges: true, isSaving: false)
	let didDiscard = policy.discardChanges(isSaving: false)
	#expect(didDiscard)
	#expect(policy.selection == .network)
	#expect(policy.pendingSelection == nil)
}

@Test func settingsCannotNavigateOrDiscardWhileSaving() {
	var policy = synchronizePolicy()
	let didNavigateWhileSaving = policy.request(.network, hasUnsavedChanges: false, isSaving: true)
	#expect(!didNavigateWhileSaving)
	#expect(policy.selection == .general)
	#expect(policy.pendingSelection == nil)
	policy.request(.network, hasUnsavedChanges: true, isSaving: false)
	let didDiscardWhileSaving = policy.discardChanges(isSaving: true)
	#expect(!didDiscardWhileSaving)
	#expect(policy.selection == .general)
	#expect(policy.pendingSelection == .network)
}

@Test func settingsIgnoreUnavailableAndAlreadySelectedCategories() {
	var policy = synchronizePolicy()
	let didSelectUnavailable = policy.request(.gateways, hasUnsavedChanges: false, isSaving: false)
	#expect(!didSelectUnavailable)
	let didSelectCurrent = policy.request(.general, hasUnsavedChanges: true, isSaving: false)
	#expect(!didSelectCurrent)
	#expect(policy.selection == .general)
	#expect(policy.pendingSelection == nil)
	let didDiscardWithoutRequest = policy.discardChanges(isSaving: false)
	#expect(!didDiscardWithoutRequest)
}
