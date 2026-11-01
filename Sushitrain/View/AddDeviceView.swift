// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import SushitrainCore

struct AddDeviceView: View {
	@Environment(AppState.self) private var appState

	@Binding var suggestedDeviceID: String
	@Binding var shown: Bool

	@State var deviceID = ""
	@State private var showHelpAfterAdding = false
	@State private var showError = false
	@State private var errorText = ""
	@FocusState private var idFieldFocus: Bool

	var body: some View {
		NavigationStack {
			Form {
				Section("Device identifier") {
					HStack {
						IdenticonView(deviceID: deviceID).frame(width: 50)

						TextField(
							"", text: $deviceID, prompt: Text("XXXX-XXXX"), axis: .vertical
						)
						.focused($idFieldFocus)
						.foregroundColor(SushitrainIsValidDeviceID(deviceID) ? .green : .red)

					}
				}
			}
			.formStyle(.grouped)
			.onAppear {
				idFieldFocus = true
				deviceID = suggestedDeviceID
			}
			.toolbar {
				SheetButton(role: .add, isDisabled: deviceID.isEmpty || !SushitrainIsValidDeviceID(deviceID)) {
					self.add()
				}

				SheetButton(role: .cancel) {
					self.shown = false
				}
			}
			.navigationTitle("Add device")
			.alert(
				isPresented: $showError,
				content: {
					Alert(
						title: Text("Could not add device"), message: Text(errorText),
						dismissButton: .default(Text("OK")))
				}
			)
			.alert(isPresented: $showHelpAfterAdding) {
				Alert(
					title: Text("The device has been added"),
					message: Text(
						"The device has been added. To ensure a connection, ensure the other device accepts this device, or add this device there as well."
					),
					dismissButton: .default(Text("OK")) {
						self.shown = false
					})
			}
		}
	}

	private func add() {
		do {
			try appState.client.addPeer(self.deviceID)
			showHelpAfterAdding = true
		}
		catch let error {
			showError = true
			errorText = error.localizedDescription
		}
	}
}
