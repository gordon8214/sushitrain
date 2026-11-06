// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore

private struct GridItemView: View {
	let size: Double
	let file: SushitrainEntry

	var body: some View {
		ZStack(alignment: .topTrailing) {
			Rectangle()
				.frame(width: size, height: size)
				.backgroundStyle(Color.primary)
				.opacity(0.05)

			ThumbnailView(file: file, showFileName: true, showErrorMessages: false)
				.frame(width: size, height: size)
		}
	}
}

struct GridScrollView<HeaderContent: View, Content: View>: View {
	let directory: Route
	@Environment(\.desktopBrowserSession) private var browserSession
	@State private var position = ScrollPosition(edge: .top)
	let minColumns = 1
	let maxColumns = 9

	@ObservedObject var userSettings: AppUserSettings
	let header: () -> HeaderContent
	let content: (Int) -> Content

	@State private var scrollViewOffset = CGFloat.zero
	@State var magnifyBy: CGFloat = 1.0

	var body: some View {
		VStack {
			GeometryReader { reader in
				ScrollView {
					self.header()

					self.content(userSettings.browserGridColumns)
						.scaleEffect(magnifyBy, anchor: .top)
						.offset(x: 0, y: (scrollViewOffset + reader.size.height / 2.0) * (1 - magnifyBy))
				}
				.scrollPosition($position)
				.onScrollGeometryChange(for: CGFloat.self) {
					$0.contentOffset.y
				} action: { _, offset in
					scrollViewOffset = -offset
					if browserSession?.active == true { browserSession?.gridOffsets[directory] = offset }
				}
				.onAppear { if let offset = browserSession?.gridOffsets[directory] { position.scrollTo(y: offset) } }

				.highPriorityGesture(magnification)
				.accessibilityZoomAction { action in
					switch action.direction {
					case .zoomIn:
						userSettings.browserGridColumns -= 1

					case .zoomOut:
						userSettings.browserGridColumns += 1
					}
					userSettings.browserGridColumns = min(maxColumns, max(minColumns, userSettings.browserGridColumns))
				}
			}
		}
	}

	// Inspired by https://stackoverflow.com/a/73058175, CC-BY-SA 4.0
	private var magnification: some Gesture {
		MagnificationGesture()
			.onChanged { state in
				if state.isNormal && state > 0.01 {
					magnifyBy = state
				}
			}
			.onEnded { state in
				if state.isNormal && state > 0.0 {
					// Determine the appropriate number of columns
					let newZoom = Double(userSettings.browserGridColumns) * 1 / state
					let newColumnCount = min(maxColumns, max(minColumns, Int(newZoom.rounded(.toNearestOrAwayFromZero))))

					withAnimation(.spring(response: 0.8)) {
						magnifyBy = 1  // reset scaleEffect
						userSettings.browserGridColumns = newColumnCount
					}
				}
				else {
					withAnimation(.spring(response: 0.8)) {
						magnifyBy = 1  // reset scaleEffect
						userSettings.browserGridColumns = maxColumns
					}
				}
			}
	}
}

struct GridFilesView: View {
	@Environment(AppState.self) private var appState

	@ObservedObject var userSettings: AppUserSettings
	var prefix: String
	var files: [SushitrainEntry]
	var subdirectories: [SushitrainEntry]
	var folder: SushitrainFolder
	let columns: Int

	var body: some View {
		let gridColumns = Array(
			repeating: GridItem(.flexible(), spacing: 1.0), count: self.columns)

		LazyVGrid(columns: gridColumns, spacing: 1.0) {
			Section {
				// List subdirectories
				ForEach(subdirectories, id: \.self.id) { (subDirEntry: SushitrainEntry) in
					GeometryReader { geo in
						BrowserDirectoryLink(
							route: Route.folder(folderID: folder.folderID, prefix: "\(self.prefix)\(subDirEntry.fileName())/")
						) {
							EntryContextMenuWrapper(
								appState: appState,
								entry: subDirEntry, inFolder: self.folder, siblings: files, honorTapToPreview: true
							) {
								GridItemView(size: geo.size.width, file: subDirEntry).id(subDirEntry.id)
							}
						}
						.buttonStyle(PlainButtonStyle())
					}
					.aspectRatio(1, contentMode: .fit)
					.clipShape(.rect)
					.contentShape(.rect())
				}

				// List files
				ForEach(files, id: \.self.id) { file in
					GeometryReader { geo in
						EntryLinkView(
							appState: appState,
							entry: file, inFolder: self.folder, siblings: files, honorTapToPreview: true
						) {
							GridItemView(size: geo.size.width, file: file)
						}
						.buttonStyle(PlainButtonStyle())
					}
					.clipShape(.rect)
					.contentShape(.rect())
					.aspectRatio(1, contentMode: .fit)
				}
			} footer: {
				FilesFooterView(subdirectories: self.subdirectories.count, files: self.files.count)
					.padding()
			}
		}
		.padding(.horizontal, 2)
	}
}
