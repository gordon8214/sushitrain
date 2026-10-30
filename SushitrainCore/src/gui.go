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
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/rand"
)

// On macOS the engine runs Syncthing's web GUI and REST API, as Syncthing.app does, on the address in the
// configuration (for a new configuration 127.0.0.1:8384, with a random API key and password). On iOS they stay off for
// the configurations the app makes: there is no web GUI to serve, and the app runs only as long as iOS lets it.

// The file next to the configuration that records that the app turned the web GUI on for it once (so that turning it
// off later lasts) and, as JSON, where the app moved it when its address was in use (so that it moves back once free)
const guiEnabledMarkerName = "synctrain-gui-enabled"

// The GUI user of configurations the app makes, whose password nobody knows: tools use the API key
const guiUser = "synctrain"

// The GUI's own address in a new configuration, as in Syncthing
const defaultGUIAddress = "127.0.0.1:8384"

// Beyond this many seconds, Syncthing's calculation of when a session ends overflows
const maxGUISessionS = math.MaxInt64 / int64(time.Second)

var (
	ErrGUINotAvailable         = errors.New("the web GUI and REST API are not available on iOS")
	ErrGUIAddressRequired      = errors.New("enter an address for the web GUI and REST API")
	ErrGUIPortRequired         = errors.New("enter a port other than 0 for the web GUI and REST API")
	ErrGUIUserRequired         = errors.New("enter a user name to go with the password")
	ErrGUIPasswordRequired     = errors.New("enter a password to go with the user name")
	ErrGUISessionTooLong       = errors.New("the session cookie lifetime is too long")
	ErrGUILDAPAddressRequired  = errors.New("enter the address of the LDAP server, as host:port")
	ErrGUILDAPBindDNRequired   = errors.New("enter the bind DN for the LDAP server")
	ErrGUILDAPSearchIncomplete = errors.New("enter both the search base DN and the search filter, or neither")
)

// What the marker records
type guiMarker struct {
	// The address the GUI is meant to have, while the app moved it to Moved
	Preferred string `json:"preferred,omitempty"`
	Moved     string `json:"moved,omitempty"`
}

// readGUIMarker reads the marker; ok is false when there is none (and only then)
func readGUIMarker(configDir string) (marker guiMarker, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(configDir, guiEnabledMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return guiMarker{}, false, nil
	} else if err != nil {
		return guiMarker{}, false, err
	}
	if len(data) > 0 {
		// An earlier or damaged marker still records that the GUI was turned on
		_ = json.Unmarshal(data, &marker)
	}
	return marker, true, nil
}

func writeGUIMarker(configDir string, marker guiMarker) {
	data := []byte{}
	if marker != (guiMarker{}) {
		data, _ = json.Marshal(marker)
	}
	if err := writeFileSynced(filepath.Join(configDir, guiEnabledMarkerName), data); err != nil {
		slog.Warn("could not record the web GUI's state", "cause", err)
	}
}

