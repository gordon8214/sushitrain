// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/protocol"
)

// Names that only the proxy gets to see: nothing resolves them, so they are no use to a direct connection
const (
	proxiedPeer      = "peer.invalid:22000"
	proxiedRelay     = "relay.invalid:22067"
	proxiedDiscovery = "discovery.invalid:443"
)

// socksRecorder is a SOCKS5 proxy that notes where it is asked to go, and then refuses
type socksRecorder struct {
	listener net.Listener
	mutex    sync.Mutex
	targets  []string
}

func newSOCKSRecorder(t *testing.T) *socksRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	s := &socksRecorder{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *socksRecorder) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	// A request for a name: version, command, reserved, the type of address, and the length of the name
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil || header[3] != 3 {
		return
	}
	name := make([]byte, header[4])
	if _, err := io.ReadFull(conn, name); err != nil {
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	s.mutex.Lock()
	s.targets = append(s.targets, fmt.Sprintf("%s:%d", name, binary.BigEndian.Uint16(port)))
	s.mutex.Unlock()
	_, _ = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
}

func (s *socksRecorder) seen() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Clone(s.targets)
}

func (s *socksRecorder) waitFor(t *testing.T, targets ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		seen := s.seen()
		if !slices.ContainsFunc(targets, func(target string) bool { return !slices.Contains(seen, target) }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy saw %v, but not all of %v", seen, targets)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// useDevicesBehindTheProxy adds what the engine has to reach: a peer, a relay and a discovery server
func useDevicesBehindTheProxy(cfg *config.Configuration) {
	cfg.Options.GlobalAnnEnabled = true
	cfg.Options.RawGlobalAnnServers = []string{"https://" + proxiedDiscovery + "/"}
	cfg.Options.RelaysEnabled = true
	cfg.Options.RawListenAddresses = []string{"tcp://127.0.0.1:0", "relay://" + proxiedRelay}
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{
		DeviceID:  protocol.DeviceID{1, 2, 3},
		Addresses: []string{"tcp://" + proxiedPeer, "dynamic"},
	})
}

func TestEngineTurnsTheProxyOnAndOffWithARestart(t *testing.T) {
	proxy := newSOCKSRecorder(t)
	t.Cleanup(func() { dialer.SetProxy(nil) })
	clt, recorder, _ := startTestEngineWith(t, useDevicesBehindTheProxy)
	if dialer.Proxied("example.com") || clt.IsEngineRestartRequired() {
		t.Fatalf("proxied %v, restart required %v", dialer.Proxied("example.com"), clt.IsEngineRestartRequired())
	}

	settings := clt.ProxySettings()
	settings.Enabled = true
	settings.URL = "socks5://" + proxy.listener.Addr().String()
	if err := clt.SetProxySettings(settings); err != nil {
		t.Fatal(err)
	}
	if !clt.IsEngineRestartRequired() || dialer.Proxied("example.com") {
		t.Fatalf("after saving: restart required %v, proxied %v", clt.IsEngineRestartRequired(), dialer.Proxied("example.com"))
	}

	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	recorder.waitFor(t, EngineStateRunning)
	if clt.IsEngineRestartRequired() || !dialer.Proxied("example.com") || dialer.Proxied("192.168.1.2") {
		t.Fatalf("after restarting: restart required %v, proxied %v, the local network %v", clt.IsEngineRestartRequired(),
			dialer.Proxied("example.com"), dialer.Proxied("192.168.1.2"))
	}
	proxy.waitFor(t, proxiedPeer, proxiedRelay, proxiedDiscovery)

	settings.Enabled = false
	if err := clt.SetProxySettings(settings); err != nil {
		t.Fatal(err)
	}
	if !clt.IsEngineRestartRequired() {
		t.Fatal("no restart required to turn it off")
	}
	if err := clt.RestartEngine(); err != nil {
		t.Fatal(err)
	}
	recorder.waitFor(t, EngineStateRunning)
	if clt.IsEngineRestartRequired() || dialer.Proxied("example.com") {
		t.Fatalf("turned off: restart required %v, proxied %v", clt.IsEngineRestartRequired(), dialer.Proxied("example.com"))
	}
	before := len(proxy.seen())
	time.Sleep(2 * time.Second)
	if seen := proxy.seen(); len(seen) != before {
		t.Fatalf("the proxy was used after it was turned off: %v", seen[before:])
	}
}

func TestEngineHasTheProxyBeforeItStarts(t *testing.T) {
	proxy := newSOCKSRecorder(t)
	t.Cleanup(func() { dialer.SetProxy(nil) })
	clt, dir := newLoadTestClient(t)
	testIdentity(t, dir)
	data, err := json.Marshal(proxyFile{Enabled: true, URL: "socks5://" + proxy.listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyFilePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clt.Load(false); err != nil {
		t.Fatal(err)
	}
	if !dialer.Proxied("example.com") {
		t.Fatal("loaded without the proxy, while loading can already send a crash report")
	}
}

func TestEngineUsesTheProxyFromItsFirstStart(t *testing.T) {
	proxy := newSOCKSRecorder(t)
	t.Cleanup(func() { dialer.SetProxy(nil) })
	startTestEngineWith(t, func(cfg *config.Configuration) {
		useDevicesBehindTheProxy(cfg)
		data, err := json.Marshal(proxyFile{Enabled: true, URL: "socks5://" + proxy.listener.Addr().String()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proxyFilePath(), data, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if !dialer.Proxied("example.com") {
		t.Fatal("not proxied")
	}
	proxy.waitFor(t, proxiedPeer, proxiedRelay, proxiedDiscovery)
}
