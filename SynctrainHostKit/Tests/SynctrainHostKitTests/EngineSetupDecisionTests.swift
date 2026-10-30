// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

private func facts(_ change: (inout EngineSetupDecision.Facts) -> Void) -> EngineSetupDecision.Facts {
	var facts = EngineSetupDecision.Facts()
	change(&facts)
	return facts
}

@Test func aMacWithoutSyncthingJustStarts() {
	#expect(EngineSetupDecision.decide(facts { _ in }) == .start)
	#expect(EngineSetupDecision.decide(facts { $0.synctrainHasConfiguration = true }) == .start)
}

@Test func theFirstStartNextToSyncthingOffersTheTakeover() {
	let first = facts { $0.syncthingHasConfiguration = true }
	#expect(EngineSetupDecision.decide(first) == .offerTakeover)
	var running = first
	running.syncthingRunning = true
	#expect(EngineSetupDecision.decide(running) == .offerTakeover)
	// Once chosen, or once the app has a configuration of its own, not again
	var chosen = first
	chosen.choiceRecorded = true
	#expect(EngineSetupDecision.decide(chosen) == .start)
	var own = first
	own.synctrainHasConfiguration = true
	#expect(EngineSetupDecision.decide(own) == .start)
}

@Test func syncthingsOwnConfigurationWaitsForItToQuit() {
	let direct = facts {
		$0.usesSyncthingHome = true
		$0.synctrainHasConfiguration = true
		$0.syncthingHasConfiguration = true
		$0.syncthingRunning = true
	}
	#expect(EngineSetupDecision.decide(direct) == .waitForSyncthing)
	var quit = direct
	quit.syncthingRunning = false
	#expect(EngineSetupDecision.decide(quit) == .start)
}

@Test func theSameIdentityNeverRunsTwice() {
	// An imported copy, while Syncthing still runs; running alongside does not change that
	let copy = facts {
		$0.synctrainHasConfiguration = true
		$0.syncthingHasConfiguration = true
		$0.sameIdentity = true
		$0.syncthingRunning = true
		$0.runsAlongside = true
	}
	#expect(EngineSetupDecision.decide(copy) == .waitForSyncthing)
}

@Test func anotherSyncthingRunsAlongsideOnlyWhenChosen() {
	let other = facts {
		$0.synctrainHasConfiguration = true
		$0.syncthingHasConfiguration = true
		$0.syncthingRunning = true
	}
	#expect(EngineSetupDecision.decide(other) == .askToRunAlongside)
	var alongside = other
	alongside.runsAlongside = true
	#expect(EngineSetupDecision.decide(alongside) == .start)
}

@Test func noCombinationRunsOneIdentityTwiceOrOffersATakeoverOverAnExistingSetup() {
	for bits in 0..<(1 << 7) {
		var facts = EngineSetupDecision.Facts()
		facts.synctrainHasConfiguration = bits & 1 != 0
		facts.usesSyncthingHome = bits & 2 != 0
		facts.choiceRecorded = bits & 4 != 0
		facts.syncthingHasConfiguration = bits & 8 != 0
		facts.syncthingRunning = bits & 16 != 0
		facts.sameIdentity = bits & 32 != 0
		facts.runsAlongside = bits & 64 != 0
		let decision = EngineSetupDecision.decide(facts)
		if (facts.usesSyncthingHome || facts.sameIdentity) && facts.syncthingRunning {
			#expect(decision == .waitForSyncthing, "\(facts)")
		}
		if decision == .offerTakeover {
			#expect(!facts.synctrainHasConfiguration && !facts.choiceRecorded, "\(facts)")
		}
	}
}

@Test func theSameIdentityWithoutSyncthingsConfigurationStillWaits() {
	let keysOnly = facts {
		$0.synctrainHasConfiguration = true
		$0.sameIdentity = true
		$0.syncthingRunning = true
		$0.runsAlongside = true
	}
	#expect(EngineSetupDecision.decide(keysOnly) == .waitForSyncthing)
}