// guiIsLocalOnly reports whether only this computer can reach a GUI: on a unix socket, or on a loopback address with
// Syncthing's host check (which keeps websites from reaching it through DNS rebinding)
func guiIsLocalOnly(gui config.GUIConfiguration) bool {
	if gui.Network() == "unix" {
		return true
	}
	host, _, err := net.SplitHostPort(gui.Address())
	if err != nil || gui.InsecureSkipHostCheck {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protectGUI gives a GUI without a password a user and a random password nobody knows, so that only the API key opens
// it: other users of this computer could otherwise use it through its web pages' CSRF token
func protectGUI(gui *config.GUIConfiguration) {
	if gui.IsAuthEnabled() {
		return
	}
	gui.User = guiUser
	if err := gui.SetPassword(rand.String(32)); err != nil {
		slog.Warn("could not set the web GUI's password", "cause", err)
	}
}

// prepareGUI readies the web GUI of a configuration being loaded on macOS. It turns the GUI on once, where only this
// computer can reach it, with a password; keeps it on an address it can listen on, moving it to a free port when its
// address is in use (as by Syncthing.app still running) and back once that is free; and turns it off, rather than let
// the engine fail to start, when its address can't be used at all. It returns the marker to write once the
// configuration is saved (nil to leave it) and a notice for the user.
func prepareGUI(conf *config.Configuration, configDir string) (*guiMarker, string) {
	marker, ok, err := readGUIMarker(configDir)
	if err != nil {
		slog.Warn("could not read the web GUI's state; leaving the GUI as it is", "cause", err)
		return nil, ""
	}
	loaded := marker
	// The marker to write: none when it is as it was
	result := func() *guiMarker {
		if ok && marker == loaded {
			return nil
		}
		return &marker
	}
	notice := ""
	if !ok && !conf.GUI.Enabled {
		if guiIsLocalOnly(conf.GUI) || conf.GUI.IsAuthEnabled() {
			slog.Info("turning on the web GUI and REST API")
			conf.GUI.Enabled = true
			protectGUI(&conf.GUI)
		} else {
			notice = fmt.Sprintf("The web GUI and REST API were left off, as other computers could reach them on %s without a password.",
				conf.GUI.Address())
		}
	}
	if !conf.GUI.Enabled || os.Getenv("STGUIADDRESS") != "" {
		return result(), notice
	}
	if conf.GUI.Network() == "unix" {
		if err := startableSocket(conf.GUI.Address()); err != nil {
			slog.Warn("the web GUI can't listen on its socket; turning it off", "address", conf.GUI.Address(), "cause", err)
			conf.GUI.Enabled = false
			notice = fmt.Sprintf("The web GUI and REST API were turned off, as they can't listen on %s (%v).", conf.GUI.Address(), err)
		}
		return result(), notice
	}

	// Back where it is meant to be, once that is free and nobody moved it elsewhere meanwhile
	address := conf.GUI.Address()
	if marker.Moved != "" && address == marker.Moved && marker.Preferred != "" && listenable(marker.Preferred) == nil {
		slog.Info("moving the web GUI back", "address", marker.Preferred)
		conf.GUI.RawAddress = marker.Preferred
		address = marker.Preferred
		marker = guiMarker{}
	} else if address != marker.Moved {
		marker = guiMarker{}
	}

	err = listenable(address)
	switch {
	case err == nil:
		if marker.Moved != "" {
			notice = fmt.Sprintf("The web GUI and REST API are on %s while %s is in use.", marker.Moved, marker.Preferred)
		}
	case errors.Is(err, syscall.EADDRINUSE):
		host, _, _ := net.SplitHostPort(address)
		moved, freeErr := freeAddress(host)
		if freeErr != nil {
			conf.GUI.Enabled = false
			notice = fmt.Sprintf("The web GUI and REST API were turned off, as %s is in use and no other port is free.", address)
			break
		}
		slog.Warn("the web GUI's address is in use; moving it", "address", address, "to", moved)
		if marker.Preferred == "" {
			marker.Preferred = address
		}
		marker.Moved = moved
		conf.GUI.RawAddress = moved
		notice = fmt.Sprintf("The web GUI and REST API are on %s while %s is in use.", moved, marker.Preferred)
	default:
		slog.Warn("the web GUI can't listen on its address; turning it off", "address", address, "cause", err)
		conf.GUI.Enabled = false
		notice = fmt.Sprintf("The web GUI and REST API were turned off, as they can't listen on %s (%v).", address, err)
	}
	return result(), notice
}

// listenable tries listening on a TCP address
func listenable(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	return listener.Close()
}

// listenableSocket tries listening on a Unix socket at a path where nothing is. The GUI service would remove whatever
// is there, without asking.
func listenableSocket(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	return listener.Close()
}

// startableSocket is listenableSocket at a start, where a socket at the path is what an earlier run left
func startableSocket(path string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		return nil
	}
	return listenableSocket(path)
}

// freeAddress is an address on host with a port nothing listens on now
func freeAddress(host string) (string, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return net.JoinHostPort(host, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)), nil
}

// IsGUIEnabled reports whether the configuration has the web GUI and REST API on
func (clt *Client) IsGUIEnabled() bool {
	cfg := clt.cfg()
	return cfg != nil && cfg.GUI().Enabled
}

// GUIAddress is the address the web GUI and REST API listen on (as STGUIADDRESS may set it)
func (clt *Client) GUIAddress() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().Address()
}

// GUIURL is the URL of the web GUI and REST API
func (clt *Client) GUIURL() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().URL()
}

// GUIAPIKey is the key that the REST API takes in the X-API-Key header
func (clt *Client) GUIAPIKey() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().APIKey
}

// GUIStatusMessage says what the last start did about the web GUI (such as moving it off an address in use), if
// anything
func (clt *Client) GUIStatusMessage() string {
	if message, ok := clt.guiNotice.Load().(string); ok {
		return message
	}
	return ""
}

// SetGUIEnabled turns the web GUI and REST API on or off. Either takes effect when the engine restarts
// (RestartEngine): they only start with it, and a running one keeps listening. On iOS they can only be turned off.
func (clt *Client) SetGUIEnabled(enabled bool) error {
	if build.IsIOS && enabled {
		return ErrGUINotAvailable
	}
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.GUI.Enabled = enabled
		if enabled && !build.IsIOS {
			protectGUI(&cfg.GUI)
		}
	})
}

