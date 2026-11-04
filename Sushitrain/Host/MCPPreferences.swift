// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#if os(macOS)
	import Foundation
	import SynctrainFileProviderKit
	import SynctrainHostKit
	@preconcurrency import SushitrainCore

	struct MCPPreferenceEditor {
		var baseline: [String: String]
		var expires: Date
	}
	extension MCPIntegration {
		private static let preferenceDefaults: [String: Any] = [
			"streamingLimitMbitsPerSec": 0, "maxBytesForPreview": 2_097_152, "browserViewStyle": "list", "browserGridColumns": 3,
			"loggingEnabled": false, "keepLogLevels": false, "keptLogLevels": "", "dotFilesHidden": true,
			"hideHiddenFolders": false, "foldersViewMetric": "", "previewVideos": false, "tapFileToPreview": false,
			"cacheThumbnailsToDisk": true, "cacheThumbnailsToFolderID": "", "showThumbnailsInSearchResults": true,
			"automaticallySwitchViewStyle": true, "automaticallyShowWebpages": true,
			"menuFolderAction": MenuFolderAction.finder.rawValue,
			"enginePriority": "background", "disableDevicesOnCellular": false, "disableDevicesOnMetered": false,
			"disableDevicesInLowPowerMode": false, "cacheLimitBytes": 5_368_709_120
		]
		func preferences(_ method: String, parameters: [String: Any]) throws -> Data {
			if method.hasPrefix("policy.") || method == "resources.set" { return try policy(method, parameters: parameters) }
			preferenceEditors = preferenceEditors.filter { $0.value.expires > Date() }
			let token = parameters["Token"] as? String ?? ""
			if method == "mcp.preferences.close" {
				preferenceEditors[token] = nil
				return Data("null".utf8)
			}
			if method == "mcp.preferences.get" { return try preferenceEditor(parameters: parameters) }
			guard let editor = preferenceEditors[token], let values = parameters["Values"] as? [String: String] else {
				throw MCPAppError("The editor expired; reopen settings.")
			}
			let now = currentPreferences()
			var converted: [String: Any] = [:]
			for (key, value) in values {
				guard let fallback = Self.preferenceDefaults[key] else { throw MCPAppError("Unknown app setting: \(key)") }
				guard now[key] == editor.baseline[key] || now[key] == value else {
					throw MCPAppError("This setting changed in the app; reopen the editor.")
				}
				converted[key] = try preferenceValue(key: key, value: value, fallback: fallback)
			}

			for (key, value) in converted { UserDefaults.standard.set(value, forKey: key) }
			preferenceEditors[token] = nil
			if converted["enginePriority"] != nil { EnginePriority.stored.apply() }
			appState?.applySettings()
			let restart =
				converted["loggingEnabled"] != nil
				&& UserDefaults.standard.bool(forKey: "loggingEnabled") != (appState?.isLoggingToFile ?? false)
			if converted["keptLogLevels"] != nil {
				appState?.client.applyLogLevels(UserDefaults.standard.string(forKey: "keptLogLevels") ?? "")
			}
			return try Self.json(["restartRequired": restart])
		}

		func preferenceSnapshot() -> [String: String] {
			Self.preferenceDefaults.mapValues { String(describing: $0) }
		}
		func currentPreferences() -> [String: String] {
			var result = preferenceSnapshot()
			for (key, fallback) in Self.preferenceDefaults {
				let value = UserDefaults.standard.object(forKey: key) ?? fallback
				if let bool = value as? NSNumber, CFGetTypeID(bool) == CFBooleanGetTypeID() {
					result[key] = bool.boolValue ? "true" : "false"
				} else {
					result[key] = String(describing: value)
				}
			}
			return result
		}
		private func policy(_ method: String, parameters: [String: Any]) throws -> Data {
			if method == "policy.get" {
				return try Self.json([
					"Metered": UserDefaults.standard.bool(forKey: "disableDevicesOnMetered"),
					"Cellular": UserDefaults.standard.bool(forKey: "disableDevicesOnCellular"),
					"PowerSaver": UserDefaults.standard.bool(forKey: "disableDevicesInLowPowerMode"),
					"ResourcePriority": EnginePriority.stored.rawValue
				])
			}
			if method == "policy.save" {
				for (input, key) in [
					"Metered": "disableDevicesOnMetered", "Cellular": "disableDevicesOnCellular",
					"PowerSaver": "disableDevicesInLowPowerMode"
				] {
					guard let enabled = parameters[input] as? Bool else { throw MCPAppError("Supply all three pause policies.") }
					UserDefaults.standard.set(enabled, forKey: key)
				}
				appState?.applySettings()
				return Data("null".utf8)
			}
			if method == "resources.set" {
				guard let priority = (parameters["Priority"] as? String).flatMap(EnginePriority.init(rawValue:)) else {
					throw MCPAppError("Choose normal, utility, or background.")
				}
				UserDefaults.standard.set(priority.rawValue, forKey: EnginePriority.storageKey)
				priority.apply()
				return Data("null".utf8)
			}
			throw MCPAppError("Unsupported policy operation.")
		}
		private func preferenceEditor(parameters: [String: Any]) throws -> Data {

			guard ["app", "files"].contains(parameters["Section"] as? String ?? "") else {
				throw MCPAppError("Choose app or files settings in Files mode.")
			}
			guard preferenceEditors.count < 64 else { throw MCPAppError("Close an unused settings editor.") }
			let values = currentPreferences()
			let token = "app:" + UUID().uuidString
			preferenceEditors[token] = MCPPreferenceEditor(baseline: values, expires: Date().addingTimeInterval(1800))
			let fields = Self.preferenceDefaults.keys.sorted().map { key -> [String: Any] in
				let fallback = Self.preferenceDefaults[key]!
				let kind = fallback is Bool ? "bool" : fallback is Int ? "int" : "string"
				return ["Path": key, "Kind": kind, "Value": values[key] ?? "", "IsSecret": false, "IsReadOnly": false]
			}
			return try Self.json(["Token": token, "Section": "app", "Fields": fields])

		}
		private func preferenceValue(key: String, value: String, fallback: Any) throws -> Any {
			if fallback is Bool {
				guard value == "true" || value == "false" else { throw MCPAppError("Enter true or false for \(key).") }
				return value == "true"
			} else if fallback is Int {
				guard let number = Int(value), number >= 0 else {
					throw MCPAppError("Enter a nonnegative whole number for \(key).")
				}
				if key == "browserGridColumns", !(1...20).contains(number) { throw MCPAppError("Grid columns must be 1–20.") }
				if key == "cacheLimitBytes", number == 0 { throw MCPAppError("The cache budget must be positive.") }
				return number
			} else {
				let choices: [String: [String]] = [
					"enginePriority": ["normal", "utility", "background"], "browserViewStyle": ["list", "grid"],
					"menuFolderAction": ["hide", "finder", "browser"]
				]
				if let choices = choices[key], !choices.contains(value) {
					throw MCPAppError("Choose a supported value for \(key).")
				}
				return value
			}
		}
	}
#endif
