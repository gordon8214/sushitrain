// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

/// What the engine is doing as a whole, which the Mac's menu bar icon and the first line of its menu show
public enum EngineStatus: Equatable, Sendable {
	/// Not running yet, or restarting
	case starting
	case failed(message: String)
	/// The names of the folders that stopped on an error, such as their path being gone
	case folderErrors(folders: [String])
	case devicesPaused
	case foldersPaused
	case offline
	case syncing
	case upToDate

	/// The first that applies wins: what needs the user comes before what explains why nothing is moving, and a pause
	/// comes before being offline, as it is the reason for that.
	public init(
		isRunning: Bool, failure: String?, foldersWithErrors: [String], allDevicesPaused: Bool, allFoldersPaused: Bool,
		connectedPeerCount: Int, isTransferring: Bool
	) {
		if let failure {
			self = .failed(message: Self.singleLine(failure))
		}
		else if !isRunning {
			self = .starting
		}
		else if !foldersWithErrors.isEmpty {
			self = .folderErrors(folders: foldersWithErrors)
		}
		else if allDevicesPaused {
			self = .devicesPaused
		}
		else if allFoldersPaused {
			self = .foldersPaused
		}
		else if connectedPeerCount == 0 {
			self = .offline
		}
		else if isTransferring {
			self = .syncing
		}
		else {
			self = .upToDate
		}
	}

	/// An SF Symbol that stays legible as a template image at menu bar size. Each state has its own, chosen by shape: a
	/// badge or a detail doesn't survive that size, and the menu bar shows them in one color.
	public var symbol: String {
		switch self {
		case .starting: return "hourglass"
		case .failed, .folderErrors: return "exclamationmark.triangle.fill"
		case .devicesPaused, .foldersPaused: return "pause.circle.fill"
		case .offline: return "folder"
		case .syncing: return "arrow.triangle.2.circlepath"
		case .upToDate: return "folder.fill"
		}
	}

	/// The first line of a message, short enough for a menu, which grows to fit its longest item
	private static func singleLine(_ message: String) -> String {
		let maxLength = 80
		let line = message.split(whereSeparator: \.isNewline).first.map(String.init) ?? ""
		return line.count > maxLength ? line.prefix(maxLength - 1) + "…" : line
	}
}
