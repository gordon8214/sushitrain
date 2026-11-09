// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// The recent transfers of files from and to gateways, with how far each has come. While shown, it reads the shared
/// index again every 2 seconds, so the File Provider's transfers move along too.
struct RemoteTransfersSection: View {
	/// The shared index (nil when the app group is unavailable)
	let store: IndexStore?
	@Environment(\.scenePhase) private var scenePhase
	@State private var transfers: [TransferRecord] = []
	@State private var names: [Int64: String] = [:]
	@State private var problem: String?

	var body: some View {
		Section("Transfers") {
			if let problem { Text(problem).foregroundStyle(.red) }
			if transfers.isEmpty { Text("No recent transfers") }
			ForEach(transfers, id: \.transferID) { transfer in
				VStack(alignment: .leading) {
					Text(transfer.itemID.flatMap { names[$0] } ?? transfer.transferID)
					Text("\(transfer.kind.rawValue.capitalized) · \(transfer.state.rawValue)").foregroundStyle(.secondary)
					if transfer.blockCount > 0 {
						ProgressView(value: Double(transfer.verifiedBlockCount), total: Double(transfer.blockCount))
					}
					if let error = transfer.lastError { Text(error).foregroundStyle(.red) }
				}
			}
		}
		// The section always shows a row, so this runs
		.task {
			while !Task.isCancelled {
				// Nobody sees it in the background, and each reading opens the shared index
				if scenePhase == .active { await reload() }
				do { try await Task.sleep(for: .seconds(2)) }
				catch { break }
			}
		}
	}

	private func reload() async {
		guard let store else {
			problem = String(localized: "The shared index is unavailable.")
			return
		}
		let result = await Task.detached {
			Result { () throws -> ([TransferRecord], [Int64: String]) in
				let records = try store.transfers(limit: 100)
				var names: [Int64: String] = [:]
				for record in records {
					if let id = record.itemID { names[id] = try store.item(id: id)?.path }
				}
				return (records, names)
			}
		}.value
		guard !Task.isCancelled else { return }
		switch result {
		case .success(let value):
			transfers = value.0
			names = value.1
			problem = nil
		case .failure(let error): problem = error.localizedDescription
		}
	}
}
