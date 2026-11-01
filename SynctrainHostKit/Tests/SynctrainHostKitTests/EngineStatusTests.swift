// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Testing

@testable import SynctrainHostKit

private func status(
	isRunning: Bool = true, failure: String? = nil, foldersWithErrors: [String] = [],
	foldersWithFailedItems: [String] = [], allDevicesPaused: Bool = false, allFoldersPaused: Bool = false,
	connectedPeerCount: Int = 1, isTransferring: Bool = false
) -> EngineStatus {
	EngineStatus(
		isRunning: isRunning, failure: failure, foldersWithErrors: foldersWithErrors,
		foldersWithFailedItems: foldersWithFailedItems, allDevicesPaused: allDevicesPaused,
		allFoldersPaused: allFoldersPaused, connectedPeerCount: connectedPeerCount, isTransferring: isTransferring)
}

@Test func anIdleEngineWithAPeerIsUpToDate() {
	#expect(status() == .upToDate)
}

@Test func aTransferIsSyncing() {
	#expect(status(isTransferring: true) == .syncing)
}

@Test func noPeersIsOfflineEvenIfAFolderStillWaitsToTransfer() {
	#expect(status(connectedPeerCount: 0) == .offline)
	#expect(status(connectedPeerCount: 0, isTransferring: true) == .offline)
}

@Test func pausingEverythingIsPausedAndNotOffline() {
	#expect(status(allDevicesPaused: true, connectedPeerCount: 0) == .devicesPaused)
	#expect(status(allFoldersPaused: true) == .foldersPaused)
	#expect(status(allDevicesPaused: true, allFoldersPaused: true, connectedPeerCount: 0) == .devicesPaused)
}

@Test func aFolderErrorOutranksEverythingTheRunningEngineDoes() {
	let errors = status(
		foldersWithErrors: ["Documents"], allDevicesPaused: true, allFoldersPaused: true, connectedPeerCount: 0,
		isTransferring: true)
	#expect(errors == .folderErrors(folders: ["Documents"]))
}

@Test func failedItemsAreAnErrorBelowAStoppedFolder() {
	#expect(status(foldersWithFailedItems: ["Photos"]) == .failedItems(folders: ["Photos"]))
	#expect(status(foldersWithFailedItems: ["Photos"]).symbol == "exclamationmark.triangle.fill")
	#expect(
		status(foldersWithErrors: ["Documents"], foldersWithFailedItems: ["Photos"]) == .folderErrors(folders: ["Documents"]))
}

@Test func failedItemsOutrankWhatExplainsWhyNothingMoves() {
	let failed = status(
		foldersWithFailedItems: ["Photos"], allDevicesPaused: true, connectedPeerCount: 0, isTransferring: true)
	#expect(failed == .failedItems(folders: ["Photos"]))
}

@Test func anEngineThatIsNotRunningIsStartingAndNotAnError() {
	#expect(status(isRunning: false) == .starting)
	#expect(status(isRunning: false, foldersWithErrors: ["Documents"], allDevicesPaused: true) == .starting)
}

@Test func anEngineFailureOutranksEverything() {
	#expect(status(isRunning: false, failure: "no database") == .failed(message: "no database"))
	#expect(status(failure: "no database", foldersWithErrors: ["Documents"]) == .failed(message: "no database"))
}

@Test func aFailureBecomesOneShortLine() {
	#expect(status(failure: "first\nsecond") == .failed(message: "first"))
	#expect(status(failure: "") == .failed(message: ""))
	guard case .failed(let message) = status(failure: String(repeating: "x", count: 200)) else {
		Issue.record("expected a failure")
		return
	}
	#expect(message.count == 80)
	#expect(message.hasSuffix("…"))
}

@Test func eachStateHasItsOwnSymbol() {
	let states: [EngineStatus] = [.starting, .failed(message: ""), .devicesPaused, .offline, .syncing, .upToDate]
	#expect(Set(states.map(\.symbol)).count == states.count)
	// The two kinds of error and of pause look the same, the first line of the menu tells them apart
	#expect(EngineStatus.folderErrors(folders: ["Documents"]).symbol == EngineStatus.failed(message: "").symbol)
	#expect(EngineStatus.foldersPaused.symbol == EngineStatus.devicesPaused.symbol)
}
