// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"crypto/tls"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"t-shaped.nl/sushitrain/v2/desktopui"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	core "t-shaped.nl/sushitrain/v2/src"
)

func TestSettingsSearchEditorDestinations(t *testing.T) {
	engine := gatewaytest.NewEngine(t, "search-routes", nil)
	peerID := protocol.NewDeviceID([]byte("settings search peer"))
	waiter, err := engine.Config.Modify(func(c *config.Configuration) {
		c.SetDevice(config.DeviceConfiguration{DeviceID: peerID, Name: "Search peer"})
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
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
	data, err := client.SettingsSearchIndex()
	if err != nil {
		t.Fatal(err)
	}
	var entries []desktopui.SearchEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Action, "advanced/") {
			continue
		}
		if strings.HasPrefix(entry.Section, "device/") && entry.Section != "device/"+peerID.String() {
			continue
		}
		editor, err := control.getSettings(entry.Action)
		if err != nil {
			t.Errorf("%s: %v", entry.ID, err)
			continue
		}
		found := false
		for _, field := range editor.Fields {
			found = found || strings.EqualFold(field.Path, entry.Anchor) || strings.EqualFold(field.Path, entry.Field)
		}
		if !found {
			t.Errorf("%s routes to %s without control %s/%s", entry.ID, entry.Action, entry.Anchor, entry.Field)
		}
		if _, err := control.Call(t.Context(), "settings.close", Request{Token: editor.Token}); err != nil {
			t.Fatal(err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no mapped settings were checked")
	}
}