// GUISettings are the web GUI's and REST API's settings, which the app shows and saves together: each change makes the
// running GUI service listen again. Whether they are on is apart, as that takes an engine restart (SetGUIEnabled).
type GUISettings struct {
	Address string
	UseTLS  bool

	// "static" or "ldap"
	AuthMode string
	User     string
	// Whether there is a password, which the app never sees, even hashed
	HasPassword bool
	// The password to set; when empty, the password stays as it is
	Password string

	LDAPAddress string
	LDAPBindDN  string
	// "plain", "tls" or "starttls"
	LDAPTransport          string
	LDAPInsecureSkipVerify bool
	LDAPSearchBaseDN       string
	LDAPSearchFilter       string

	Theme               string
	MetricsWithoutAuth  bool
	SendBasicAuthPrompt bool
	// 0 is Syncthing's default of a week, and below 0 the session doesn't expire
	SessionCookieDurationS int
	SessionCookiePath      string

	InsecureAdminAccess       bool
	InsecureSkipHostCheck     bool
	InsecureAllowFrameLoading bool
}

func newGUISettings(gui config.GUIConfiguration, ldap config.LDAPConfiguration) *GUISettings {
	return &GUISettings{
		Address:                   gui.RawAddress,
		UseTLS:                    gui.RawUseTLS,
		AuthMode:                  gui.AuthMode.String(),
		User:                      gui.User,
		HasPassword:               gui.Password != "",
		LDAPAddress:               ldap.Address,
		LDAPBindDN:                ldap.BindDN,
		LDAPTransport:             ldap.Transport.String(),
		LDAPInsecureSkipVerify:    ldap.InsecureSkipVerify,
		LDAPSearchBaseDN:          ldap.SearchBaseDN,
		LDAPSearchFilter:          ldap.SearchFilter,
		Theme:                     gui.Theme,
		MetricsWithoutAuth:        gui.MetricsWithoutAuth,
		SendBasicAuthPrompt:       gui.SendBasicAuthPrompt,
		SessionCookieDurationS:    gui.SessionCookieDurationS,
		SessionCookiePath:         gui.SessionCookiePath,
		InsecureAdminAccess:       gui.InsecureAdminAccess,
		InsecureSkipHostCheck:     gui.InsecureSkipHostCheck,
		InsecureAllowFrameLoading: gui.InsecureAllowFrameLoading,
	}
}

// Equal reports whether both hold the same settings
func (s *GUISettings) Equal(other *GUISettings) bool {
	return *s == *other
}

// IsUnprotected reports whether, with these settings, nobody has to sign in: whoever reaches the GUI can then use all of
// it through its web pages' CSRF token, without the API key
func (s *GUISettings) IsUnprotected() bool {
	authenticated := s.AuthMode == config.AuthModeLDAP.String() || (s.User != "" && (s.HasPassword || s.Password != ""))
	return !authenticated
}

// IsExposed reports whether, with these settings, more than this computer can reach an unprotected GUI
func (s *GUISettings) IsExposed() bool {
	gui := config.GUIConfiguration{RawAddress: strings.TrimSpace(s.Address), InsecureSkipHostCheck: s.InsecureSkipHostCheck}
	return s.IsUnprotected() && !guiIsLocalOnly(gui)
}

func (s *GUISettings) ldapConfiguration() config.LDAPConfiguration {
	ldap := config.LDAPConfiguration{
		Address:            strings.TrimSpace(s.LDAPAddress),
		BindDN:             s.LDAPBindDN,
		InsecureSkipVerify: s.LDAPInsecureSkipVerify,
		SearchBaseDN:       s.LDAPSearchBaseDN,
		SearchFilter:       s.LDAPSearchFilter,
	}
	_ = ldap.Transport.UnmarshalText([]byte(s.LDAPTransport))
	return ldap
}

// applyTo checks the settings and puts them in gui and ldap, which are copies to throw away if that fails
func (s *GUISettings) applyTo(gui *config.GUIConfiguration, ldap *config.LDAPConfiguration) error {
	running := gui.RawAddress
	gui.RawAddress = strings.TrimSpace(s.Address)
	if gui.RawAddress == "" {
		return ErrGUIAddressRequired
	}
	if gui.RawAddress != running && !gui.IsOverridden() {
		if err := checkGUIAddress(gui, running); err != nil {
			return err
		}
	}
	gui.RawUseTLS = s.UseTLS

	_ = gui.AuthMode.UnmarshalText([]byte(s.AuthMode))
	*ldap = s.ldapConfiguration()
	if gui.AuthMode == config.AuthModeLDAP {
		if err := checkLDAP(*ldap); err != nil {
			return err
		}
	} else if err := s.applyCredentials(gui); err != nil {
		return err
	}

	if duration := int64(s.SessionCookieDurationS); duration > maxGUISessionS || duration < -maxGUISessionS {
		return ErrGUISessionTooLong
	}
	gui.Theme = s.Theme
	gui.MetricsWithoutAuth = s.MetricsWithoutAuth
	gui.SendBasicAuthPrompt = s.SendBasicAuthPrompt
	gui.SessionCookieDurationS = s.SessionCookieDurationS
	gui.SessionCookiePath = s.SessionCookiePath
	gui.InsecureAdminAccess = s.InsecureAdminAccess
	gui.InsecureSkipHostCheck = s.InsecureSkipHostCheck
	gui.InsecureAllowFrameLoading = s.InsecureAllowFrameLoading
	return nil
}

