// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
#if DEBUG
	import Foundation
	import SwiftUI
	import SynctrainFileProviderKit

	/// An explicitly launched, isolated native presentation fixture: no app group, Keychain or File Provider domain.
	struct HistoryFixtureApp: App {
		var body: some Scene { WindowGroup { HistoryFixtureView() } }
	}

	private struct HistoryFixtureView: View {
		@State private var model: HistoryBrowserModel?
		@State private var environment: RemoteEnvironment?
		@State private var problem: String?
		var body: some View {
			TabView {
				Tab("Versions", systemImage: "clock.arrow.circlepath") {
					if let model {
						HistoryBrowserView(model: model, done: {})
					}
					else {
						Text(problem ?? "Loading…")
					}
				}
				Tab("History Sources", systemImage: "externaldrive") {
					if let environment { NavigationStack { HistorySourcesView(environment: environment, allowPairing: false) } }
				}
			}.task {
				do {
					let root =
						ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_HISTORY_STORAGE"].map { URL(fileURLWithPath: $0) }
						?? URL.temporaryDirectory.appending(path: "Synctrain-history-fixture-\(UUID().uuidString)")
					let container = SharedContainer(baseURL: root)
					let credentials = InMemoryCredentialStore(credentials: [
						"fixture-online": "fixture", "fixture-offline": "fixture", "fixture-alias": "fixture",
					])
					let environment = RemoteEnvironment(container: container, credentials: credentials) { _ in
						let configuration = URLSessionConfiguration.ephemeral
						configuration.protocolClasses = [HistoryFixtureProtocol.self]
						return configuration
					}
					// Leave storage untouched until the History Sources page first reads it.
					if ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_HISTORY_EMPTY"] == "1" {
						self.environment = environment
						return
					}
					var endpoints = [
						RemoteEndpoint(serviceID: "fixture-online", host: "history.fixture.test", port: 443, displayName: "Archive NAS"),
						RemoteEndpoint(
							serviceID: "fixture-offline", host: "offline.fixture.test", port: 443, displayName: "Offline Archive"),
					]
					endpoints.append(
						RemoteEndpoint(serviceID: "fixture-alias", host: "alias.fixture.test", port: 443, displayName: "Mirror Alias"))
					for endpoint in endpoints {
						try environment.store.saveRemoteEndpoint(endpoint, usage: .history)
						try environment.store.saveHistoryFolders(serviceID: endpoint.serviceID, folderIDs: ["docs"])
					}
					let histories = endpoints.map {
						RemoteHistory(
							store: environment.store, source: HistorySource(endpoint: $0, folderID: "docs", path: "Notes.txt"),
							client: environment.client(for: $0))
					}
					let destination = HistoryDestination(
						name: "This Device", canRestore: ProcessInfo.processInfo.environment["SYNCTRAIN_TEST_HISTORY_READ_ONLY"] != "1",
						explanation:
							"Restore replaces the current file. Destination versioning is off, so current contents will not be archived."
					) {
						return { file, _ in
							let target = root.appending(path: "Restored Notes.txt")
							try? FileManager.default.removeItem(at: target)
							try FileManager.default.copyItem(at: file, to: target)
						}
					}
					self.environment = environment
					model = HistoryBrowserModel(
						fileName: "Notes.txt", store: environment.store, histories: histories, destination: destination)
				}
				catch { problem = error.localizedDescription }
			}
		}
	}

	private final class HistoryFixtureProtocol: URLProtocol, @unchecked Sendable {
		override class func canInit(with request: URLRequest) -> Bool { request.url?.host?.hasSuffix("fixture.test") == true }
		override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
		override func startLoading() {
			guard let url = request.url else { return }
			if url.host == "offline.fixture.test" {
				client?.urlProtocol(self, didFailWithError: URLError(.cannotConnectToHost))
				return
			}
			let body: Data
			switch url.path {
			case "/v1/capabilities":
				body = Data(
					"""
					{"apiVersion":1,"serviceID":"\(url.host == "alias.fixture.test" ? "fixture-alias" : "fixture-online")","serviceName":"Archive NAS","syncthingDeviceID":"fixture-device","authorityGeneration":1,"features":["versions","versions.byPath"],"limits":{"pageSize":256,"maxPageSize":1024,"longPollSeconds":30,"leaseSeconds":60,"uploadExpirySeconds":600,"maxUploadSize":1000000,"maxControlBodyBytes":1048576}}
					""".utf8)
			case "/v1/folders":
				body = Data(
					"""
					{"folders":[{"id":"docs","label":"Documents","rootItemID":"fixture-root","permissions":{"read":true,"write":false},"type":"sendreceive"}]}
					""".utf8)
			case "/v1/folders/docs/versions":
				body = Data(
					"""
					{"folderID":"docs","path":"Notes.txt","versioning":"simple","versions":[{"id":"ver_1792545380","versionTime":"2026-10-21T01:16:20Z","modifiedNs":1792545000000000000,"size":24,"etag":"\\\"fixture-archive\\\""},{"id":"ver_1792458980","versionTime":"2026-10-20T01:16:20Z","modifiedNs":1792458600000000000,"size":24,"etag":"\\\"fixture-archive\\\""}]}
					""".utf8)
			default: body = Data("An archived note.\nHello!".utf8)
			}
			guard
				let response = HTTPURLResponse(
					url: url, statusCode: 200, httpVersion: "HTTP/1.1",
					headerFields: ["Content-Length": String(body.count), "ETag": "\"fixture-archive\""])
			else { return }
			client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
			client?.urlProtocol(self, didLoad: body)
			client?.urlProtocolDidFinishLoading(self)
		}
		override func stopLoading() {}
	}
#endif
