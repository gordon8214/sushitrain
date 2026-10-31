#!/bin/bash
# Copyright (C) 2026 Gordon
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

# Turns a Developer ID archive of the Mac app into a disk image to distribute: it checks how the app is signed, has it
# notarized and staples the ticket, then does the same for a disk image around it. Both carry their own ticket, so that
# Gatekeeper accepts either offline, also once the app is copied out of the image. See Docs/macos.md.
#
#   Scripts/notarize-mac.sh --check                   checks the notary credentials and provisioning profiles, and nothing else
#   Scripts/notarize-mac.sh <archive> <output folder>
#
# Notary credentials come from the environment, never from the repository. Either NOTARY_KEYCHAIN_PROFILE names a
# profile saved with `xcrun notarytool store-credentials`, or NOTARY_KEY_PATH, NOTARY_KEY_ID and NOTARY_ISSUER_ID give
# an App Store Connect API key.
set -euo pipefail

fail() {
	echo "error: $*" >&2
	exit 1
}

if [[ -n "${NOTARY_KEYCHAIN_PROFILE:-}" ]]; then
	notary_credentials=(--keychain-profile "$NOTARY_KEYCHAIN_PROFILE")
elif [[ -n "${NOTARY_KEY_PATH:-}" && -n "${NOTARY_KEY_ID:-}" && -n "${NOTARY_ISSUER_ID:-}" ]]; then
	[[ -f "$NOTARY_KEY_PATH" ]] || fail "NOTARY_KEY_PATH ($NOTARY_KEY_PATH) is not a file"
	notary_credentials=(--key "$NOTARY_KEY_PATH" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER_ID")
else
	fail "set NOTARY_KEYCHAIN_PROFILE, or NOTARY_KEY_PATH, NOTARY_KEY_ID and NOTARY_ISSUER_ID"
fi

# Keychain sharing between the app and its File Provider takes a profile for each. Without them Xcode can't sign the app,
# which this finds out before a build instead of during one.
check_profiles() {
	local name file found
	for name in "Synctrain Developer ID" "Synctrain File Provider Developer ID" "Synctrain File Provider UI Developer ID"; do
		found=false
		for file in "$HOME/Library/MobileDevice/Provisioning Profiles/"* \
			"$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles/"*; do
			[[ -f "$file" ]] || continue
			if [[ "$(security cms -D -i "$file" 2>/dev/null | plutil -extract Name raw -o - - 2>/dev/null)" == "$name" ]]; then
				found=true
				break
			fi
		done
		$found || fail "the provisioning profile \"$name\" is not installed (see Docs/macos.md)"
	done
}

if [[ "${1:-}" == "--check" ]]; then
	check_profiles
	exit 0
fi
[[ $# -eq 2 ]] || fail "usage: $0 <archive> <output folder>"

archive="$1"
output="$2"
archived_app="$archive/Products/Applications/Synctrain.app"
[[ -d "$archived_app" ]] || fail "$archived_app does not exist"

plist_value() {
	plutil -extract "$2" raw -o - "$1"
}

entitlements() {
	codesign --display --entitlements - --xml "$1" 2>/dev/null
}

# Notarization checks some of this too, but only after an upload and a wait. It doesn't check the app group at all: a
# group without the team's prefix passes notarization and Gatekeeper, and the app then can't reach its extension's files.
check_signature() {
	local code="$1" details
	details=$(codesign --display --verbose=4 "$code" 2>&1)

	grep -q "^Authority=Developer ID Application: " <<<"$details" || fail "$code is not signed with Developer ID Application"
	grep -q "^TeamIdentifier=$team$" <<<"$details" || fail "$code is not signed by team $team"
	grep -q "^Timestamp=" <<<"$details" || fail "$code has no secure timestamp"
	grep -Eq "^CodeDirectory .*flags=[^ ]*[(,]runtime[,)]" <<<"$details" || fail "$code does not have the hardened runtime"
	if entitlements "$code" | grep -q "<key>com.apple.security.get-task-allow</key>"; then
		fail "$code has the get-task-allow entitlement"
	fi
}

check_app_group() {
	local code="$1" info_group
	info_group=$(plist_value "$code/Contents/Info.plist" SynctrainAppGroup)
	[[ "$info_group" == "$team".* ]] || fail "$code uses app group $info_group, without team $team as its prefix"
	[[ "$info_group" == "$app_group" ]] || fail "$code uses app group $info_group, and the app uses $app_group"
	entitlements "$code" | grep -q "<string>$app_group</string>" || fail "$code is not entitled to app group $app_group"
}

# Keychain access groups are restricted: macOS won't start code that claims one without a profile that allows it
check_profile() {
	local code="$1" claimed bundle_id
	claimed=$(entitlements "$code")
	grep -q "<key>keychain-access-groups</key>" <<<"$claimed" || return 0
	[[ -f "$code/Contents/embedded.provisionprofile" ]] || fail "$code has no provisioning profile, and would not start"
	bundle_id=$(plist_value "$code/Contents/Info.plist" CFBundleIdentifier)
	grep -q "<string>$team.$bundle_id</string>" <<<"$claimed" || fail "$code is not entitled to its application identifier, $team.$bundle_id"
}

check_app() {
	local code extensions=0
	codesign --verify --deep --strict --verbose=2 "$app"

	team=$(codesign --display --verbose=2 "$app" 2>&1 | sed -n 's/^TeamIdentifier=//p')
	[[ -n "$team" && "$team" != "not set" ]] || fail "$app has no team"
	app_group=$(plist_value "$app/Contents/Info.plist" SynctrainAppGroup)

	check_signature "$app"
	check_app_group "$app"
	check_profile "$app"
	if entitlements "$app" | grep -q "<key>com.apple.security.app-sandbox</key>"; then
		fail "$app is sandboxed"
	fi

	while IFS= read -r -d '' code; do
		check_signature "$code"
		if [[ "$code" == *.appex ]]; then
			extensions=$((extensions + 1))
			check_app_group "$code"
			check_profile "$code"
			if ! entitlements "$code" | grep -q "<key>com.apple.security.app-sandbox</key>"; then
				fail "$code is not sandboxed, which macOS requires of extensions"
			fi
		fi
	done < <(find "$app/Contents" \( -name '*.app' -o -name '*.appex' -o -name '*.xpc' -o -name '*.framework' -o -name '*.dylib' \) -print0)

	[[ $extensions -eq 3 ]] || fail "$app has $extensions extensions, not the share extension and the two of the File Provider"
}

# Waits for the notary service's verdict. notarytool returns on any verdict and doesn't document its exit codes, so this
# reads the verdict itself, and shows the service's log when there is one.
notarize() {
	local file="$1" result status id
	result=$(mktemp)

	echo "Notarizing $(basename "$file")..."
	xcrun notarytool submit "$file" "${notary_credentials[@]}" --wait --timeout 2h --output-format plist >"$result" || true

	status=$(plist_value "$result" status 2>/dev/null || true)
	if [[ "$status" != "Accepted" ]]; then
		cat "$result" >&2
		if id=$(plist_value "$result" id 2>/dev/null); then
			xcrun notarytool log "$id" "${notary_credentials[@]}" >&2 || true
		fi
		fail "the notary service did not accept $file (status: ${status:-none})"
	fi
	rm -f "$result"
}

mkdir -p "$output"
app="$output/Synctrain.app"
rm -rf "$app"
# Stapling changes the app, and the archive stays as it was built
ditto "$archived_app" "$app"

check_app

submission=$(mktemp -d)
ditto -c -k --keepParent "$app" "$submission/Synctrain.zip"
notarize "$submission/Synctrain.zip"
rm -rf "$submission"
xcrun stapler staple "$app"

version=$(plist_value "$app/Contents/Info.plist" CFBundleShortVersionString)
build=$(plist_value "$app/Contents/Info.plist" CFBundleVersion)
disk_image="$output/Synctrain-$version.$build.dmg"

contents=$(mktemp -d)
ditto "$app" "$contents/Synctrain.app"
ln -s /Applications "$contents/Applications"
hdiutil create -volname "Synctrain" -srcfolder "$contents" -format ULFO -ov "$disk_image"
rm -rf "$contents"

identity=$(codesign --display --verbose=2 "$app" 2>&1 | sed -n 's/^Authority=//p' | head -n 1)
codesign --sign "$identity" --timestamp "$disk_image"
notarize "$disk_image"
xcrun stapler staple "$disk_image"

xcrun stapler validate "$app"
xcrun stapler validate "$disk_image"
spctl --assess --verbose=2 --type execute "$app"
spctl --assess --verbose=2 --type open --context context:primary-signature "$disk_image"

shasum -a 256 "$disk_image"
echo "Ready to distribute: $disk_image"
