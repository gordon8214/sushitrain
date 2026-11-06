// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import Testing
@testable import SynctrainHostKit

@Test func settingsStackBackProtectsOnlyDepartingDrafts() {
	var stack = SettingsStackPolicy<String>()
	let navigationResult1 = stack.request(["network", "proxy"], dirtyPages: [], isSaving: false)
	#expect(navigationResult1)
	let navigationResult2 = stack.request(["network"], dirtyPages: ["network"], isSaving: false)
	#expect(navigationResult2)
	#expect(stack.path == ["network"])
	let navigationResult3 = stack.request([], dirtyPages: ["network"], isSaving: false)
	#expect(!navigationResult3)
	#expect(stack.path == ["network"] && stack.pendingPath == [])
	stack.keepEditing()
	#expect(stack.pendingPath == nil && stack.path == ["network"])
	let navigationResult4 = stack.request([], dirtyPages: ["network"], isSaving: false)
	#expect(!navigationResult4)
	let navigationResult5 = stack.discardChanges(isSaving: false)
	#expect(navigationResult5)
	#expect(stack.path.isEmpty)
}

@Test func settingsStackSavingBlocksPushBackAndDiscard() {
	var stack = SettingsStackPolicy<String>()
	let navigationResult6 = stack.request(["advanced"], dirtyPages: [], isSaving: false)
	#expect(navigationResult6)
	let navigationResult7 = stack.request(["advanced", "gui"], dirtyPages: [], isSaving: true)
	#expect(!navigationResult7)
	let navigationResult8 = stack.request([], dirtyPages: ["advanced"], isSaving: true)
	#expect(!navigationResult8)
	#expect(stack.pendingPath == nil)
	let navigationResult9 = stack.request([], dirtyPages: ["advanced"], isSaving: false)
	#expect(!navigationResult9)
	let navigationResult10 = stack.discardChanges(isSaving: true)
	#expect(!navigationResult10)
	#expect(stack.path == ["advanced"])
	stack.reset()
	#expect(stack.path.isEmpty && stack.pendingPath == nil)
}

@Test func settingsStackReplacementPreservesTheCommonAncestor() {
	var stack = SettingsStackPolicy<String>()
	let navigationResult11 = stack.request(["advanced", "gui"], dirtyPages: [], isSaving: false)
	#expect(navigationResult11)
	let navigationResult12 = stack.request(["advanced", "configuration"], dirtyPages: ["gui"], isSaving: false)
	#expect(!navigationResult12)
	let navigationResult13 = stack.discardChanges(isSaving: false)
	#expect(navigationResult13)
	#expect(stack.path == ["advanced", "configuration"])
}
