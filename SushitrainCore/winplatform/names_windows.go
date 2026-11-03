// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package winplatform

import "strings"

// ValidComponent restricts destinations to names that both NT and Win32 applications can address consistently.
func ValidComponent(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	if strings.ContainsAny(name, `<>:"/\|?*`) {
		return false
	}
	for _, c := range name {
		if c < 32 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT")
		if len([]rune(suffix)) == 1 && strings.ContainsAny(suffix, "123456789¹²³") {
			return false
		}
	}
	return true
}
