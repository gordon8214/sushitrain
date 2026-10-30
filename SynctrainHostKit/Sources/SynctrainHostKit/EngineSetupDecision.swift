// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation

/// What the Mac app does before its engine starts, when Syncthing (Syncthing.app's backend) may also be on this Mac.
/// Two engines must never run on one configuration, nor one device identity run twice.
public enum EngineSetupDecision: Equatable, Sendable {
	/// Start the engine
	case start
	/// Offer to use Syncthing's configuration, import a copy of it, or start anew
	case offerTakeover
	/// Wait until the Syncthing that runs on the same configuration, or with the same device identity, has quit
	case waitForSyncthing
	/// Another Syncthing runs, with another identity: start only once it quit, or the user chose to run alongside it
	case askToRunAlongside

	public struct Facts: Sendable, Equatable {
		/// The configuration folder the app will use has an engine configuration or identity
		public var synctrainHasConfiguration = false
		/// That folder is Syncthing's own home directory (used directly)
		public var usesSyncthingHome = false
		/// The user chose how to set up the engine before (take over, import, start anew)
		public var choiceRecorded = false
		/// Syncthing's home directory has a configuration
		public var syncthingHasConfiguration = false
		/// A Syncthing runs on its home directory (holds its lock), or Syncthing.app runs
		public var syncthingRunning = false
		/// Syncthing's device identity is the one the app will use (as with an imported copy)
		public var sameIdentity = false
		/// The user chose to run next to another Syncthing
		public var runsAlongside = false

		public init() {}
	}

	public static func decide(_ facts: Facts) -> EngineSetupDecision {
		if facts.usesSyncthingHome || facts.sameIdentity {
			return facts.syncthingRunning ? .waitForSyncthing : .start
		}
		if !facts.synctrainHasConfiguration && !facts.choiceRecorded && facts.syncthingHasConfiguration {
			return .offerTakeover
		}
		if facts.syncthingRunning && !facts.runsAlongside {
			return .askToRunAlongside
		}
		return .start
	}
}
