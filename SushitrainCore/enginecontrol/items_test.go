// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	core "t-shaped.nl/sushitrain/v2/src"
)

func TestIndexedStateRequiresMatchingIndexAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	base := map[string]any{"version": "{A:2}", "size": info.Size(), "modifiedNs": info.ModTime().UnixNano()}
	for _, tc := range []struct {
		name          string
		local, global map[string]any
		want          string
	}{
		{"missing index", nil, base, "unknown"},
		{"matching", base, base, "synced"},
		{"remote changed", base, map[string]any{"version": "{A:3}", "size": 5, "modifiedNs": info.ModTime().UnixNano()}, "syncing"},
		{"disk changed", map[string]any{"version": "{A:2}", "size": 4}, map[string]any{"version": "{A:2}", "size": 4}, "scanning"},
		{"invalid", map[string]any{"invalid": true}, base, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"local": tc.local, "global": tc.global})
			if got := indexedState(data, info); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDesktopItemsIgnoredDirectoryOutsideIndex(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	ignored := filepath.Join(data, "ignored")
	if err := os.Mkdir(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, ".stignore"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := core.NewClient(home, t.TempDir(), t.TempDir(), true, false)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := client.Load(false); err != nil {
		t.Fatal(err)
	}
	for _, configure := range []func() error{
		func() error { return client.SetLocalAnnounceEnabled(false) },
		func() error { return client.SetGlobalAnnounceEnabled(false) },
		func() error { return client.SetRelaysEnabled(false) },
		func() error { return client.SetNATEnabled(false) },
		func() error { return client.SetSTUNEnabled(false) },
		func() error { return client.SetGUIEnabled(false) },
		func() error { return client.SetListenAddresses(core.List([]string{"tcp://127.0.0.1:0"})) },
	} {
		if err := configure(); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for {
		err := client.AddFolder("desktop-items", data, false)
		if err == nil {
			break
		}
		if err != core.ErrStillLoading {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	folder := client.FolderWithID("desktop-items")
	for folder.State() != "idle" {
		select {
		case <-ctx.Done():
			t.Fatalf("folder never became idle: %s (%v)", folder.State(), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if entry, err := folder.GetFileInformation("ignored"); err != nil || entry != nil {
		t.Fatal("ignored directory unexpectedly indexed", entry, err)
	}
	c := New(client, nil)
	defer c.Close()
	a := adminsock.NewServer()
	defer a.Close()
	c.Register(a)
	params, _ := json.Marshal(map[string]any{"Paths": []string{ignored}})
	result, err := a.Invoke(ctx, "desktop.items", params)
	if err != nil {
		t.Fatal(err)
	}
	items := result.([]desktopapi.ItemStatus)
	if len(items) != 1 || items[0].Error != "" || items[0].State != "ignored" || !items[0].Directory {
		t.Fatalf("ignored directory status: %+v", items)
	}

	t.Run("checked delete preserves a replacement", func(t *testing.T) {
		name := "delete-fixture.txt"
		path := filepath.Join(data, name)
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := folder.RescanSubdirectory(""); err != nil {
			t.Fatal(err)
		}
		var entry *core.Entry
		for entry == nil {
			entry, _ = folder.GetFileInformation(name)
			if entry != nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("fixture did not enter the index", ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		token := entry.DesktopIdentity()
		if token == "" {
			t.Fatal("missing fixture identity")
		}
		replacement := []byte("replacement with a different size")
		if err := os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(ctx, "engine.action", Request{Folder: "desktop-items", Path: name, Action: "deleteFile", Token: token}); err == nil {
			t.Fatal("stale delete accepted")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(replacement) {
			t.Fatal("replacement was changed", err)
		}
		current := entry.DesktopIdentity()
		if _, err := c.Call(ctx, "engine.action", Request{Folder: "desktop-items", Path: name, Action: "deleteFile", Token: current}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("confirmed file was not deleted", err)
		}
	})
}
