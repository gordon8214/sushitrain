// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyIsOffUntilSaved(t *testing.T) {
	file, err := loadProxyFile(filepath.Join(t.TempDir(), proxyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if file.Enabled || file.URL != "" {
		t.Fatalf("settings %+v", file)
	}
	for _, entry := range []string{"localhost", "*.local", "127.0.0.0/8", "::1", "192.168.0.0/16", "fe80::/10"} {
		if !slices.Contains(file.Bypass, entry) {
			t.Errorf("%s isn't direct by default", entry)
		}
	}
	if proxy, err := file.proxy(); proxy != nil || err != nil {
		t.Fatalf("proxy %v, error %v", proxy, err)
	}
}

func TestProxySettingsKeepWhatWasEntered(t *testing.T) {
	file, err := newProxyFile(&ProxySettings{
		Enabled: true, URL: " socks5://proxy.example.com:1080\n", User: "alice", Password: "se/cr#t?", Bypass: " localhost \n\n10.0.0.0/8\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), proxyFileName)
	if err := writeFileSynced(path, data); err != nil {
		t.Fatal(err)
	}
	read, err := loadProxyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := ProxySettings{Enabled: true, URL: "socks5://proxy.example.com:1080", User: "alice", Password: "se/cr#t?",
		Bypass: "localhost\n10.0.0.0/8"}
	if !read.sameProxy(file) || !read.settings().Equal(&expected) {
		t.Fatalf("saved %+v, read %+v", file, read)
	}
	// A password with characters a URL can't hold still makes a proxy
	if proxy, err := read.proxy(); proxy == nil || err != nil {
		t.Fatalf("proxy %v, error %v", proxy, err)
	}
}

func TestProxySettingsRefuseWhatCantBeUsed(t *testing.T) {
	for name, settings := range map[string]ProxySettings{
		"no address":          {Enabled: true},
		"no kind":             {Enabled: true, URL: "127.0.0.1:1080"},
		"no port":             {Enabled: true, URL: "socks5://127.0.0.1"},
		"a user in the URL":   {Enabled: true, URL: "socks5://user:secret@127.0.0.1:1080"},
		"no user":             {Enabled: true, URL: "socks5://127.0.0.1:1080", Password: "secret"},
		"another kind":        {Enabled: true, URL: "ftp://127.0.0.1:21"},
		"a bad address, off":  {URL: "127.0.0.1:1080"},
		"a bad network":       {Enabled: true, URL: "socks5://127.0.0.1:1080", Bypass: "10.0.0.0/33"},
		"a bad network, off":  {URL: "socks5://127.0.0.1:1080", Bypass: "10.0.0.0/33"},
		"something else":      {Enabled: true, URL: "socks5://127.0.0.1:1080", Bypass: "two words"},
		"everything directly": {Enabled: true, URL: "socks5://127.0.0.1:1080", Bypass: "*"},
	} {
		if _, err := newProxyFile(&settings); err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
	for name, settings := range map[string]ProxySettings{
		"off, and nothing entered": {},
		"off, with an address":     {URL: "http://proxy.example.com:8080"},
		"nothing direct":           {Enabled: true, URL: "https://proxy.example.com:8443"},
	} {
		if _, err := newProxyFile(&settings); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProxyFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"damaged":     `{"enabled":true,`,
		"empty":       ``,
		"null":        `null`,
		"nothing":     `{}`,
		"unusable":    `{"enabled":true,"url":"nothing"}`,
		"no address":  `{"enabled":true}`,
		"bad network": `{"enabled":true,"url":"socks5://127.0.0.1:1080","bypass":["10.0.0.0/33"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			clt, dir := newLoadTestClient(t)
			testIdentity(t, dir)
			if err := os.WriteFile(filepath.Join(dir, proxyFileName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := clt.Load(false); err == nil || !strings.Contains(err.Error(), proxyFileName) {
				t.Fatalf("error %v", err)
			}
			if clt.currentRun() != nil {
				t.Fatal("the engine loaded")
			}
			if clt.StartupProblem() != StartupProblemDamagedProxySettings {
				t.Fatalf("problem %q", clt.StartupProblem())
			}

			// Only the user's choice gets past this, and it leaves the file there for them
			if err := clt.SetProxySettingsAside(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(clt.ProxySettingsFilePath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the file stayed: %v", err)
			}
			aside, err := filepath.Glob(clt.ProxySettingsFilePath() + ".broken-*")
			if err != nil || len(aside) != 1 {
				t.Fatalf("set aside: %v, %v", aside, err)
			}
		})
	}
}

func TestProxySettingsAreOnlySetAsideWhenDamaged(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	if err := os.WriteFile(filepath.Join(dir, proxyFileName), []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clt.SetProxySettingsAside(); err == nil {
		t.Fatal("working settings were set aside")
	}
}

func TestProxyStaysWithAMovedConfiguration(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	other := t.TempDir()
	if err := clt.CopyProxySettings(other); err != nil {
		t.Fatalf("nothing to copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, proxyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copied nothing into a file: %v", err)
	}

	source := filepath.Join(dir, proxyFileName)
	if err := os.WriteFile(source, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clt.CopyProxySettings(other); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(other, proxyFileName)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != `{"enabled":true}` {
		t.Fatalf("copied %q, error %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, error %v", info.Mode(), err)
	}

	// A folder with settings of its own keeps them
	if err := os.WriteFile(source, []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clt.CopyProxySettings(other); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != `{"enabled":true}` {
		t.Fatalf("replaced with %q, error %v", data, err)
	}
}

func TestSettingsSayWhenAPasswordIsSentUnencrypted(t *testing.T) {
	for address, expected := range map[string]bool{
		"socks5://proxy.example.com:1080": true,
		"http://proxy.example.com:8080":   true,
		"https://proxy.example.com:8443":  false,
		"socks5://127.0.0.1:1080":         false,
		"socks5://[::1]:1080":             false,
		"http://localhost:8080":           false,
	} {
		settings := ProxySettings{URL: address, User: "alice", Password: "secret"}
		if settings.SendsPasswordUnencrypted() != expected {
			t.Errorf("%s: %v", address, !expected)
		}
		settings.Password = ""
		if settings.SendsPasswordUnencrypted() {
			t.Errorf("%s without a password", address)
		}
	}
}

func TestOnlyWhatTheEngineUsesAsksForARestart(t *testing.T) {
	off := proxyFile{URL: "socks5://127.0.0.1:1080", Bypass: []string{"localhost"}}
	otherOff := proxyFile{URL: "http://proxy.example.com:8080"}
	on := off
	on.Enabled = true
	if !off.sameProxy(&otherOff) {
		t.Error("two ways of having no proxy differ")
	}
	if on.sameProxy(&off) || off.sameProxy(&on) {
		t.Error("turning it on is no change")
	}
	for _, change := range []func(*proxyFile){
		func(f *proxyFile) { f.URL = "socks5://127.0.0.1:1081" },
		func(f *proxyFile) { f.User = "alice" },
		func(f *proxyFile) { f.Password = "secret" },
		func(f *proxyFile) { f.Bypass = nil },
	} {
		changed := on
		change(&changed)
		if on.sameProxy(&changed) {
			t.Errorf("no change: %+v", changed)
		}
	}
}

type eventCounter struct {
	events atomic.Int32
}

func (c *eventCounter) OnEvent(string)                            { c.events.Add(1) }
func (c *eventCounter) OnFolderScanFinished(string)               {}
func (c *eventCounter) OnDeviceDiscovered(string, *ListOfStrings) {}
func (c *eventCounter) OnListenAddressesChanged(*ListOfStrings)   {}
func (c *eventCounter) OnChange(*Change)                          {}
func (c *eventCounter) OnMeasurementsUpdated()                    {}
func (c *eventCounter) OnPendingDevicesChanged()                  {}
func (c *eventCounter) OnEngineStateChanged(string, string)       {}

func TestSavingTheProxyWritesAPrivateFileAndAnnouncesIt(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	if err := clt.SetProxySettings(&ProxySettings{}); !errors.Is(err, ErrStillLoading) {
		t.Fatalf("before Load: %v", err)
	}
	if clt.ProxySettings() != nil {
		t.Fatal("settings before Load")
	}
	file, err := loadProxyFile(proxyFilePath())
	if err != nil {
		t.Fatal(err)
	}
	clt.proxy.Store(file)
	delegate := &eventCounter{}
	clt.Delegate = delegate

	settings := &ProxySettings{Enabled: true, URL: "socks5://127.0.0.1:1080", User: "alice", Password: "secret", Bypass: "localhost"}
	if err := clt.SetProxySettings(settings); err != nil {
		t.Fatal(err)
	}
	if delegate.events.Load() != 1 {
		t.Fatalf("%d events", delegate.events.Load())
	}
	path := filepath.Join(dir, proxyFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if read, err := loadProxyFile(path); err != nil || !read.settings().Equal(settings) {
		t.Fatalf("read %+v, error %v", read, err)
	}
	if !clt.ProxySettings().Equal(settings) {
		t.Fatalf("settings %+v", clt.ProxySettings())
	}
	if leftovers, err := filepath.Glob(path + ".tmp*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("left behind: %v, %v", leftovers, err)
	}

	if err := clt.SetProxySettings(&ProxySettings{Enabled: true}); err == nil {
		t.Fatal("no address was accepted")
	}
	if !clt.ProxySettings().Equal(settings) || delegate.events.Load() != 1 {
		t.Fatalf("a refused save changed the settings: %+v, %d events", clt.ProxySettings(), delegate.events.Load())
	}
}
