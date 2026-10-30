// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

struct AboutView: View {
	var body: some View {
		Form {
			Section {
				LabeledContent("Version", value: Bundle.main.releaseVersionNumber ?? "-")
				LabeledContent("Build", value: Bundle.main.buildVersionNumber ?? "-")
			} footer: {
				Text("Synctrain opens your folders through a gateway that synchronizes them with Syncthing.")
			}
		}
		.navigationTitle("About Synctrain")
	}
}
