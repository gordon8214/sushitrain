// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import Foundation
import UniformTypeIdentifiers

/// Directories that are one document (a package or bundle), like an Icon Composer icon or an Xcode project, rather than
/// folders to browse
public enum PackageTypes {
	/// The type of a directory with this name when it is a package, nil when it is a folder. The directories come from
	/// Macs, and iOS declares far fewer package types than macOS (not `.icon` or `.xcodeproj`, say), so packages a Mac
	/// knows count as well as the ones this device declares. iCloud Drive also shows those as documents on an iPhone.
	///
	/// `markedByGateway`: a gateway in Synctrain on a Mac counts the directory as a package, which covers the types of
	/// every app installed there. Without it (`synctrain-gateway` marks none), Apple's and open formats still count.
	public static func type(ofDirectoryNamed name: String, markedByGateway: Bool = false) -> UTType? {
		let pathExtension = (name as NSString).pathExtension
		guard !pathExtension.isEmpty else {
			return nil
		}
		if let type = declaredType(forExtension: pathExtension) {
			return type
		}
		guard markedByGateway || macPackageExtensions.contains(pathExtension.lowercased()) else {
			return nil
		}
		// A dynamic type that conforms to a package, and keeps the extension for the system's generic icon
		return UTType(filenameExtension: pathExtension, conformingTo: .package)
	}

	/// Whether this system declares a package type for directories with this extension: on a Mac, Apple's and those of
	/// every installed app. A gateway on a Mac tells clients so.
	public static func isDeclaredPackage(extension pathExtension: String) -> Bool {
		!pathExtension.isEmpty && declaredType(forExtension: pathExtension) != nil
	}

	/// The type a directory has in the reply to a change the system made (a create or modify): the one in the system's
	/// template, nil when the template has none. The system refuses a reply that turns its folder into a package or
	/// the other way around (`itemKindMismatch` on iOS 27.2: an upload error, and nothing inside is sent). That
	/// happens both ways: a package copied in from where iOS sees a folder (On My iPhone), and a package iOS was given
	/// a type for elsewhere whose name doesn't make it one here.
	public static func replyType(ofDirectoryTemplate template: UTType?) -> UTType? {
		guard let template, template.conforms(to: .directory) else {
			return nil
		}
		return template
	}

	private static func declaredType(forExtension pathExtension: String) -> UTType? {
		UTType.types(tag: pathExtension, tagClass: .filenameExtension, conformingTo: .package).first { !$0.isDynamic }
	}

	/// The extensions of the types that conform to `com.apple.package` on a Mac, from Apple and from open formats, in
	/// lower case: from LaunchServices on macOS 27.2 with Xcode 27.2. Every package type iOS 27.2 declares is in it.
	/// Left out: Mac-only formats named with ordinary words (Automator's `action`, `definition` and `workflow`, Help's
	/// `help`, Instruments' `trace`), which are more likely the names of ordinary folders. A folder shown as a document
	/// hides its contents in Files until the location is rebuilt; a package shown as a folder can still be browsed.
	static let macPackageExtensions: Set<String> = [
		"aimodel", "aimodelc", "aipackage", "aiperf", "aplibrary", "app", "appdownload", "appex",
		"avchd", "backupbundle", "band", "bundle", "caction", "cannedsearch", "dext", "dist",
		"distz", "doccarchive", "docset", "dsym", "dvtplugin", "epub", "fcpproj", "fmadapter", "fs",
		"gamesavebundle", "gbproj", "gputrace", "gtdbg", "hvpl", "ibooks", "ibplugin", "icbu", "icon",
		"ideplugin", "ilmbplugin", "imovielibrary", "instrdst", "ite", "itlp", "kext", "key", "key-tef", "kth",
		"logarchive", "lpdf", "magnifier", "mdimporter", "menu", "migratedaplibrary", "migratedphotolibrary",
		"mlmodelc", "mlpackage", "mlperf", "mlproj", "movpkg", "mpkg", "mpsgraphpackage", "mtlpackage",
		"musiclibrary", "nib", "nmbtemplate", "notesarchive", "numbers", "numbers-tef", "octest", "order", "pages",
		"pages-tef", "pbproj", "photolibrary", "photosasset", "photoslibrary", "pkg", "pkpass", "placeholder",
		"playground", "plugin", "pluginkit", "ppp", "prefpane", "pvt", "qlgenerator", "qtpxcomposition",
		"remoteplaceholder", "rtfd", "saver", "scptd", "service", "slidesaver", "sparsebundle",
		"stickiesappexport", "storyboardc", "suggestionsassets", "swiftpm", "sysprefex", "systemextension",
		"template", "textbundle", "theater", "tvlibrary", "videoslibrary", "viewhierarchy", "wdgt",
		"webplugin", "widgetkitsim", "xcappdata", "xcarchive", "xccrashpoint", "xcdatamodel",
		"xcdatamodeld", "xcdiskwritepoint", "xcfeedbackpoint", "xcfilescontainer", "xchangpoint", "xclaunchpoint",
		"xcloc", "xcmappingmodel", "xcode", "xcodeproj", "xcodesettings", "xcplaygroundpage", "xcplugin",
		"xcprocessorusagepoint", "xcresult", "xcsnapshots", "xctest", "xctestproducts", "xctoolchain",
		"xcworkspace", "xpc",
	]
}
