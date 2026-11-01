// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/locations"
)

// Syncthing's configuration has no place for a proxy: it takes one from the environment, which it reads when the
// framework loads, before the app can set anything. So the app keeps its own file next to the configuration, and hands
// the proxy to the dialer each time the engine starts.

const proxyFileName = "proxy.json"

var (
	errProxyURLRequired      = errors.New("enter the address of the proxy, such as socks5://127.0.0.1:1080")
	errProxyCredentialsInURL = errors.New("enter the user and password in their own fields, not in the address")
	errProxyUserRequired     = errors.New("enter the user that the password is for")
)

// ProxySettings are the proxy for the engine's outgoing connections
type ProxySettings struct {
	Enabled bool
	// A socks5, socks5h, socks, http or https URL
	URL      string
	User     string
	Password string
	// One per line: addresses, networks such as 192.168.0.0/16, host names, and zones such as *.example.com
	Bypass string
}

// Equal reports whether both hold the same settings
func (s *ProxySettings) Equal(other *ProxySettings) bool {
	return *s == *other
}

// SendsPasswordUnencrypted reports whether the password leaves this device readable: only an https address encrypts it
func (s *ProxySettings) SendsPasswordUnencrypted() bool {
	address, err := url.Parse(strings.TrimSpace(s.URL))
	if s.Password == "" || err != nil || address.Scheme == "https" || address.Hostname() == "localhost" {
		return false
	}
	ip := net.ParseIP(address.Hostname())
	return ip == nil || !ip.IsLoopback()
}

// proxyFile is what is saved
type proxyFile struct {
	Enabled  bool     `json:"enabled"`
	URL      string   `json:"url"`
	User     string   `json:"user,omitempty"`
	Password string   `json:"password,omitempty"`
	Bypass   []string `json:"bypass"`
}

// newProxyFile checks settings. The address is checked while the proxy is off too, as it is kept for turning it on.
func newProxyFile(settings *ProxySettings) (*proxyFile, error) {
	file := &proxyFile{Enabled: settings.Enabled, URL: strings.TrimSpace(settings.URL), User: settings.User,
		Password: settings.Password, Bypass: advancedListItems(settings.Bypass, false)}
	if file.URL == "" {
		if file.Enabled {
			return nil, errProxyURLRequired
		}
		return file, nil
	}
	if address, err := url.Parse(file.URL); err == nil && address.User != nil {
		return nil, errProxyCredentialsInURL
	}
	if file.Password != "" && file.User == "" {
		return nil, errProxyUserRequired
	}
	if _, err := file.dialerProxy(); err != nil {
		return nil, err
	}
	return file, nil
}

// loadProxyFile reads what was saved. This fails closed: the engine doesn't start from a file it can't read or use,
// which could connect directly to everything while the user asked for a proxy.
func loadProxyFile(path string) (*proxyFile, error) {
	// What connects directly until the user says otherwise: this device, and what is on the local network
	file := &proxyFile{Bypass: []string{
		"localhost", "*.local", "127.0.0.0/8", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
		"fc00::/7", "fe80::/10",
	}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the proxy settings: %w", err)
	}
	// null and {} parse as well, and would read as turned off
	var fields struct {
		Enabled *bool `json:"enabled"`
	}
	err = json.Unmarshal(data, &fields)
	if err == nil && fields.Enabled == nil {
		err = errors.New("they don't say whether the proxy is on")
	}
	if err == nil {
		err = json.Unmarshal(data, file)
	}
	if err != nil {
		return nil, fmt.Errorf("the proxy settings in %s are damaged: %w", path, err)
	}
	if _, err := file.proxy(); err != nil {
		return nil, fmt.Errorf("the proxy settings in %s can't be used: %w", path, err)
	}
	return file, nil
}

func (f *proxyFile) settings() *ProxySettings {
	return &ProxySettings{Enabled: f.Enabled, URL: f.URL, User: f.User, Password: f.Password,
		Bypass: strings.Join(f.Bypass, "\n")}
}