// checkGUIAddress reports what keeps the GUI from moving to the address in gui from the one it runs on. Unchecked, the
// service would fail to listen with no way to tell the user. If the GUI is off, prepareGUI checks when it starts.
func checkGUIAddress(gui *config.GUIConfiguration, running string) error {
	isSocket := gui.Network() == "unix"
	if !isSocket {
		if _, port, err := net.SplitHostPort(gui.RawAddress); err == nil && port == "0" {
			return ErrGUIPortRequired
		}
	}
	if !gui.Enabled {
		return nil
	}
	var err error
	if isSocket {
		err = listenableSocket(gui.RawAddress)
	} else if !sameTCPAddress(gui.RawAddress, running) {
		err = listenable(gui.RawAddress)
	}
	if err != nil {
		return fmt.Errorf("the web GUI and REST API can't listen there: %w", err)
	}
	return nil
}

// checkLDAP reports what would make every sign-in fail, or leave it to the server's policy on anonymous binds
func checkLDAP(ldap config.LDAPConfiguration) error {
	if _, _, err := net.SplitHostPort(ldap.Address); err != nil {
		return ErrGUILDAPAddressRequired
	}
	if ldap.BindDN == "" {
		return ErrGUILDAPBindDNRequired
	}
	if (ldap.SearchBaseDN == "") != (ldap.SearchFilter == "") {
		return ErrGUILDAPSearchIncomplete
	}
	return nil
}

// applyCredentials sets the user and password. Without a user there is no signing in, and no password either: one left
// behind would start to work for whichever user comes next.
func (s *GUISettings) applyCredentials(gui *config.GUIConfiguration) error {
	if s.User == "" {
		if s.Password != "" {
			return ErrGUIUserRequired
		}
		gui.User, gui.Password = "", ""
		return nil
	}
	gui.User = s.User
	if s.Password != "" {
		return gui.SetPassword(s.Password)
	}
	if gui.Password == "" {
		return ErrGUIPasswordRequired
	}
	return nil
}

// sameTCPAddress reports whether two addresses are one place to listen on, such as localhost and 127.0.0.1 on one port
func sameTCPAddress(a, b string) bool {
	first, err := net.ResolveTCPAddr("tcp", a)
	if err != nil {
		return false
	}
	second, err := net.ResolveTCPAddr("tcp", b)
	return err == nil && first.IP.Equal(second.IP) && first.Port == second.Port
}

// GUISettings are the settings of the web GUI and REST API; nil while the configuration is not loaded
func (clt *Client) GUISettings() *GUISettings {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newGUISettings(cfg.GUI(), cfg.LDAP())
}

// SetGUISettings changes the settings of the web GUI and REST API. A running GUI service listens again with them at
// once, with the engine left running.
func (clt *Client) SetGUISettings(settings *GUISettings) error {
	if build.IsIOS {
		return ErrGUINotAvailable
	}
	var invalid error
	moved := false
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		gui, ldap := cfg.GUI, cfg.LDAP
		if invalid = settings.applyTo(&gui, &ldap); invalid != nil {
			return
		}
		moved = gui.RawAddress != cfg.GUI.RawAddress
		cfg.GUI, cfg.LDAP = gui, ldap
	})
	if invalid != nil {
		return invalid
	}
	if err != nil {
		return err
	}
	if moved {
		// What the last start said about the address is no longer true, and no load will replace it
		clt.guiNotice.Store("")
	}
	return nil
}

// RegenerateGUIAPIKey replaces the API key, at once: whatever uses the old one loses access
func (clt *Client) RegenerateGUIAPIKey() error {
	if build.IsIOS {
		return ErrGUINotAvailable
	}
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.GUI.APIKey = rand.String(32)
	})
}

// IsGUIAddressOverridden reports whether STGUIADDRESS sets the address and TLS, whatever the configuration says
func (clt *Client) IsGUIAddressOverridden() bool {
	cfg := clt.cfg()
	return cfg != nil && cfg.GUI().IsOverridden()
}
