// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
@preconcurrency import SushitrainCore

private struct AboutIgnoresSection: View {
	var body: some View {
		Section {
			Text(
				"**This is an advanced feature and should only be used if you know what you are doing.** For more information about how ignore patterns work, see [the Syncthing documentation on ignoring files](https://docs.syncthing.net/users/ignoring.html)."
			)
		} header: {
			Label("About ignore patterns", systemImage: "questionmark.circle")
		}
	}
}

private enum IgnoresError: LocalizedError {
	case changedElsewhere

	var errorDescription: String? {
		return String(
			localized:
				"These patterns were changed elsewhere while you were editing them. Save again to replace them, or cancel to keep them."
		)
	}
}

/// View for editing the ignore file of a folder
struct IgnoresView: View {
	var folder: SushitrainFolder

	@Environment(AppState.self) private var appState
	@Environment(\.dismiss) private var dismiss

	@State var ignoreLines: [String] = []
	@State private var loadedLines: [String] = []
	@State var error: ErrorMessage? = nil
	@State var loading = true
	@State var loadError: String? = nil
	@State var showSaveConfirmation = false

	private static let prependedLine = "# Synctrain user-defined ignore file"
	private static let defaultIgnoreLines = [
		"(?d).DS_Store", "(?d)Thumbs.db", "(?d)desktop.ini", ".AppleDB", ".AppleDesktop", ".Trashes",
		".Spotlight-V100", ".localized",
	]

	var body: some View {
		List {
			AboutIgnoresSection()

			Section {
				if let loadError {
					ContentUnavailableView {
						Label("Unable to load ignore patterns", systemImage: "exclamationmark.triangle")
					} description: {
						Text(loadError)
					} actions: {
						Button("Retry") {
							self.update()
						}
					}
				}
				else if ignoreLines.isEmpty && !loading {
					Text("No ignore patterns defined")
				}

				ForEach(Array(ignoreLines.enumerated()), id: \.offset) { idx in
					HStack {
						TextField(
							"",
							text: Binding(
								get: {
									if idx.offset < self.ignoreLines.count {
										return self.ignoreLines[idx.offset]
									}
									return ""
								},
								set: { nv in
									if idx.offset < self.ignoreLines.count {
										self.ignoreLines[idx.offset] = nv
									}
								}), prompt: Text("Pattern...")
						)
						.autocorrectionDisabled()

						Button("Delete", systemImage: "trash") {
							ignoreLines.remove(at: idx.offset)
						}
						.labelStyle(.iconOnly)
						.buttonStyle(.borderless)
					}
				}
				.onDelete(perform: { indexSet in
					ignoreLines.remove(atOffsets: indexSet)
				})
				.disabled(self.loading)

				Button("Add line", systemImage: "plus") {
					self.ignoreLines.append("")
				}.disabled(self.loading)
					.buttonStyle(.link)
			} header: {
				Text("Ignore patterns")
			} footer: {
				Text(
					"Files and subdirectories whose paths match any of the patterns above will not be synchronized with other devices. Existing items matching the patterns will not be updated. [Learn more about the pattern syntax...](https://docs.syncthing.net/users/ignoring.html#patterns)"
				)
				.lineLimit(3, reservesSpace: true)
			}

			Section {
				Button("Set default patterns") {
					self.ignoreLines = Self.defaultIgnoreLines
				}
				.buttonStyle(.link)

				Button("Remove all patterns", role: .destructive) {
					self.ignoreLines = []
				}.disabled(self.ignoreLines.isEmpty)
					.buttonStyle(.link)
			}.disabled(self.loading)
		}
		.listStyle(.inset(alternatesRowBackgrounds: true))
		.task {
			self.update()
		}
		// Patterns saved elsewhere meanwhile, as in the web GUI, which scans the folder, unless they are being edited here
		.onChange(of: appState.folderScanRevisions[self.folder.folderID]) {
			if self.loadError == nil && self.ignoreLines == self.loadedLines {
				self.update()
			}
		}
		.navigationTitle("Files to ignore")
		.toolbar {
			ToolbarItem(
				placement: .confirmationAction,
				content: {
					Button("Save") {
						showSaveConfirmation = true
					}
					.disabled(self.loading)
					.confirmationDialog(
						"Are you sure you want to save the patterns? Files and subdirectories that match the patterns will not be synchronized with other devices.",
						isPresented: $showSaveConfirmation, titleVisibility: .visible
					) {
						Button("Apply patterns") {
							Task {
								await self.write()
								if self.error == nil {
									self.showSaveConfirmation = false
									self.dismiss()
								}
							}
						}.disabled(self.loading)
					}
				})

			SheetButton(role: .cancel) {
				dismiss()
			}
		}
		.errorAlert($error)
	}

	private func write() async {
		do {
			Log.info("writing ignore lines \(self.ignoreLines)")
			if self.error == nil {
				// Patterns saved elsewhere meanwhile aren't replaced unseen. They count as seen now, for a second save.
				let current = try self.readLines()
				if current != self.loadedLines {
					self.loadedLines = current
					throw IgnoresError.changedElsewhere
				}
				var lines = self.ignoreLines
				lines.removeAll { $0.isEmpty }
				lines.insert(Self.prependedLine, at: 0)
				try self.folder.setIgnoreLines(SushitrainListOfStrings.from(lines))
			}
		}
		catch {
			Log.warn("failed to write ignore file: \(error)")
			self.error = ErrorMessage(error)
		}
	}

	private func readLines() throws -> [String] {
		var lines = (try self.folder.ignoreLines()).asArray()
		Log.info("Got lines: \(lines)")
		if !lines.isEmpty && lines[0] == Self.prependedLine {
			lines.remove(at: 0)
		}
		return lines
	}

	private func update() {
		do {
			self.loadError = nil
			self.loading = true
			let lines = try self.readLines()
			self.ignoreLines = lines
			self.loadedLines = lines
		}
		catch {
			self.ignoreLines = []
			self.loadedLines = []
			Log.warn("failed to read ignore file: \(error)")
			self.loadError = error.localizedDescription
			// Remain in the loading state, as saving would replace a file that could not be read
			return
		}
		self.loading = false
	}
}
