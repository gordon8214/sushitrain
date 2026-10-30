// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import Testing

@testable import SynctrainHostKit

/// A sandboxed install's container with engine data, and the folder the unsandboxed app keeps it in
private final class MigrationFixture {
	let root: URL
	let source: URL
	let destination: URL
	let migration: ContainerMigration

	init() throws {
		self.root = FileManager.default.temporaryDirectory.appending(path: "hostkit-migration-\(UUID().uuidString)")
		self.source = root.appending(path: "Containers/test.Synctrain/Data/Library/Application Support")
		self.destination = root.appending(path: "Library/Application Support/Synctrain")
		try FileManager.default.createDirectory(at: source, withIntermediateDirectories: true)
		try FileManager.default.createDirectory(
			at: destination.deletingLastPathComponent(), withIntermediateDirectories: true)
		self.migration = ContainerMigration(source: source, destination: destination)
	}

	deinit {
		try? FileManager.default.removeItem(at: root)
	}

	func write(_ relativePath: String, _ contents: String = "x", in base: URL? = nil) throws {
		let url = (base ?? source).appending(path: relativePath)
		try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
		try Data(contents.utf8).write(to: url)
	}

	func read(_ relativePath: String, in base: URL? = nil) -> String? {
		(try? Data(contentsOf: (base ?? destination).appending(path: relativePath))).map {
			String(decoding: $0, as: UTF8.self)
		}
	}

	func writeEngineData() throws {
		try write("config.xml", "<configuration version=\"52\"/>")
		try write("cert.pem", "certificate")
		try write("key.pem", "key")
		try write("index-v2/main.db", "database")
		try write("index-v2/main.db-wal", "journal")
		try write("remote-managed.json", "[]")
		try write("synctrain-gateway/service.sqlite", "service")
		try write("synctrain-gateway/content/spool", "spool")
		try write("synctrain-gateway/thumbnails/a.jpg", "thumbnail")
		try write("RemoteFileService/certificate.pem", "chosen certificate")
		try write("syncthing.lock", "")
		try write("unrelated.txt", "not the engine's")
	}
}

@Test func anEmptyContainerHasNothingToMigrate() throws {
	let f = try MigrationFixture()
	#expect(f.migration.state() == .nothingToMigrate)
}

@Test func migrationCopiesTheEngineDataAndLeavesTheContainer() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	#expect(f.migration.state() == .needed)

	let receipt = try f.migration.run(now: Date(timeIntervalSince1970: 1_800_000_000))

	for (path, contents) in [
		("config.xml", "<configuration version=\"52\"/>"), ("cert.pem", "certificate"), ("key.pem", "key"),
		("index-v2/main.db", "database"), ("index-v2/main.db-wal", "journal"), ("remote-managed.json", "[]"),
		("synctrain-gateway/service.sqlite", "service"), ("RemoteFileService/certificate.pem", "chosen certificate"),
	] {
		#expect(f.read(path) == contents, "\(path)")
		#expect(f.read(path, in: f.source) == contents, "\(path) left in the container")
	}
	for left in ["synctrain-gateway/content", "synctrain-gateway/thumbnails", "unrelated.txt", "syncthing.lock"] {
		#expect(!FileManager.default.fileExists(atPath: f.destination.appending(path: left).path), "\(left)")
	}
	#expect(!FileManager.default.fileExists(atPath: f.migration.staging.path))
	#expect(receipt.certificateSHA256 == ContainerMigration.sha256(of: f.source.appending(path: "cert.pem")))
	#expect(receipt.items.contains("index-v2") && !receipt.items.contains("https-cert.pem"))
	#expect(f.migration.receipt() == receipt)
	#expect(f.migration.state() == .done)
}

@Test func migrationKeepsTheDataPrivate() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try f.migration.run()
	let attributes = try FileManager.default.attributesOfItem(atPath: f.destination.path)
	#expect((attributes[.posixPermissions] as? Int) == 0o700)
}

@Test func migrationReplacesAnEmptyDestinationOnly() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try FileManager.default.createDirectory(at: f.destination, withIntermediateDirectories: true)
	try f.migration.run()
	#expect(f.read("cert.pem") == "certificate")
}

