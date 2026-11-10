// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Testing
import UniformTypeIdentifiers

@testable import SynctrainFileProviderKit

@Suite struct PackageTypesTests {
	@Test(arguments: ["AppIcon.icon", "Demo.xcodeproj", "Notes.rtfd", "Synctrain.app", "Report.pages", "Loud.ICON"])
	func packagesAreDocuments(name: String) {
		#expect(PackageTypes.type(ofDirectoryNamed: name)?.conforms(to: .package) == true)
	}

	@Test(arguments: [
		"Photos", "v1.2", "com.example.app-data", "Assets.xcassets", "SushitrainCore.xcframework", "Base.lproj", ".hidden",
		"Trailing.",
	])
	func foldersStayFolders(name: String) {
		#expect(PackageTypes.type(ofDirectoryNamed: name) == nil)
	}

	/// Each entry of the list (which stands in for the Mac's types iOS lacks) is in lower case, and gives a directory
	/// a package type on this system too
	@Test(arguments: PackageTypes.macPackageExtensions.sorted())
	func listedExtensionsArePackages(pathExtension: String) {
		#expect(pathExtension == pathExtension.lowercased())
		#expect(PackageTypes.type(ofDirectoryNamed: "Item." + pathExtension)?.conforms(to: .package) == true)
	}

	/// Mac-only formats named with ordinary words are left out of the list: without a gateway's mark, such directories
	/// stay folders (a Mac declares them, so only iOS can show it)
	@Test(arguments: ["action", "definition", "help", "trace", "workflow"])
	func ordinaryWordsStayFolders(pathExtension: String) {
		#expect(!PackageTypes.macPackageExtensions.contains(pathExtension))
		#if os(iOS)
			#expect(PackageTypes.type(ofDirectoryNamed: "Docs." + pathExtension) == nil)
			#expect(PackageTypes.type(ofDirectoryNamed: "Docs." + pathExtension, markedByGateway: true) != nil)
		#endif
	}

	/// A reply keeps the type the system gave a directory, folder or package
	@Test func repliesKeepTheTemplatesType() throws {
		let package = try #require(UTType(filenameExtension: "zzqpkg", conformingTo: .package))
		#expect(PackageTypes.replyType(ofDirectoryTemplate: .folder) == .folder)
		#expect(PackageTypes.replyType(ofDirectoryTemplate: package) == package)
		#expect(PackageTypes.replyType(ofDirectoryTemplate: .rtfd) == .rtfd)
		#expect(PackageTypes.replyType(ofDirectoryTemplate: .plainText) == nil)
		#expect(PackageTypes.replyType(ofDirectoryTemplate: nil) == nil)
	}

	/// A gateway on a Mac marks the packages of apps neither this device nor the list knows
	@Test func packagesTheGatewayMarksAreDocuments() throws {
		let type = try #require(PackageTypes.type(ofDirectoryNamed: "Site.zzqpkg", markedByGateway: true))
		#expect(type.conforms(to: .package) && type.isDynamic && type.preferredFilenameExtension == "zzqpkg")
		#expect(PackageTypes.type(ofDirectoryNamed: "Site.zzqpkg") == nil)
		// A Mac counts a directory as a package by its extension
		#expect(PackageTypes.type(ofDirectoryNamed: "Photos", markedByGateway: true) == nil)
		// A type this device declares wins over the dynamic one
		#expect(PackageTypes.type(ofDirectoryNamed: "Notes.rtfd", markedByGateway: true) == UTType.rtfd)
	}

	/// What a gateway on a Mac answers: the package types of this system, whatever the case of the extension
	@Test func declaredPackages() {
		#if os(macOS)
			let declared = ["icon", "xcodeproj", "rtfd", "app", "PAGES"]
		#else
			let declared = ["rtfd", "app", "PAGES"]
		#endif
		for pathExtension in declared {
			#expect(PackageTypes.isDeclaredPackage(extension: pathExtension), "\(pathExtension)")
		}
		for pathExtension in ["", "txt", "jpg", "lproj", "xcassets", "zzqpkg"] {
			#expect(!PackageTypes.isDeclaredPackage(extension: pathExtension), "\(pathExtension)")
		}
	}
}
