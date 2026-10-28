// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import CryptoKit
import Foundation

@testable import SynctrainFileProviderKit

/// Uploads (tus 1.0) and operations of a mocked gateway
final class TusGateway: Sendable {
	struct Upload: Sendable {
		var length: Int64
		var data: Data
		var digest: String
	}

	struct State: Sendable {
		var requests: [MockRequest] = []
		var uploads: [String: Upload] = [:]
		var nextUpload = 1
		/// Operations by client operation ID, with the payload they were submitted with
		var operations: [String: (payload: Data, operation: RemoteOperation)] = [:]
		/// Polls an operation stays pending for
		var pendingPolls = 0
		/// Accepted submissions whose answer is lost (the gateway answers 503 after storing them)
		var loseResponses = 0
		/// The item a committed operation results in
		var resultItem: RemoteItem?
	}

	let state = Locked(State())

	func handle(_ request: MockRequest) -> MockResponse {
		state.update { $0.requests.append(request) }
		let path = request.path
		switch (request.method, path) {
		case ("POST", "/v1/uploads"):
			return create(request)
		case ("HEAD", _) where path.hasPrefix("/v1/uploads/"):
			let id = String(path.dropFirst("/v1/uploads/".count))
			guard let upload = state.value.uploads[id] else {
				return MockResponse(status: 404)
			}
			return MockResponse(
				status: 200,
				headers: ["Upload-Offset": String(upload.data.count), "Upload-Length": String(upload.length)])
		case ("PATCH", _) where path.hasPrefix("/v1/uploads/"):
			return append(request, id: String(path.dropFirst("/v1/uploads/".count)))
		case ("POST", "/v1/operations"):
			return submit(request)
		case ("GET", _) where path.hasPrefix("/v1/operations/"):
			return poll(String(path.dropFirst("/v1/operations/".count)))
		default:
			return .error(404, code: "not_found")
		}
	}

	private func create(_ request: MockRequest) -> MockResponse {
		guard let length = request.header("Upload-Length").flatMap({ Int64($0) }),
			let metadata = request.header("Upload-Metadata")
		else {
			return .error(400, code: "invalid_request")
		}
		var values: [String: String] = [:]
		for pair in metadata.split(separator: ",") {
			let parts = pair.split(separator: " ")
			if parts.count == 2, let data = Data(base64Encoded: String(parts[1])) {
				values[String(parts[0])] = String(decoding: data, as: UTF8.self)
			}
		}
		guard values["folder"] != nil, let digest = values["digest"] else {
			return .error(400, code: "invalid_request")
		}
		let id = state.update { state -> String in
			let id = "upl_\(state.nextUpload)"
			state.nextUpload += 1
			state.uploads[id] = Upload(length: length, data: Data(), digest: digest)
			return id
		}
		return .json(
			UploadInfo(upload: id, token: "stu_\(id)", offset: 0, length: length, expires: Date().addingTimeInterval(604_800)),
			status: 201)
	}

	private func append(_ request: MockRequest, id: String) -> MockResponse {
		guard request.header("Content-Type") == "application/offset+octet-stream" else {
			return MockResponse(status: 415)
		}
		let offset = request.header("Upload-Offset").flatMap { Int64($0) }
		if let checksum = request.header("Upload-Checksum") {
			let expected = "sha256 " + Data(SHA256.hash(data: request.body)).base64EncodedString()
			if checksum != expected {
				return MockResponse(status: 460)
			}
		}
		return state.update { state -> MockResponse in
			guard var upload = state.uploads[id] else {
				return MockResponse(status: 404)
			}
			guard offset == Int64(upload.data.count) else {
				return MockResponse(status: 409)
			}
			upload.data.append(request.body)
			state.uploads[id] = upload
			return MockResponse(status: 204, headers: ["Upload-Offset": String(upload.data.count)])
		}
	}

	private func submit(_ request: MockRequest) -> MockResponse {
		guard let operationRequest = try? RemoteJSON.decoder().decode(OperationRequest.self, from: request.body) else {
			return .error(400, code: "invalid_request")
		}
		return state.update { state -> MockResponse in
			if let known = state.operations[operationRequest.id] {
				guard known.payload == request.body else {
					return .error(422, code: "operation_mismatch")
				}
				return .json(known.operation, status: 200)
			}
			if let upload = operationRequest.upload {
				// As the gateway does: an unknown (or expired) upload is not found, and nothing is stored
				guard let staged = state.uploads[upload] else {
					return .error(404, code: "not_found")
				}
				guard Int64(staged.data.count) == staged.length,
					Data(SHA256.hash(data: staged.data)).base64URLEncodedString() == operationRequest.uploadDigest
				else {
					return .error(409, code: "upload_mismatch")
				}
			}
			let pending = state.pendingPolls > 0
			let operation = RemoteOperation(
				id: operationRequest.id, operation: "op_\(state.operations.count + 1)", kind: operationRequest.kind,
				state: pending ? .pending : .committed, item: pending ? nil : state.resultItem, conflictItem: nil,
				contentID: nil, error: nil, authorityGeneration: 3)
			state.operations[operationRequest.id] = (request.body, operation)
			if state.loseResponses > 0 {
				state.loseResponses -= 1
				return .error(503, code: "content_unavailable")
			}
			return .json(operation, status: pending ? 202 : 201)
		}
	}

	private func poll(_ serverID: String) -> MockResponse {
		state.update { state -> MockResponse in
			guard let (id, known) = state.operations.first(where: { $0.value.operation.operation == serverID }) else {
				return .error(404, code: "not_found")
			}
			var operation = known.operation
			if state.pendingPolls > 0 {
				state.pendingPolls -= 1
			}
			if state.pendingPolls == 0 {
				operation.state = .committed
				operation.item = state.resultItem
				state.operations[id] = (known.payload, operation)
			}
			return .json(operation)
		}
	}

	var patches: [MockRequest] {
		state.value.requests.filter { $0.method == "PATCH" }
	}

	var submissions: [MockRequest] {
		state.value.requests.filter { $0.method == "POST" && $0.path == "/v1/operations" }
	}
}
