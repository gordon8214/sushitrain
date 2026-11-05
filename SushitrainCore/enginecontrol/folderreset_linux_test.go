// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"crypto/tls"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	core "t-shaped.nl/sushitrain/v2/src"
)

func TestFolderResetEditorLifecycle(t *testing.T) {
	engine := gatewaytest.NewEngine(t, "reset-native", nil)
	cert, err := tls.LoadX509KeyPair(filepath.Join(engine.Dir, "cert.pem"), filepath.Join(engine.Dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client, detach, err := core.AttachDesktopEngine(t.Context(), engine.App, engine.Config, engine.Events, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	control := New(client, nil)
	defer control.Close()
	waiter, err := engine.Config.Modify(func(c *config.Configuration) {
		folder, _, _ := c.Folder("reset-native")
		folder.Copiers = 7
		c.SetFolder(folder)
		c.Defaults.Folder.Copiers = 17
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	for _, section := range []string{"reset/folder/reset-native", "reset/defaults/folder"} {
		readCurrent := func() config.FolderConfiguration {
			if section == "reset/defaults/folder" {
				return engine.Config.DefaultFolder()
			}
			f, _ := engine.Config.Folder("reset-native")
			return f
		}
		original := readCurrent().Copiers
		editor, err := control.getSettings(section)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range editor.Fields {
			if !core.IsFolderSyncResetField(field.Path) {
				t.Fatalf("protected field in reset: %s", field.Path)
			}
			if field.Path == "Copiers" && field.Value == "17" {
				t.Fatal("used customized defaults")
			}
		}
		if readCurrent().Copiers != original {
			t.Fatal("opening preview saved it")
		}
		if _, err := control.Call(t.Context(), "settings.close", Request{Token: editor.Token}); err != nil {
			t.Fatal(err)
		}
		if readCurrent().Copiers != original {
			t.Fatal("Cancel changed settings")
		}
		if _, err := control.saveSettings(Request{Token: editor.Token}); err == nil {
			t.Fatal("closed token accepted")
		}
		editor, err = control.getSettings(section)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := control.saveSettings(Request{Token: editor.Token, Values: map[string]string{"Copiers": "invalid"}}); err == nil {
			t.Fatal("invalid reset saved")
		}
		if readCurrent().Copiers != original {
			t.Fatal("failed save changed configuration")
		}
		if _, err := control.saveSettings(Request{Token: editor.Token, Values: map[string]string{"Copiers": "4", "Path": "/must-not-change"}}); err != nil {
			t.Fatal(err)
		}
		if readCurrent().Copiers != 4 || readCurrent().Path == "/must-not-change" {
			t.Fatal("wrong reset save scope")
		}
	}
}
