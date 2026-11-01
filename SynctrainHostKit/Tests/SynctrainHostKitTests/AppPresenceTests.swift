// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

/// A suite named by a path keeps out of the user's preferences, where even a removed one leaves a file
private func withEmptyDefaults(_ body: (UserDefaults) throws -> Void) throws {
	let directory = FileManager.default.temporaryDirectory.appending(
		path: "AppPresenceTests-\(UUID().uuidString)", directoryHint: .isDirectory)
	try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
	defer { try? FileManager.default.removeItem(at: directory) }
	try body(try #require(UserDefaults(suiteName: directory.appending(path: "defaults").path(percentEncoded: false))))
}

@Test func takingTheAppOutOfTheMenuBarKeepsItInTheDock() {
	var presence = AppPresence.menuBarAndDock
	presence.inMenuBar = false
	#expect(presence == .dock)
}

@Test func takingTheAppOutOfTheDockKeepsItInTheMenuBar() {
	var presence = AppPresence.menuBarAndDock
	presence.inDock = false
	#expect(presence == .menuBar)
}

@Test func takingTheAppOutOfTheOnlyPlaceItIsPutsItInTheOther() {
	var presence = AppPresence.menuBar
	presence.inMenuBar = false
	#expect(presence == .dock)

	presence.inDock = false
	#expect(presence == .menuBar)
}

@Test func addingAPlaceKeepsTheOther() {
	var presence = AppPresence.menuBar
	presence.inDock = true
	#expect(presence == .menuBarAndDock)

	presence = .dock
	presence.inMenuBar = true
	#expect(presence == .menuBarAndDock)
}

@Test func settingWhatIsAlreadySoChangesNothing() {
	var presence = AppPresence.menuBar
	presence.inMenuBar = true
	#expect(presence == .menuBar)

	presence = .dock
	presence.inDock = true
	#expect(presence == .dock)
}

@Test func theAppIsInBothPlacesUntilToldOtherwise() throws {
	try withEmptyDefaults { defaults in
		#expect(AppPresence.stored(in: defaults) == .menuBarAndDock)

		defaults.set("somewhere", forKey: AppPresence.storageKey)
		#expect(AppPresence.stored(in: defaults) == .menuBarAndDock)

		defaults.set(AppPresence.dock.rawValue, forKey: AppPresence.storageKey)
		#expect(AppPresence.stored(in: defaults) == .dock)
	}
}

@Test func aHiddenDockIconStaysHiddenWithItsMenuBarItem() throws {
	try withEmptyDefaults { defaults in
		defaults.set(true, forKey: "hideInDock")
		AppPresence.adoptHideInDock(in: defaults)
		#expect(defaults.string(forKey: AppPresence.storageKey) == AppPresence.menuBar.rawValue)
		#expect(defaults.object(forKey: "hideInDock") == nil)
	}
}

@Test func aDockIconShownByChoiceStaysWithoutAMenuBarItem() throws {
	try withEmptyDefaults { defaults in
		defaults.set(false, forKey: "hideInDock")
		AppPresence.adoptHideInDock(in: defaults)
		#expect(defaults.string(forKey: AppPresence.storageKey) == AppPresence.dock.rawValue)
		#expect(defaults.object(forKey: "hideInDock") == nil)
	}
}

@Test func withoutTheOldSettingNothingIsStored() throws {
	try withEmptyDefaults { defaults in
		AppPresence.adoptHideInDock(in: defaults)
		#expect(defaults.object(forKey: AppPresence.storageKey) == nil)
	}
}

@Test func theNewSettingWinsOverTheOldOne() throws {
	try withEmptyDefaults { defaults in
		defaults.set(AppPresence.menuBarAndDock.rawValue, forKey: AppPresence.storageKey)
		defaults.set(true, forKey: "hideInDock")
		AppPresence.adoptHideInDock(in: defaults)
		#expect(defaults.string(forKey: AppPresence.storageKey) == AppPresence.menuBarAndDock.rawValue)
		#expect(defaults.object(forKey: "hideInDock") == nil)
	}
}
