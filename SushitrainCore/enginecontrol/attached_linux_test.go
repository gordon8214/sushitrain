// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"crypto/tls"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/locations"
	"os"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	core "t-shaped.nl/sushitrain/v2/src"
	"testing"
)

func TestAttachedControlWithoutGUIOrGateway(t *testing.T) {
	engine := gatewaytest.NewEngine(t, "native", nil)
	oldConfig, oldData := locations.GetBaseDir(locations.ConfigBaseDir), locations.GetBaseDir(locations.DataBaseDir)
	locations.SetBaseDir(locations.ConfigBaseDir, engine.Dir)
	locations.SetBaseDir(locations.DataBaseDir, engine.Dir)
	defer locations.SetBaseDir(locations.ConfigBaseDir, oldConfig)
	defer locations.SetBaseDir(locations.DataBaseDir, oldData)
	cert, err := tls.LoadX509KeyPair(filepath.Join(engine.Dir, "cert.pem"), filepath.Join(engine.Dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client, detach, err := core.AttachDesktopEngine(t.Context(), engine.App, engine.Config, engine.Events, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	control := New(client, func() error { return nil })
	result, err := control.Call(t.Context(), "engine.folders", Request{})
	if err != nil || len(result.([]Folder)) != 1 {
		t.Fatalf("folders: %#v %v", result, err)
	}
	editor, err := control.getSettings("basic/folder/native")
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := engine.Config.Modify(func(c *config.Configuration) { f, _, _ := c.Folder("native"); f.MaxConflicts = 12; c.SetFolder(f) })
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	_, err = control.saveSettings(Request{Token: editor.Token, Values: map[string]string{"label": "Native Test"}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := engine.Config.Folder("native")
	if f.Label != "Native Test" || f.MaxConflicts != 12 {
		t.Fatalf("settings overwrote another writer: %#v", f)
	}
	devices, err := control.Call(t.Context(), "engine.devices", Request{})
	if err != nil || len(devices.([]Device)) != 0 {
		t.Fatal("local device appeared as remote peer", devices, err)
	}
	for _, bad := range []string{"", "invalid"} {
		if _, err = control.Call(t.Context(), "engine.pendingFolders", Request{Device: bad}); err == nil {
			t.Fatal("invalid device accepted")
		}
	}
	engine.Write("empty.txt", "")
	engine.Write("original.txt", "selected")
	engine.Scan()
	page, err := control.Call(t.Context(), "engine.browsePage", Request{Folder: "native"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := page.(map[string]any); !ok {
		t.Fatal("paged browser returned the wrong shape", page)
	}
	output := filepath.Join(t.TempDir(), "empty-copy")
	if _, err = control.Call(t.Context(), "engine.exportCurrent", Request{Folder: "native", Path: "empty.txt", Name: output}); err != nil {
		t.Fatal(err)
	}
	if info, e := os.Stat(output); e != nil || info.Size() != 0 {
		t.Fatal("empty export failed", e)
	}
	source := filepath.Join(t.TempDir(), "import.txt")
	os.WriteFile(source, []byte("imported"), 0600)
	if _, err = control.Call(t.Context(), "engine.action", Request{Action: "import", Folder: "native", Path: "imported.txt", ID: source}); err != nil {
		t.Fatal(err)
	}
	current, err := client.FolderWithID("native").GetFileInformation("original.txt")
	if err != nil {
		t.Fatal(err)
	}
	identity := current.DesktopIdentity()
	if _, err = control.Call(t.Context(), "engine.action", Request{Action: "rename", Folder: "native", Path: "original.txt", Name: "imported.txt", Token: identity}); err == nil {
		t.Fatal("rename replaced existing content")
	}
	os.WriteFile(engine.Path("original.txt"), []byte("intervening edit"), 0600)
	if _, err = control.Call(t.Context(), "engine.action", Request{Action: "rename", Folder: "native", Path: "original.txt", Name: "renamed.txt", Token: identity}); err == nil {
		t.Fatal("stale identity moved an edited file")
	}
	staging := ".syncthing.import-interrupted.tmp"
	os.WriteFile(engine.Path(staging), []byte("partial"), 0600)
	engine.Scan()
	entries, err := control.Call(t.Context(), "engine.browse", Request{Folder: "native"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries.([]Entry) {
		if strings.Contains(entry.Name, "interrupted") {
			t.Fatal("interrupted import became synchronized", entry)
		}
	}
	for _, method := range []string{"engine.browse", "engine.browsePage", "engine.thumbnail", "engine.share", "engine.versions", "engine.exportCurrent", "engine.captureRestore", "engine.restoreDownloaded", "engine.action"} {
		for _, folder := range []string{"missing", ""} {
			if _, err = control.Call(t.Context(), method, Request{Folder: folder, Action: "mkdir", Path: "file"}); err == nil {
				t.Errorf("%s accepted unknown folder %q", method, folder)
			}
		}
	}
	for _, section := range []string{"folder/missing", "versioning/missing", "basic/folder/missing"} {
		if _, err = control.Call(t.Context(), "settings.get", Request{Section: section}); err == nil {
			t.Errorf("accepted unknown settings section %s", section)
		}
	}
	if _, err = control.Call(t.Context(), "engine.addFolder", Request{ID: "new-native", Path: filepath.Join(t.TempDir(), "new-folder"), Name: "Added Natively"}); err != nil {
		t.Fatal("adding an unconfigured folder:", err)
	}
	if added := client.FolderWithID("new-native"); added == nil || added.Label() != "Added Natively" {
		t.Fatal("new native folder was not configured", added)
	}
	guiEditor, err := control.getSettings("gui")
	if err != nil {
		t.Fatal(err)
	}
	waiter, err = engine.Config.Modify(func(c *config.Configuration) {
		c.GUI.RawAddress = "127.0.0.1:8385"
		c.GUI.User = "another editor"
		c.GUI.Password = "preserved hash"
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	if _, err = control.saveSettings(Request{Token: guiEditor.Token, Values: map[string]string{"Theme": "dark", "Password": ""}}); err != nil {
		t.Fatal(err)
	}
	gui := engine.Config.GUI()
	if gui.Theme != "dark" || gui.RawAddress != "127.0.0.1:8385" || gui.User != "another editor" || gui.Password != "preserved hash" {
		t.Fatal("GUI draft overwrote another editor", gui)
	}
	detach()
	if _, _, err := engine.App.Internals.FolderState("native"); err != nil {
		t.Fatalf("detach stopped the host's engine: %v", err)
	}
	_ = context.Background()
}
