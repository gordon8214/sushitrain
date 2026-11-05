// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package desktopapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"t-shaped.nl/sushitrain/v2/adminsock"
)

func TestRelative(t *testing.T) {
	for _, tc := range []struct {
		root, path, want string
		allowed          bool
	}{
		{"/data/sync", "/data/sync", "", true},
		{"/data/sync", "/data/sync/space #/café.txt", "space #/café.txt", true},
		{"/data/sync", "/data/sync-other/a", "", false},
		{"/data/sync", "/data/sync/../private", "", false},
		{"/data/sync", "file:///data/sync/a", "", false},
		{"/data/sync", "a", "", false},
	} {
		got, err := Relative(tc.root, tc.path)
		if (err == nil) != tc.allowed || got != tc.want {
			t.Errorf("Relative(%q, %q) = %q, %v", tc.root, tc.path, got, err)
		}
	}
}
func TestItemBatchBoundsAndCancellation(t *testing.T) {
	a := adminsock.NewServer()
	calls := 0
	RegisterItems(a, func(context.Context, string) (ItemStatus, error) { calls++; return ItemStatus{State: "remote"}, nil })
	paths := make([]string, 257)
	for i := range paths {
		paths[i] = "/data/a"
	}
	b, _ := json.Marshal(map[string]any{"Paths": paths})
	if _, err := a.Invoke(context.Background(), "desktop.items", b); err == nil || calls != 0 {
		t.Fatal("oversize batch was processed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Invoke(ctx, "desktop.items", []byte(`{"Paths":["/data/a"]}`)); err == nil {
		t.Fatal("canceled batch was processed")
	}
	result, err := a.Invoke(context.Background(), "desktop.items", []byte(`{"Paths":["/data/a","/data/b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	items := result.([]ItemStatus)
	if len(items) != 2 || !strings.HasSuffix(items[1].Path, "b") {
		t.Fatal(items)
	}
}
