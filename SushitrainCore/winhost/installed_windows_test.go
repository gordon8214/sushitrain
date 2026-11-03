//go:build windows && installedintegration

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

// Explicit installed-product acceptance test. It refuses to use a populated
// profile and pairs only its own temporary gateway and disposable test folder.
func TestInstalledCloudFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	sid, err := winplatform.UserSID()
	if err != nil {
		t.Fatal(err)
	}
	pipe := `\\.\pipe\Synctrain-` + sid
	call := func(method string, params, out any) {
		t.Helper()
		if err := adminsock.CallContext(ctx, pipe, method, params, out); err != nil {
			t.Fatal(method, err)
		}
	}
	var status struct{ Mode, Root string }
	call("status", nil, &status)
	if status.Mode != "files" {
		t.Fatal("acceptance requires Files mode")
	}
	var existing []files.Gateway
	call("files.gateways", nil, &existing)
	if len(existing) != 0 {
		t.Fatal("refusing a profile with existing gateways")
	}
	var folders []files.Folder
	call("files.folders", nil, &folders)
	if len(folders) != 0 {
		t.Fatal("refusing a profile with selected folders")
	}
	fixturePath := os.Getenv("SYNCTRAIN_GATEWAY_FIXTURE")
	var fixture struct{ Link, ShareRoot, Confirmed, Done string }
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(fixture.Done, nil, 0600) }()
	var paired files.Gateway
	call("files.pair", map[string]any{"Link": fixture.Link}, &paired)
	var folder files.Folder
	call("files.add", map[string]any{"ServiceID": paired.ServiceID, "Folder": "windows-acceptance"}, &folder)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := adminsock.CallContext(cleanup, pipe, "files.remove", map[string]any{"Item": folder.ID}, nil); err != nil {
			t.Error("remove acceptance folder:", err)
		}
		if err := adminsock.CallContext(cleanup, pipe, "files.forget", map[string]any{"ServiceID": paired.ServiceID}, nil); err != nil {
			t.Error("forget acceptance gateway:", err)
		}
	}()
	path := filepath.Join(status.Root, folder.DirName, "Acceptance.txt")
	var projected files.Entry
	gatewaytest.WaitFor(t, 10*time.Second, "installed indexed placeholder version", func() bool {
		if err := adminsock.CallContext(ctx, pipe, "files.resolve", map[string]any{"Path": folder.DirName + "/Acceptance.txt"}, &projected); err != nil {
			return false
		}
		var checkpoint []struct {
			Entry files.Entry
			Path  string
		}
		if err := adminsock.CallContext(ctx, pipe, "projection.load", nil, &checkpoint); err != nil {
			return false
		}
		for _, record := range checkpoint {
			if record.Entry.Item == projected.Item && record.Entry.Node == projected.Node && record.Path == folder.DirName+"/Acceptance.txt" {
				return true
			}
		}
		return false
	})
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "Installed Synctrain Cloud Files verification" {
		t.Fatalf("installed hydration: %q %v", data, err)
	}
	if err := os.WriteFile(path, []byte("Edited through the installed provider"), 0600); err != nil {
		t.Fatal(err)
	}
	gatewaytest.WaitFor(t, 10*time.Second, "installed edit reaching gateway", func() bool {
		_, err := os.Stat(fixture.Confirmed)
		return err == nil
	})
	output := os.Getenv("SYNCTRAIN_ACCEPTANCE_DIRECTORY")
	if os.Getenv("SYNCTRAIN_ACCEPTANCE_BACKGROUND") == "1" {
		var entry files.Entry
		call("files.resolve", map[string]any{"Path": folder.DirName + "/Acceptance.txt"}, &entry)
		gatewaytest.WaitFor(t, 8*time.Second, "background edit checkpoint settled", func() bool {
			if err := adminsock.CallContext(ctx, pipe, "files.entry", map[string]any{"Item": entry.Item}, &entry); err != nil {
				return false
			}
			var checkpoint []struct {
				Entry        files.Entry
				AwaitingSync bool
			}
			if err := adminsock.CallContext(ctx, pipe, "projection.load", nil, &checkpoint); err != nil {
				return false
			}
			for _, record := range checkpoint {
				if record.Entry.Item == entry.Item && record.Entry.Node == entry.Node && !record.AwaitingSync {
					return true
				}
			}
			return false
		})
		t.Log("Installed hydration and editing passed with the native window hidden")
		return
	}
	if output == "" {
		t.Fatal("SYNCTRAIN_ACCEPTANCE_DIRECTORY is required")
	}
	metadata, _ := json.Marshal(map[string]any{"Root": status.Root, "Folder": folder.DirName, "Image": filepath.Join(status.Root, folder.DirName, "Preview.png"), "ServiceID": paired.ServiceID})
	if err := os.WriteFile(filepath.Join(output, "installed-ready.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("Installed provider hydration and editing passed; ready for UI and packaged COM checks")
	gatewaytest.WaitFor(t, 90*time.Second, "installed UI acceptance completion", func() bool { _, err := os.Stat(filepath.Join(output, "installed-continue")); return err == nil })
}
