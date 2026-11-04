// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
@preconcurrency import FileProvider
import Foundation
import SwiftUI
import SynctrainFileProviderKit

struct GatewayHistoryView: View {
	let itemID: Int64
	var done: () -> Void
	@State private var model: HistoryBrowserModel?
	@State private var problem: String?

	var body: some View {
		Group {
			if let model {
				HistoryBrowserView(model: model, done: done)
			}
			else if let problem {
				ContentUnavailableView {
					Label("Versions Are Not Available", systemImage: "exclamationmark.triangle")
				} description: {
					Text(problem)
				} actions: {
					Button("Done", action: done)
				}
			}
			else {
				ProgressView()
			}
		}.task {
			guard model == nil else { return }
			HistoryBrowserModel.removeLeftovers()
			do { model = try Self.makeModel(itemID: itemID) }
			catch { problem = HistoryError.message(for: error) }
		}
	}

	@MainActor static func makeModel(itemID: Int64) throws -> HistoryBrowserModel {
		guard let container = SharedContainer.appGroup() else { throw HistoryError.sourceChanged }
		let store = IndexStore(container: container)
		let group = Bundle.main.object(forInfoDictionaryKey: SharedContainer.appGroupInfoKey) as? String
		let credentials = KeychainCredentialStore(accessGroup: group?.isEmpty == false ? group : nil)
		let browser = try RemoteVersionBrowser(store: store, itemID: itemID) {
			RemoteFileClient(endpoint: $0, credentials: credentials)
		}
		let file = try browser.file()
		let primary = HistorySource(
			endpoint: browser.client.endpoint, folderID: browser.binding.remoteFolderID, path: file.remote.path,
			itemID: file.remote.gatewayID, requiresEnabled: false)
		let additional = try store.historyEndpoints().filter { $0.serviceID != primary.id }.map {
			HistorySource(endpoint: $0, folderID: primary.folderID, path: primary.path)
		}
		let histories = ([primary] + additional).map {
			RemoteHistory(store: store, source: $0, client: RemoteFileClient(endpoint: $0.endpoint, credentials: credentials))
		}
		let explanation =
			browser.canWrite
			? String(
				localized:
					"Restore replaces the file on \(primary.endpoint.displayName) and synchronizes normally. Its current contents are archived only if destination versioning is enabled. Pending edits must finish first."
			)
			: String(localized: "This folder is read only. Versions can be opened or exported.")
		let destination = HistoryDestination(
			name: primary.endpoint.displayName, canRestore: browser.canWrite,
			explanation: explanation, primarySourceID: primary.id,
			isAvailable: { (try? browser.file().item.pending == .none) == true },
			restoreInPlace: { row in
				guard row.sourceID == primary.id, row.version.entityTag == nil else { return false }
				_ = try await browser.restore(row.version)
				await Self.signalFiles()
				return true
			},
			prepare: {
				let captured = try browser.file()
				guard captured.item.pending == .none, browser.canWrite else { throw HistoryError.destinationChanged }
				return { downloaded, version in
					let storage = RemoteStorage(container: container)
					try storage.createDirectories()
					let result = try await HistoryRestore.recordDownloadedVersion(
						downloaded, version: version, itemID: itemID, binding: browser.binding,
						expectedRevision: captured.remote.revision, store: store, storage: storage)
					SharedNotification.workQueued.post()
					SharedNotification.indexChanged.post()
					await Self.signalFiles()
					guard let operationID = result.mutation?.operationID else { throw HistoryError.destinationChanged }
					var outbox = Outbox(
						store: store, storage: storage, owner: "history-\(UUID().uuidString)", credentials: credentials
					) { serviceID in
						guard let endpoint = try? store.remoteEndpoint(serviceID: serviceID) else { return nil }
						return RemoteFileClient(endpoint: endpoint, credentials: credentials)
					}
					#if os(iOS)
						outbox.handoff = await HistoryUploadSessions.handoff(store: store, storage: storage, credentials: credentials)
					#endif
					let deadline = ContinuousClock.now + .seconds(90)
					while ContinuousClock.now < deadline {
						try Task.checkCancellation()
						_ = try await outbox.process(operationID: operationID)
						if let mutation = try store.mutation(operationID: operationID) {
							if try HistoryRestore.isCommitted(mutation.state) {
								try? await browser.refresh()
								SharedNotification.indexChanged.post()
								await Self.signalFiles()
								return
							}
						}
						try await Task.sleep(for: .milliseconds(500))
					}
					throw HistoryError.restorePending(nil)
				}
			})
		return HistoryBrowserModel(fileName: file.item.name, store: store, histories: histories, destination: destination)
	}
	static func signalFiles() async {
		guard
			let domain = (try? await NSFileProviderManager.domains())?.first(where: {
				$0.identifier.rawValue == SharedContainer.domainIdentifier
			}),
			let manager = NSFileProviderManager(for: domain)
		else { return }
		do { try await manager.signalEnumerator(for: .workingSet) }
		catch { NSLog("Could not refresh Files after restoring history: %@", error.localizedDescription) }
	}

}
