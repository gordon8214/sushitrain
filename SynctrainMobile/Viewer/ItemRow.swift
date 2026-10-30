// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SynctrainFileProviderKit
import UniformTypeIdentifiers

/// A file or directory in a list
struct ItemRow: View {
	let item: IndexItem
	/// Shown instead of the size and date, such as where a search result is
	var detail: String? = nil

	private var subtitle: String? {
		if let detail {
			return detail
		}
		guard item.kind == .file else {
			return nil
		}
		let size = item.size.formatted(.byteCount(style: .file))
		guard let date = item.modificationDate else {
			return size
		}
		return "\(size), \(date.formatted(date: .abbreviated, time: .shortened))"
	}

	var body: some View {
		HStack(spacing: 12) {
			ItemThumbnailView(item: item, side: 44)
			VStack(alignment: .leading, spacing: 2) {
				Text(item.name).lineLimit(2)
				if let subtitle {
					Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1)
				}
			}
			Spacer(minLength: 0)
			if FileNames.isConflictCopy(item.name) {
				Image(systemName: "exclamationmark.triangle.fill")
					.foregroundStyle(.orange)
					.accessibilityLabel("Conflict copy")
			}
			if item.pending != .none {
				Image(systemName: "arrow.up.circle")
					.foregroundStyle(.secondary)
					.accessibilityLabel("Not sent to the gateway yet")
			}
		}
	}
}

/// A file's thumbnail, or the icon of its type
struct ItemThumbnailView: View {
	let item: IndexItem
	let side: CGFloat
	@Environment(\.displayScale) private var displayScale
	@State private var image: UIImage? = nil

	private var symbol: String {
		guard item.kind == .file else {
			return "folder.fill"
		}
		let type = UTType(filenameExtension: (item.name as NSString).pathExtension) ?? .data
		if type.conforms(to: .image) {
			return "photo"
		}
		if type.conforms(to: .movie) {
			return "film"
		}
		if type.conforms(to: .audio) {
			return "music.note"
		}
		if type.conforms(to: .pdf) || type.conforms(to: .text) {
			return "doc.text"
		}
		return "doc"
	}

	var body: some View {
		Group {
			if let image {
				Image(uiImage: image)
					.resizable()
					.scaledToFill()
					.frame(width: side, height: side)
					.clipShape(.rect(cornerRadius: 6))
			}
			else {
				Image(systemName: symbol)
					.font(.title2)
					.foregroundStyle(item.kind == .file ? Color.secondary : Color.accentColor)
					.frame(width: side, height: side)
			}
		}
		.accessibilityHidden(true)
		.task(id: item.blocksHash) {
			image = await ThumbnailLoader.shared.thumbnail(of: item, pixels: Int(side * displayScale))
		}
	}
}
