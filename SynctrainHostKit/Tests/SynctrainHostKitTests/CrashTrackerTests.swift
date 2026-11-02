// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

/// A suite named by a path keeps out of the user's preferences, where even a removed one leaves a file
private func withTracker(_ body: (CrashTracker) throws -> Void) throws {
	let directory = FileManager.default.temporaryDirectory.appending(
		path: "CrashTrackerTests-\(UUID().uuidString)", directoryHint: .isDirectory)
	try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
	defer { try? FileManager.default.removeItem(at: directory) }
	let defaults = try #require(UserDefaults(suiteName: directory.appending(path: "defaults").path(percentEncoded: false)))
	try body(CrashTracker(defaults: defaults))
}

private let macStarted = Date(timeIntervalSince1970: 1_000_000)

@Test func aFirstRunIsNoCrash() throws {
	try withTracker { tracker in
		#expect(tracker.runStarted(at: macStarted + 60, macStartedAt: macStarted) == nil)
		#expect(tracker.notice == nil)
	}
}

@Test func quittingIsNoCrash() throws {
	try withTracker { tracker in
		_ = tracker.runStarted(at: macStarted + 60, macStartedAt: macStarted)
		tracker.runEnded()

		#expect(tracker.runStarted(at: macStarted + 120, macStartedAt: macStarted) == nil)
		#expect(tracker.notice == nil)
		#expect(tracker.relaunchDelay(at: macStarted + 120) == CrashTracker.firstDelay)
	}
}

@Test func aRunThatLeftItsMarkCrashed() throws {
	try withTracker { tracker in
		let started = macStarted + 60
		let again = started + 300
		_ = tracker.runStarted(at: started, macStartedAt: macStarted)

		#expect(tracker.runStarted(at: again, macStartedAt: macStarted) == started)
		#expect(tracker.notice == CrashNotice(count: 1, startedAgain: again))
	}
}

@Test func aRunThatEndedWithTheMacDidNotCrash() throws {
	try withTracker { tracker in
		_ = tracker.runStarted(at: macStarted + 60, macStartedAt: macStarted)
		let restarted = macStarted + 3600

		#expect(tracker.runStarted(at: restarted + 30, macStartedAt: restarted) == nil)
		#expect(tracker.notice == nil)
		#expect(tracker.recentCrashes(at: restarted + 30).isEmpty)
	}
}

@Test func theDelayDoublesWithEachCrashAndStopsAtTheLimit() throws {
	try withTracker { tracker in
		var now = macStarted + 60
		_ = tracker.runStarted(at: now, macStartedAt: macStarted)
		#expect(tracker.relaunchDelay(at: now) == 10)

		for expected: TimeInterval in [20, 40, 80, 160] {
			now += 30
			#expect(tracker.runStarted(at: now, macStartedAt: macStarted) != nil)
			#expect(tracker.relaunchDelay(at: now) == expected)
		}

		now += 30
		#expect(tracker.runStarted(at: now, macStartedAt: macStarted) != nil)
		#expect(tracker.recentCrashes(at: now).count == CrashTracker.crashLimit)
		#expect(tracker.relaunchDelay(at: now) == nil)
	}
}

@Test func anHourAfterTheCrashesTheDelayIsBackToTheStart() throws {
	try withTracker { tracker in
		var now = macStarted + 60
		_ = tracker.runStarted(at: now, macStartedAt: macStarted)
		for _ in 0..<CrashTracker.crashLimit {
			now += 30
			_ = tracker.runStarted(at: now, macStartedAt: macStarted)
		}
		#expect(tracker.relaunchDelay(at: now) == nil)

		#expect(tracker.relaunchDelay(at: now + CrashTracker.window) == CrashTracker.firstDelay)
	}
}

@Test func aCrashLongAfterTheOthersCountsAlone() throws {
	try withTracker { tracker in
		let first = macStarted + 60
		_ = tracker.runStarted(at: first, macStartedAt: macStarted)
		_ = tracker.runStarted(at: first + 30, macStartedAt: macStarted)

		let later = first + 30 + CrashTracker.window
		_ = tracker.runStarted(at: later, macStartedAt: macStarted)
		#expect(tracker.recentCrashes(at: later) == [later])
		#expect(tracker.relaunchDelay(at: later) == 20)
	}
}

@Test func theNoticeAddsUpUntilItIsDismissed() throws {
	try withTracker { tracker in
		let first = macStarted + 60
		_ = tracker.runStarted(at: first, macStartedAt: macStarted)
		_ = tracker.runStarted(at: first + 30, macStartedAt: macStarted)
		_ = tracker.runStarted(at: first + 60, macStartedAt: macStarted)
		#expect(tracker.notice == CrashNotice(count: 2, startedAgain: first + 60))

		tracker.dismissNotice()
		#expect(tracker.notice == nil)

		// Dismissing it says nothing about how often the app crashes
		#expect(tracker.recentCrashes(at: first + 60).count == 2)
	}
}