// proxy is what the dialer takes for these settings: nil when they are turned off
func (f *proxyFile) proxy() (*dialer.Proxy, error) {
	if !f.Enabled {
		return nil, nil
	}
	return f.dialerProxy()
}

func (f *proxyFile) dialerProxy() (*dialer.Proxy, error) {
	address := f.URL
	if parsed, err := url.Parse(f.URL); err == nil && f.User != "" {
		// This encodes what a password can hold that a URL can't
		parsed.User = url.UserPassword(f.User, f.Password)
		address = parsed.String()
	}
	return dialer.NewProxy(address, f.Bypass)
}

// sameProxy reports whether the engine goes through the same proxy with both
func (f *proxyFile) sameProxy(other *proxyFile) bool {
	if !f.Enabled && !other.Enabled {
		return true
	}
	return f.Enabled == other.Enabled && f.URL == other.URL && f.User == other.User && f.Password == other.Password &&
		slices.Equal(f.Bypass, other.Bypass)
}

func proxyFilePath() string {
	return filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), proxyFileName)
}

// applyProxy hands the saved proxy to the dialer, for a run that hasn't connected to anything yet, and returns what it
// applied
func (clt *Client) applyProxy() (*proxyFile, error) {
	file := clt.proxy.Load()
	proxy, err := file.proxy()
	if err != nil {
		return nil, err
	}
	dialer.SetProxy(proxy)
	return file, nil
}

// ProxySettings are the saved settings, which the engine uses from when it next starts; nil before Load
func (clt *Client) ProxySettings() *ProxySettings {
	if file := clt.proxy.Load(); file != nil {
		return file.settings()
	}
	return nil
}

// SetProxySettings saves settings for when the engine next starts (IsEngineRestartRequired). When they can't be used,
// nothing changes and the error says why.
func (clt *Client) SetProxySettings(settings *ProxySettings) error {
	file, err := newProxyFile(settings)
	if err != nil {
		return err
	}
	if err := clt.saveProxyFile(file); err != nil {
		return err
	}
	if delegate := clt.Delegate; delegate != nil {
		// The offer to restart follows the engine's events, and the engine has none for this
		delegate.OnEvent("ProxySettingsChanged")
	}
	return nil
}

func (clt *Client) saveProxyFile(file *proxyFile) error {
	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()
	if clt.proxy.Load() == nil {
		return ErrStillLoading
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	// Only readable by the user, as this can hold a password
	if err := writeFileSynced(proxyFilePath(), data); err != nil {
		return err
	}
	clt.proxy.Store(file)
	return nil
}

// CopyProxySettings gives a configuration folder the app is about to switch to these settings, unless it has its own.
// Without them the engine would connect directly there, although only the folder changed.
func (clt *Client) CopyProxySettings(directory string) error {
	target := filepath.Join(directory, proxyFileName)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(proxyFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeFileSynced(target, data)
}

// ProxySettingsFilePath is where the settings are saved
func (clt *Client) ProxySettingsFilePath() string {
	return proxyFilePath()
}

// SetProxySettingsAside renames settings that kept the engine from starting (StartupProblemDamagedProxySettings), so
// that it starts without a proxy. The user has to ask for this, as it connects directly from then on.
func (clt *Client) SetProxySettingsAside() error {
	if clt.StartupProblem() != StartupProblemDamagedProxySettings {
		return errors.New("only damaged proxy settings can be set aside")
	}
	if err := clt.acquireLock(); err != nil {
		return err
	}
	path := proxyFilePath()
	asidePath := path + ".broken-" + time.Now().UTC().Format("20060102-150405")
	slog.Warn("Setting the proxy settings aside", "path", path, "newPath", asidePath)
	// As in SetConfigurationAside
	if err := os.Link(path, asidePath); err != nil {
		return err
	}
	return os.Remove(path)
}
