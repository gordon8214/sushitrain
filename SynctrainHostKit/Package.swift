// swift-tools-version: 6.0
// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import PackageDescription

let package = Package(
	name: "SynctrainHostKit",
	platforms: [.iOS(.v17), .macOS(.v15)],
	products: [
		.library(name: "SynctrainHostKit", targets: ["SynctrainHostKit"])
	],
	targets: [
		.target(name: "SynctrainHostKit"),
		.testTarget(name: "SynctrainHostKitTests", dependencies: ["SynctrainHostKit"]),
	]
)
