// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit

/// History selections are shared with the action extension, independently of folder subscriptions.
struct HistorySourcesView: View {
	var environment: RemoteEnvironment? = .shared
	var allowPairing = true
	@State private var endpoints: [RemoteEndpoint] = []
	@State private var enabled: Set<String> = []
	@State private var messages: [String: String] = [:]
	@State private var showPairing = false
	@State private var errorMessage: String?

	var body: some View {
		Form {
			Section {
				ForEach(endpoints, id: \.serviceID) { endpoint in
					VStack(alignment: .leading) {
						Toggle(
							endpoint.displayName,
							isOn: Binding(
								get: { enabled.contains(endpoint.serviceID) },
								set: { setEnabled($0, endpoint: endpoint) }))
						Text(endpoint.baseURL.absoluteString).font(.caption).foregroundStyle(.secondary)
						if let message = messages[endpoint.serviceID] { Text(message).font(.caption).foregroundStyle(.secondary) }
					}
				}
				Button("Pair with a History Source…", systemImage: "plus") { showPairing = true }.disabled(!allowPairing)
			} header: {
				Text("History Sources")
			} footer: {
				Text(
					"Include previous versions from these gateways in files with the same Syncthing folder ID and path. Read access is enough. Pairing for history does not add folders to Finder or Files."
				)
			}
		}
		.formStyle(.grouped)
		.navigationTitle("History Sources")
		.task { await reload() }
		.refreshable { await reload() }
		.sheet(isPresented: $showPairing, onDismiss: { Task { await reload() } }) { RemotePairingView(purpose: .history) }
		.alert(
			"Could Not Save History Sources",
			isPresented: Binding(get: { errorMessage != nil }, set: { if !$0 { errorMessage = nil } })
		) {
			Button("OK") {}
		} message: {
			Text(errorMessage ?? "")
		}
	}

	private func setEnabled(_ value: Bool, endpoint: RemoteEndpoint) {
		guard let store = environment?.store else { return }
		do {
			var usage = try store.gatewayUsage(serviceID: endpoint.serviceID)
			if value {
				usage.insert(.history)
			}
			else {
				usage.remove(.history)
			}
			try store.setGatewayUsage(usage, serviceID: endpoint.serviceID)
			if value {
				enabled.insert(endpoint.serviceID)
			}
			else {
				enabled.remove(endpoint.serviceID)
			}
			Task { await check(endpoint) }
		}
		catch { errorMessage = HistoryError.message(for: error) }
	}

	private func reload() async {
		guard let store = environment?.store else { return }
		do {
			endpoints = try store.remoteEndpoints()
			enabled = Set(try store.historyEndpoints().map(\.serviceID))
			await withTaskGroup(of: Void.self) { group in
				for endpoint in endpoints where enabled.contains(endpoint.serviceID) {
					group.addTask { await check(endpoint) }
				}
			}
		}
		catch { errorMessage = HistoryError.message(for: error) }
	}

	private func check(_ endpoint: RemoteEndpoint) async {
		guard enabled.contains(endpoint.serviceID), let environment else { return }
		do {
			let client = environment.client(for: endpoint)
			let capabilities = try await client.capabilities()
			guard capabilities.serviceID == endpoint.serviceID else { throw HistoryError.sourceChanged }
			let folders = try await client.folders()
			guard try environment.store.remoteEndpoint(serviceID: endpoint.serviceID) == endpoint else { return }
			try environment.store.saveHistoryFolders(serviceID: endpoint.serviceID, folderIDs: folders.map(\.id))
			messages[endpoint.serviceID] =
				capabilities.supports(.versionsByPath)
				? (folders.count == 1
					? String(localized: "Ready · 1 accessible folder")
					: String(localized: "Ready · \(folders.count) accessible folders")) : HistoryError.needsUpdate.localizedDescription
		}
		catch { messages[endpoint.serviceID] = HistoryError.message(for: error) }
	}
}
