// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI

struct SheetButton: ToolbarContent {
	enum SheetButtonRole {
		case done
		case cancel
		case add
		case save

		var buttonRole: ButtonRole {
			switch self {
			case .done: return .close
			case .cancel: return .cancel
			case .add: return .confirm
			case .save: return .confirm
			}
		}

		var placement: ToolbarItemPlacement {
			switch self {
			case .add: return .confirmationAction
			case .done: return .confirmationAction
			case .cancel: return .cancellationAction
			case .save: return .confirmationAction
			}
		}
	}

	@Environment(\.editMode) private var editMode

	let role: SheetButtonRole
	var isDisabled: Bool = false
	let action: () -> Void

	var body: some ToolbarContent {
		ToolbarItem(placement: self.role.placement) {
			Button(role: self.role.buttonRole, action: self.action)
				.disabled(self.isDisabled || self.editMode?.wrappedValue.isEditing == true)
		}
	}
}
