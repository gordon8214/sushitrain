// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package desktopsettings

import (
	"path/filepath"
	"testing"
)

func TestConcurrentDesktopEditsPreserveUntouchedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.json")
	baseline, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Update(t.Context(), path, baseline, map[string]string{"Grid": "true"}); err != nil {
		t.Fatal(err)
	}
	result, err := Update(t.Context(), path, baseline, map[string]string{"ImageLimitMiB": "32"})
	if err != nil || !result.Grid || result.ImageLimitMiB != 32 {
		t.Fatal(result, err)
	}
	if _, err = Update(t.Context(), path, baseline, map[string]string{"Grid": "false"}); err == nil {
		t.Fatal("conflicting edit overwritten")
	}
	if _, err = Update(t.Context(), path, result, map[string]string{"ImageLimitMiB": "0"}); err == nil {
		t.Fatal("invalid image budget accepted")
	}
}
