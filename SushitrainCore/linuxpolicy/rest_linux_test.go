//go:build engineintegration

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package linuxpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/api"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

var _ api.DevicePauseController = (*Policy)(nil)

func TestRESTPauseIntent(t *testing.T) {
	dir := t.TempDir()
	oldConfig := locations.GetBaseDir(locations.ConfigBaseDir)
	oldData := locations.GetBaseDir(locations.DataBaseDir)
	if err := locations.SetBaseDir(locations.ConfigBaseDir, dir); err != nil {
		t.Fatal(err)
	}
	if err := locations.SetBaseDir(locations.DataBaseDir, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		locations.SetBaseDir(locations.ConfigBaseDir, oldConfig)
		locations.SetBaseDir(locations.DataBaseDir, oldData)
	})
	cert, err := syncthing.GenerateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	self := protocol.NewDeviceID(cert.Certificate[0])
	peers := []protocol.DeviceID{protocol.NewDeviceID([]byte("REST pause peer one")), protocol.NewDeviceID([]byte("REST pause peer two"))}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	conf := config.New(self)
	conf.GUI.Enabled = true
	conf.GUI.RawAddress = address
	conf.GUI.APIKey = "isolated-rest-pause-test"
	conf.Options.GlobalAnnEnabled = false
	conf.Options.LocalAnnEnabled = false
	conf.Options.RelaysEnabled = false
	conf.Options.NATEnabled = false
	conf.Options.CREnabled = false
	conf.Options.URAccepted = -1
	conf.Options.RawListenAddresses = []string{"tcp://127.0.0.1:0"}
	for _, peer := range peers {
		conf.SetDevice(config.DeviceConfiguration{DeviceID: peer, Addresses: []string{"tcp://127.0.0.1:1"}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := events.NewLogger()
	go logger.Serve(ctx)
	cfg := config.Wrap(filepath.Join(dir, "config.xml"), conf, self, logger)
	served := make(chan struct{})
	go func() { defer close(served); cfg.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	p, err := Open(cfg, filepath.Join(dir, "connectivity.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.settings.Metered = true
	p.environment = Environment{NetworkKnown: true, Metered: true}
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	database, err := syncthing.OpenDatabase(filepath.Join(dir, "index"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app, err := syncthing.New(p, database, logger, cert, syncthing.Options{})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err = app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Stop(svcutil.ExitSuccess); app.Wait() })
	client := &http.Client{Timeout: 3 * time.Second}
	request := func(method, path string, body []byte) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, "http://"+address+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", conf.GUI.APIKey)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s error=%v", method, path, response.StatusCode, data, err)
		}
		return data
	}
	setFence := func(enabled bool) {
		t.Helper()
		p.mu.Lock()
		p.environment.Metered = enabled
		p.mu.Unlock()
		if err := p.apply(); err != nil {
			t.Fatal(err)
		}
	}
	assertPaused := func(peer protocol.DeviceID, want bool) {
		t.Helper()
		device, ok := cfg.Device(peer)
		if !ok || device.Paused != want {
			t.Fatalf("device %s paused=%v, want %v", peer, device.Paused, want)
		}
	}
	// Echoing effective configuration must not create manual pause intent.
	data := request(http.MethodGet, "/rest/config", nil)
	var returned config.Configuration
	if err = json.Unmarshal(data, &returned); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/rest/config", data)
	setFence(false)
	for _, peer := range peers {
		assertPaused(peer, false)
	}
	// Explicit pause is meaningful even when the effective state is already paused.
	setFence(true)
	request(http.MethodPost, "/rest/system/pause?device="+peers[0].String(), nil)
	data = request(http.MethodGet, "/rest/config", nil)
	request(http.MethodPut, "/rest/config", data)
	setFence(false)
	assertPaused(peers[0], true)
	assertPaused(peers[1], false)
	reopened, err := Open(cfg, filepath.Join(dir, "connectivity.json"))
	if err != nil || !reopened.settings.Manual[peers[0].String()] {
		t.Fatal("REST pause was not persisted", err)
	}
	// Resume clears the manual choice while retaining the active automatic fence.
	setFence(true)
	request(http.MethodPost, "/rest/system/resume?device="+peers[0].String(), nil)
	assertPaused(peers[0], true)
	setFence(false)
	assertPaused(peers[0], false)
	// Bulk actions apply the same intent to every remote peer.
	setFence(true)
	request(http.MethodPost, "/rest/system/pause", nil)
	setFence(false)
	for _, peer := range peers {
		assertPaused(peer, true)
	}
	setFence(true)
	request(http.MethodPost, "/rest/system/resume", nil)
	for _, peer := range peers {
		assertPaused(peer, true)
	}
	setFence(false)
	for _, peer := range peers {
		assertPaused(peer, false)
	}
}
