// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if os(iOS)
	import Foundation
	import os
	import SynctrainFileProviderKit

	/// The UI action extension owns a distinct session; its events can be completed by the containing app after termination.
	@MainActor enum HistoryUploadSessions {
		private static var sessions: [String: BackgroundTransfers] = [:]
		private nonisolated static let finished = OSAllocatedUnfairLock<Set<String>>(initialState: [])
		static func takeFinished() -> Set<String> {
			finished.withLock {
				defer { $0 = [] }
				return $0
			}
		}

		private static var reconciliations: [String: Task<Void, Never>] = [:]
		private static var handlingEvents: Set<String> = []
		private static var workers: [String: Task<Void, Never>] = [:]

		static func session(
			identifier: String, store: IndexStore, storage: RemoteStorage, credentials: any RemoteCredentialStore
		) -> BackgroundTransfers {
			if let session = sessions[identifier] { return session }
			let group = Bundle.main.object(forInfoDictionaryKey: SharedContainer.appGroupInfoKey) as? String
			let transfers = BackgroundTransfers(
				identifier: identifier, appGroupIdentifier: group, store: store, storage: storage, credentials: credentials
			) { record in
				guard record.kind == .upload, let operationID = record.operationID else { return }
				_ = finished.withLock { $0.insert(operationID) }
				SharedNotification.workQueued.post()
				Task { @MainActor in
					startCompletion(
						operationID: operationID, identifier: identifier, store: store, storage: storage, credentials: credentials)
				}
			}
			sessions[identifier] = transfers
			return transfers
		}

		/// Call only for a session this process owns: its app session, its extension session, or OS-delivered events.
		static func reconcile(_ transfers: BackgroundTransfers, force: Bool = false) async {
			let identifier = transfers.identifier
			if force { reconciliations[identifier] = nil }
			if reconciliations[identifier] == nil {
				reconciliations[identifier] = Task {
					do { _ = try await transfers.reconcile() }
					catch { NSLog("Could not reconcile history uploads: %@", error.localizedDescription) }
				}
			}
			await reconciliations[identifier]?.value
		}

		static func handoff(store: IndexStore, storage: RemoteStorage, credentials: any RemoteCredentialStore) async
			-> BackgroundHandoff?
		{
			guard let appID = RemoteSessionIdentifier.appBundleIdentifier() else { return nil }
			let isExtension = Bundle.main.bundleURL.pathExtension == "appex"
			let identifier =
				isExtension
				? RemoteSessionIdentifier.historyActionExtension(appBundleIdentifier: appID)
				: RemoteSessionIdentifier.historyApp(appBundleIdentifier: appID)
			let transfers = session(identifier: identifier, store: store, storage: storage, credentials: credentials)
			await reconcile(transfers)
			return BackgroundHandoff(transfers: transfers, foregroundBudget: 5)
		}

		private static func startCompletion(
			operationID: String, identifier: String, store: IndexStore, storage: RemoteStorage,
			credentials: any RemoteCredentialStore
		) {
			guard !handlingEvents.contains(identifier), workers[operationID] == nil, let transfers = sessions[identifier] else {
				return
			}
			workers[operationID] = Task {
				defer { workers[operationID] = nil }
				var outbox = Outbox(
					store: store, storage: storage, owner: "history-completion-\(UUID().uuidString)", credentials: credentials
				) { serviceID in
					guard let endpoint = try? store.remoteEndpoint(serviceID: serviceID) else { return nil }
					return RemoteFileClient(endpoint: endpoint, credentials: credentials)
				}
				let handoff = BackgroundHandoff(transfers: transfers, minimumRemainder: 1)
				handoff.request()
				outbox.handoff = handoff
				let work = Task {
					do {
						try store.expediteMutation(operationID: operationID)
						while !Task.isCancelled {
							_ = try await outbox.process(operationID: operationID)
							if try store.mutation(operationID: operationID)?.state.isTerminal == true { break }
							try await Task.sleep(for: .seconds(1))
						}
					}
					catch {
						// The durable outbox resumes after cancellation or a gateway error.
					}
				}
				let timeout = Task {
					do {
						try await Task.sleep(for: .seconds(15))
						work.cancel()
					}
					catch {}
				}
				await withTaskCancellationHandler {
					await work.value
				} onCancel: {
					work.cancel()
				}
				timeout.cancel()
				await GatewayHistoryView.signalFiles()
			}
		}

		static func beginEvents(identifier: String) { handlingEvents.insert(identifier) }
		static func endEvents(identifier: String) { handlingEvents.remove(identifier) }

		static func drainCompletions() async {
			let pending = Array(workers.values)
			for worker in pending { worker.cancel() }
			for worker in pending { await worker.value }
		}

		static func release(identifier: String) {
			sessions[identifier] = nil
			reconciliations[identifier] = nil
		}
	}
#endif
