// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import BackgroundTasks

/// Occasional runs while the app is not open: gateways are refreshed and edits waiting for them sent, so that Files
/// stays current without the app being opened
enum BackgroundRefresh {
	static let identifier = "gordonbyrnes.Sushitrain.gateway-refresh"

	/// How long a run lasts: the system ends it earlier when its time is up
	private static let runDuration: Duration = .seconds(20)

	static func schedule() {
		let request = BGAppRefreshTaskRequest(identifier: identifier)
		request.earliestBeginDate = Date(timeIntervalSinceNow: 3600)
		do {
			try BGTaskScheduler.shared.submit(request)
		}
		catch {
			Log.warn("Could not schedule a background refresh: \(error.localizedDescription)")
		}
	}

	@MainActor static func run() async {
		schedule()
		RemoteCoordinator.shared.backgroundRefresh()
		try? await Task.sleep(for: runDuration)
		// The rest of a long upload goes to the background session, which sends it while the app is suspended
		await RemoteCoordinator.shared.prepareForSuspension()
	}
}
