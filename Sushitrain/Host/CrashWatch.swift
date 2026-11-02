// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(macOS)
	import ServiceManagement
	import SwiftUI
	import SynctrainHostKit

	/// Opens the app again after it crashes, while it launches at login, and says that it did.
	///
	/// A small process that the app starts keeps an eye on it, rather than a launch agent: launchd only looks after what it
	/// started itself, which leaves out any start other than the one at login, and it would start a second app the moment
	/// the agent is registered.
	@MainActor @Observable final class CrashWatch {
		static let shared = CrashWatch()

		private(set) var notice: CrashNotice?
		/// Whether too many crashes have stopped the app from opening again, for now
		private(set) var hasStoppedOpening = false

		@ObservationIgnored private var tracker: CrashTracker?
		/// What the next log can say, as there is none before the engine's client exists
		@ObservationIgnored private var pendingLogMessage: String?
		@ObservationIgnored private var watcher: Watcher?
		@ObservationIgnored private var refresh: Task<Void, Never>?

		/// A process that opens the app some time after the pipe it reads ends, unless it reads `stop` from it. The app is
		/// the only one with the other end, so that the system closes it when the app goes, however that happens.
		private struct Watcher {
			static let script = """
				read -r word
				[ "$word" = stop ] && exit 0
				/bin/sleep "$1"
				path=$2; identifier=$3; shift 3
				/usr/bin/open -g "$@" "$path" || /usr/bin/open -g "$@" -b "$identifier"
				"""

			let delay: TimeInterval
			let input: FileHandle

			init(delay: TimeInterval) throws {
				let pipe = Pipe()
				let input = pipe.fileHandleForWriting
				// Processes the engine starts stay out of this, and the app survives a watcher that went away
				_ = fcntl(input.fileDescriptor, F_SETFD, FD_CLOEXEC)
				_ = fcntl(input.fileDescriptor, F_SETNOSIGPIPE, 1)

				let process = Process()
				process.executableURL = URL(fileURLWithPath: "/bin/sh")
				process.arguments =
					[
						"-c", Self.script, "sh", String(Int(delay)), Bundle.main.bundleURL.path(percentEncoded: false),
						Bundle.main.bundleIdentifier ?? "",
					] + TestRun.openArguments
				process.standardInput = pipe
				process.standardOutput = FileHandle.nullDevice
				process.standardError = FileHandle.nullDevice
				try process.run()
				try pipe.fileHandleForReading.close()
				self.delay = delay
				self.input = input
			}

			func stop() {
				try? input.write(contentsOf: Data("stop\n".utf8))
				try? input.close()
			}
		}

		/// To be called first thing, so that a crash while the app starts counts too
		func start() {
			// Stopping a run in Xcode would count as a crash, in preferences a debug build shares with the real app
			#if DEBUG
				guard TestRun.isActive else { return }
			#endif
			let tracker = CrashTracker(defaults: .standard)
			self.tracker = tracker
			if let started = tracker.runStarted(at: .now, macStartedAt: Self.macStartedAt) {
				let count = tracker.recentCrashes(at: .now).count
				pendingLogMessage =
					"The last run, which started at \(started.formatted(.iso8601)), ended unexpectedly (\(count) in the last hour)"
			}
			notice = tracker.notice
			update()
		}

		/// To be called once the log works
		func logLastRun() {
			if let pendingLogMessage {
				Log.warn(pendingLogMessage)
				self.pendingLogMessage = nil
			}
		}

		/// Follows the setting and the crashes so far. To be called when Launch at login changes.
		func update() {
			guard let tracker else { return }
			let opens = Self.opensAfterCrash
			let delay = opens ? tracker.relaunchDelay(at: .now) : nil
			hasStoppedOpening = opens && delay == nil
			if delay != watcher?.delay {
				watcher?.stop()
				watcher = nil
				if let delay {
					do {
						watcher = try Watcher(delay: delay)
						Log.info("Opening the app \(Int(delay)) seconds after a crash")
					}
					catch {
						Log.warn("The app will not open again after a crash: \(error.localizedDescription)")
					}
				}
			}

			// Crashes stop counting while the app runs, also for a run that was to be left closed
			refresh?.cancel()
			refresh = nil
			if let oldest = tracker.recentCrashes(at: .now).first {
				refresh = Task { [weak self] in
					try? await Task.sleep(for: .seconds(oldest.timeIntervalSinceNow + CrashTracker.window))
					if !Task.isCancelled {
						self?.update()
					}
				}
			}
		}

		/// To be called as soon as the app is going to end by itself. What goes wrong after this leaves it closed, and with
		/// no tracker left, nothing that still runs while quitting can start watching again.
		func stop() {
			refresh?.cancel()
			refresh = nil
			watcher?.stop()
			watcher = nil
			tracker?.runEnded()
			tracker = nil
		}

		func dismissNotice() {
			tracker?.dismissNotice()
			notice = nil
		}

		/// A debug run says so itself, as stopping one in Xcode ends it the way a crash does
		private static var opensAfterCrash: Bool {
			#if DEBUG
				ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_CRASH_RELAUNCH"] != nil
			#else
				AppRelauncher.canRelaunch && SMAppService.mainApp.status == .enabled
			#endif
		}

		private static var macStartedAt: Date {
			var started = timeval()
			var size = MemoryLayout<timeval>.size
			guard sysctlbyname("kern.boottime", &started, &size, nil, 0) == 0 else {
				return .distantPast
			}
			return Date(timeIntervalSince1970: TimeInterval(started.tv_sec))
		}
	}

	extension CrashNotice {
		var localizedDescription: String {
			let time = startedAgain.formatted(date: .abbreviated, time: .shortened)
			return count == 1
				? String(localized: "Synctrain stopped unexpectedly and started again on \(time).")
				: String(localized: "Synctrain stopped unexpectedly \(count) times and last started again on \(time).")
		}
	}

	/// Says that the app crashed, in a window. It stays until dismissed, as nobody may be looking when the app comes back.
	struct CrashNoticeSection: View {
		private let crashWatch = CrashWatch.shared

		var body: some View {
			if let notice = crashWatch.notice {
				Section {
					Label(notice.localizedDescription, systemImage: "exclamationmark.triangle")
					if crashWatch.hasStoppedOpening {
						Text("This happened too often, so Synctrain won't open again by itself for up to an hour.")
							.foregroundStyle(.secondary)
					}
					Button("Dismiss") {
						crashWatch.dismissNotice()
					}
				}
			}
		}
	}

	/// The same in the menu bar menu, which is all there is of the app without a Dock icon
	struct CrashNoticeMenuItems: View {
		private let crashWatch = CrashWatch.shared

		var body: some View {
			if let notice = crashWatch.notice {
				Text(verbatim: notice.localizedDescription)
				Button("Dismiss", systemImage: "xmark") {
					crashWatch.dismissNotice()
				}
			}
		}
	}
#endif

/// Ends the app at once, for what then has the user open it again. On a Mac that is no crash.
@MainActor func exitApp() -> Never {
	#if os(macOS)
		CrashWatch.shared.stop()
	#endif
	exit(0)
}