@Test func anIdentityAtTheDestinationIsNeverReplaced() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try f.write("cert.pem", "another identity", in: f.destination)
	#expect(f.migration.state() == .destinationInUse)
	#expect(throws: ContainerMigration.MigrationError.destinationInUse) {
		try f.migration.run()
	}
	#expect(f.read("cert.pem") == "another identity")
	#expect(f.read("config.xml") == nil)
}

@Test func aRunningSandboxedEngineBlocksTheMigration() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	let descriptor = open(f.source.appending(path: "syncthing.lock").path, O_RDONLY)
	#expect(descriptor >= 0)
	defer { close(descriptor) }
	#expect(flock(descriptor, LOCK_EX | LOCK_NB) == 0)

	#expect(throws: ContainerMigration.MigrationError.containerInUse) {
		try f.migration.run()
	}
	#expect(!FileManager.default.fileExists(atPath: f.destination.path))
	#expect(!FileManager.default.fileExists(atPath: f.migration.staging.path))
}

@Test func anInterruptedMigrationStartsOver() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try f.write("config.xml", "half a copy", in: f.migration.staging)
	try f.migration.run()
	#expect(f.read("config.xml") == "<configuration version=\"52\"/>")
	#expect(!FileManager.default.fileExists(atPath: f.migration.staging.path))
}

@Test func onlyPreferencesNotSetYetAreImported() {
	let imported = ContainerPreferences.valuesToImport(
		from: [
			"hideInDock": true, "loggingEnabled": true, "folderSettings": Data(), "NSWindow Frame main": "0 0 1 1",
			"NSNavLastRootDirectory": "~/Library/Containers", "AppleLanguages": ["nl"],
		],
		current: ["hideInDock": false])
	#expect(Set(imported.keys) == ["loggingEnabled", "folderSettings", "AppleLanguages"])
}

@Test func anUnreadableContainerIsNotTakenForAnEmptyOne() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try FileManager.default.setAttributes([.posixPermissions: 0], ofItemAtPath: f.source.path)
	defer { try? FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: f.source.path) }

	#expect(f.migration.state() == .unreadable(EACCES))
	#expect(throws: ContainerMigration.MigrationError.containerUnreadable(EACCES)) {
		try f.migration.run()
	}
	#expect(!FileManager.default.fileExists(atPath: f.destination.path))
}

@Test func anIdentityWithoutAConfigurationIsTakenOver() throws {
	// As "Start with a new configuration" leaves it
	let f = try MigrationFixture()
	try f.write("cert.pem", "certificate")
	try f.write("key.pem", "key")
	try f.write("config.xml.broken-20261030-120000", "damaged")
	#expect(f.migration.state() == .needed)
	try f.migration.run()
	#expect(f.read("cert.pem") == "certificate")
	#expect(f.read("key.pem") == "key")
}

@Test func whatAFailedAttemptLeftIsSetAside() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try f.write("syncthing.lock", "", in: f.destination)
	#expect(f.migration.state() == .needed)
	try f.migration.run()
	#expect(f.read("cert.pem") == "certificate")
	let aside = try FileManager.default.contentsOfDirectory(atPath: f.destination.deletingLastPathComponent().path)
		.filter { $0.hasPrefix("Synctrain.premigration-") }
	#expect(aside.count == 1)
}

@Test func aSymbolicLinkStopsTheMigration() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try FileManager.default.removeItem(at: f.source.appending(path: "cert.pem"))
	try f.write("elsewhere/cert.pem", "certificate", in: f.root)
	try FileManager.default.createSymbolicLink(
		at: f.source.appending(path: "cert.pem"), withDestinationURL: f.root.appending(path: "elsewhere/cert.pem"))

	#expect(throws: ContainerMigration.MigrationError.symbolicLink("cert.pem")) {
		try f.migration.run()
	}
	#expect(!FileManager.default.fileExists(atPath: f.destination.path))
	#expect(!FileManager.default.fileExists(atPath: f.migration.staging.path))
}

@Test func aFinishedMigrationIsNeverRunAgain() throws {
	let f = try MigrationFixture()
	try f.writeEngineData()
	try f.migration.run()
	#expect(throws: ContainerMigration.MigrationError.destinationInUse) {
		try f.migration.run()
	}
	#expect(f.read("cert.pem") == "certificate")
}
