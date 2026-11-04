// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogSchemasAndRoutes(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Catalog() {
		if seen[tool.Name] || !strings.HasPrefix(tool.Name, "synctrain_") {
			t.Fatalf("invalid name %s", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Description == "" {
			t.Fatal(tool.Name)
		}
		if _, err := json.Marshal(tool.Schema()); err != nil {
			t.Fatal(err)
		}
		for _, r := range []Route{tool.Synchronize, tool.Files} {
			for arg := range r.Fields {
				if _, ok := tool.Arguments[arg]; !ok {
					t.Errorf("%s route uses an undeclared argument %s", tool.Name, arg)
				}
			}
		}
		if err := tool.Validate(map[string]any{"undeclared": true}); err == nil {
			t.Fatal("unknown argument accepted", tool.Name)
		}
	}
}
func TestArgumentValidation(t *testing.T) {
	tool, _ := Find("synctrain_file_read")
	for _, args := range []map[string]any{{"offset": -1.0}, {"offset": 1.5}, {"bytes": "10"}, {"limit": 501.0}, {"limit": 0.0}, {"offset": 1e20}} {
		if tool.Validate(args) == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
	if err := tool.Validate(map[string]any{"offset": 0.0, "bytes": 64.0, "limit": 100.0}); err != nil {
		t.Fatal(err)
	}
}

func TestLogContinuationPreservesNanoseconds(t *testing.T) {
	tool, _ := Find("synctrain_logs")
	var native struct{ Offset int64 }
	if err := json.Unmarshal(tool.Synchronize.Params(map[string]any{"after": "1793811023123456789"}), &native); err != nil {
		t.Fatal(err)
	}
	if native.Offset != 1793811023123456789 {
		t.Fatal(native.Offset)
	}
}

func TestSelectedVersionRequiresNonemptyValidator(t *testing.T) {
	tool, _ := Find("synctrain_version_restore")
	if err := tool.Validate(map[string]any{"folder": "test", "path": "file", "version": "version", "token": "validator"}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", " \n\t"} {
		if tool.Validate(map[string]any{"folder": "test", "path": "file", "version": "version", "token": token}) == nil {
			t.Fatal("empty validator accepted")
		}
	}
}
