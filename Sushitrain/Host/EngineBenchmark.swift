// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if SYNCTRAIN_BENCHMARK && os(macOS)
	import AppKit
	import SynctrainHostKit

	/// Instrumentation compiled only by the tools repository's optimized benchmark build.
	@MainActor enum EngineBenchmark {
		private static var timer: Timer?
		private static var previous = ProcessInfo.processInfo.systemUptime
		private static var delays: [Double] = []
		private static var ticks = 0
		private static let probesResponsiveness = ProcessInfo.processInfo.environment["SYNCTRAIN_BENCHMARK_PROBE"] == "1"
		private static var interval: Double { probesResponsiveness ? 0.02 : 1 }
		static var folderIsVisible = false

		static func prepare() {
			#if DEBUG
				fatalError("Performance measurements require an optimized Release build")
			#endif
			guard let root = EngineLocations.testRoot else {
				fatalError("A benchmark build requires an isolated root")
			}
			let defaults = UserDefaults.standard
			defaults.set("synchronize", forKey: "appMode")
			defaults.set("fresh", forKey: "engineSetupChoice")
			defaults.set(1, forKey: "onboardingVersionShown")
			defaults.set(Bundle.main.buildVersionNumber, forKey: "lastRunBuild")
			defaults.set(root.appending(path: "engine").path, forKey: "configDirectoryPath")
			defaults.set(
				ProcessInfo.processInfo.environment["SYNCTRAIN_BENCHMARK_PRIORITY"] ?? "normal", forKey: "enginePriority")
			let proof: [String: Any] = [
				"pid": ProcessInfo.processInfo.processIdentifier, "root": root.path,
				"config": root.appending(path: "engine").path, "optimized": true,
				"fileProvider": TestRun.allowsFileProvider,
			]
			do {
				try JSONSerialization.data(withJSONObject: proof).write(to: root.appending(path: "benchmark-ready.json"))
			}
			catch {
				fatalError("Could not record benchmark isolation: \(error)")
			}
			// A main-run-loop timer measures scheduling delays, including synchronous work in event callbacks.
			// Aggregate once a second to keep instrumentation out of most ticks.
			let timer = Timer(timeInterval: interval, repeats: true) { _ in
				MainActor.assumeIsolated { tick(root: root) }
			}
			Self.timer = timer
			RunLoop.main.add(timer, forMode: .common)
		}

		private static func tick(root: URL) {
			let now = ProcessInfo.processInfo.systemUptime
			delays.append(max(0, now - previous - interval))
			previous = now
			ticks += 1
			guard ticks % (probesResponsiveness ? 50 : 1) == 0 else { return }
			let windows = NSApp.windows.filter { $0.isVisible && $0.canBecomeMain }
			if ProcessInfo.processInfo.environment["SYNCTRAIN_BENCHMARK_WINDOW"] == "closed" {
				for window in windows { window.close() }
			}
			if ProcessInfo.processInfo.environment["SYNCTRAIN_BENCHMARK_WINDOW"] == "folder",
				!folderIsVisible,
				FileManager.default.fileExists(atPath: root.appending(path: "benchmark-show-folder").path)
			{
				QuickActionService.shared.action = .folder(folderID: "benchmark", prefix: nil)
			}
			let sample: [String: Any] = [
				"uptime": now, "probesResponsiveness": probesResponsiveness, "maxDelayS": delays.max() ?? 0, "ticks": delays.count,
				"over50ms": delays.filter { $0 > 0.05 }.count, "visibleWindows": windows.count, "folderIsVisible": folderIsVisible,
			]
			if let data = try? JSONSerialization.data(withJSONObject: sample) {
				try? data.write(to: root.appending(path: "benchmark-ui.json"), options: .atomic)
			}
			delays.removeAll(keepingCapacity: true)
		}
	}
#endif
