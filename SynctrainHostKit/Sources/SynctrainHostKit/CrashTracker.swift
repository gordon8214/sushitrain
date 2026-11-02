// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// What the user is told about runs of the Mac app that ended without it quitting
public struct CrashNotice: Equatable, Sendable {
	/// How many runs ended that way since the user last dismissed the notice
	public let count: Int
	/// When the app started again after the last of them
	public let startedAgain: Date
}

/// What the Mac app keeps from one run to the next to tell how the last one ended, and how soon to open it again after a
/// crash. Nothing gets to note a crash as it happens, so each run leaves a mark that quitting removes: finding one at the
/// next start means that run ended in some other way, as a crash or a Force Quit does.
public struct CrashTracker {
	/// How long the app stays closed after a crash, as Syncthing.app does for its daemon. It doubles with each crash
	/// within `window`, as an app that crashes as it starts would otherwise spend the Mac's time doing just that.
	public static let firstDelay: TimeInterval = 10
	/// From this many crashes within `window`, the app isn't opened again. Something that keeps crashing needs a person.
	public static let crashLimit = 5
	public static let window: TimeInterval = 60 * 60

	private static let runStartKey = "crashTrackerRunStart"
	private static let crashesKey = "crashTrackerCrashes"
	private static let noticeCountKey = "crashTrackerNoticeCount"
	private static let noticeDateKey = "crashTrackerNoticeDate"

	private let defaults: UserDefaults

	public init(defaults: UserDefaults) {
		self.defaults = defaults
	}

	/// To be called first thing in a run, with when the Mac started. Returns when the last run started, if it ended
	/// unexpectedly. One from before the Mac started went down with it, which says nothing about the app.
	public func runStarted(at now: Date, macStartedAt: Date) -> Date? {
		let previous = defaults.object(forKey: Self.runStartKey) as? Date
		defaults.set(now, forKey: Self.runStartKey)
		defer { defaults.synchronize() }
		guard let previous, previous >= macStartedAt else {
			return nil
		}
		defaults.set(recentCrashes(at: now) + [now], forKey: Self.crashesKey)
		defaults.set(defaults.integer(forKey: Self.noticeCountKey) + 1, forKey: Self.noticeCountKey)
		defaults.set(now, forKey: Self.noticeDateKey)
		return previous
	}

	/// To be called when the app starts quitting, or exits by itself: what follows is no crash
	public func runEnded() {
		defaults.removeObject(forKey: Self.runStartKey)
		defaults.synchronize()
	}

	public func recentCrashes(at now: Date) -> [Date] {
		let crashes = defaults.array(forKey: Self.crashesKey) as? [Date] ?? []
		return crashes.filter { now.timeIntervalSince($0) < Self.window }
	}

	/// How long to wait before opening the app after a crash in this run, or nil to leave it closed
	public func relaunchDelay(at now: Date) -> TimeInterval? {
		let crashes = recentCrashes(at: now).count
		guard crashes < Self.crashLimit else {
			return nil
		}
		return Self.firstDelay * pow(2, Double(crashes))
	}

	/// What the user has not dismissed yet
	public var notice: CrashNotice? {
		guard let date = defaults.object(forKey: Self.noticeDateKey) as? Date else {
			return nil
		}
		return CrashNotice(count: max(defaults.integer(forKey: Self.noticeCountKey), 1), startedAgain: date)
	}

	public func dismissNotice() {
		defaults.removeObject(forKey: Self.noticeCountKey)
		defaults.removeObject(forKey: Self.noticeDateKey)
	}
}
