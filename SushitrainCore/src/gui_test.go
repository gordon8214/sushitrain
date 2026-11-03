// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

// freeGUIAddress is an address on 127.0.0.1 that nothing listens on right now
func freeGUIAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// writeGUIConfig writes a configuration as an earlier version of the app made it, with the GUI as given
func writeGUIConfig(t *testing.T, dir string, enabled bool, address string) string {
	t.Helper()
	cfg := config.New(protocolID(testIdentity(t, dir)))
	cfg.GUI.Enabled = enabled
	cfg.GUI.RawAddress = address
	path := filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, cfg)
	return path
}

func loadGUI(t *testing.T, path string, ios bool) (config.GUIConfiguration, string) {
	t.Helper()
	cert := testIdentity(t, filepath.Dir(path))
	cfg, notice, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, ios)
	if err != nil {
		t.Fatal(err)
	}
	saved := savedConfig(t, path, cert)
	if saved.GUI != cfg.GUI() {
		t.Fatal("the GUI's configuration was not saved")
	}
	return saved.GUI, notice
}

func TestTheGUIIsTurnedOnOnceOnMacOS(t *testing.T) {
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	gui, _ := loadGUI(t, path, false)
	if !gui.Enabled || !gui.IsAuthEnabled() || gui.User != guiUser {
		t.Fatalf("GUI on %v, with a password %v", gui.Enabled, gui.IsAuthEnabled())
	}

	// Turned off since, it stays off
	cert := testIdentity(t, dir)
	conf := savedConfig(t, path, cert)
	conf.GUI.Enabled = false
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Fatal("the GUI was turned on again")
	}
}

func TestTheGUIIsLeftOffWhereOthersCouldReachIt(t *testing.T) {
	_, port, _ := net.SplitHostPort(freeGUIAddress(t))
	for _, address := range []string{"0.0.0.0:" + port, ":" + port, "192.0.2.1:" + port} {
		dir := t.TempDir()
		path := writeGUIConfig(t, dir, false, address)
		if gui, notice := loadGUI(t, path, false); gui.Enabled || notice == "" {
			t.Errorf("%s: GUI on %v (%q)", address, gui.Enabled, notice)
		}
	}
	// Nor where websites could reach it through DNS rebinding
	dir := t.TempDir()
	cert := testIdentity(t, dir)
	conf := config.New(protocolID(cert))
	conf.GUI.Enabled = false
	conf.GUI.RawAddress = freeGUIAddress(t)
	conf.GUI.InsecureSkipHostCheck = true
	path := filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Error("turned on without the host check")
	}
	// With a password, anywhere
	dir = t.TempDir()
	cert = testIdentity(t, dir)
	conf = config.New(protocolID(cert))
	conf.GUI.Enabled = false
	conf.GUI.RawAddress = "0.0.0.0:" + port
	conf.GUI.User = "me"
	if err := conf.GUI.SetPassword("password"); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); !gui.Enabled || gui.User != "me" {
		t.Errorf("GUI on %v for %s", gui.Enabled, gui.User)
	}
}

func TestTheGUIStaysOffOnIOS(t *testing.T) {
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	if gui, _ := loadGUI(t, path, true); gui.Enabled {
		t.Fatal("the GUI was turned on")
	}
	if _, err := os.Stat(filepath.Join(dir, guiEnabledMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker written: %v", err)
	}
}

func TestAnUnreadableMarkerLeavesTheGUIAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	marker := filepath.Join(dir, guiEnabledMarkerName)
	if err := os.WriteFile(marker, nil, 0); err != nil {
		t.Fatal(err)
	}
	makeUnreadable(t, marker)
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Fatal("turned on, although it may have been turned off")
	}
}

func TestATakenGUIAddressMovesAndMovesBack(t *testing.T) {
	// As when Syncthing.app still runs
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := taken.Addr().String()

	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, address)
	gui, notice := loadGUI(t, path, false)
	host, port, err := net.SplitHostPort(gui.Address())
	if err != nil || host != "127.0.0.1" || gui.Address() == address || port == "0" || notice == "" {
		t.Fatalf("GUI at %s (%q)", gui.Address(), notice)
	}
	// Still taken: it stays, and says so
	if again, notice := loadGUI(t, path, false); again.Address() != gui.Address() || notice == "" {
		t.Fatalf("at %s (%q)", again.Address(), notice)
	}
	// Free again: back
	taken.Close()
	if back, notice := loadGUI(t, path, false); back.Address() != address || notice != "" {
		t.Fatalf("at %s (%q)", back.Address(), notice)
	}
}

func TestAnAddressChosenSinceIsNotMovedBack(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := taken.Addr().String()
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, address)
	loadGUI(t, path, false)
	taken.Close()

	// Chosen in the web GUI or the REST API meanwhile
	cert := testIdentity(t, dir)
	conf := savedConfig(t, path, cert)
	chosen := freeGUIAddress(t)
	conf.GUI.RawAddress = chosen
	writeConfiguration(t, path, conf)
	if gui, notice := loadGUI(t, path, false); gui.Address() != chosen || notice != "" {
		t.Fatalf("at %s (%q)", gui.Address(), notice)
	}
}

// socketDir is a directory with a path short enough for a Unix socket in it, as t.TempDir's may not be

func guiForSettings(t *testing.T) config.GUIConfiguration {
	t.Helper()
	gui := config.Configuration{}.GUI
	gui.Enabled = true
	gui.RawAddress = freeGUIAddress(t)
	gui.User = "me"
	if err := gui.SetPassword("password"); err != nil {
		t.Fatal(err)
	}
	return gui
}

func applyGUISettings(gui config.GUIConfiguration, change func(*GUISettings)) (config.GUIConfiguration, config.LDAPConfiguration, error) {
	ldap := config.LDAPConfiguration{}
	settings := newGUISettings(gui, ldap)
	change(settings)
	err := settings.applyTo(&gui, &ldap)
	return gui, ldap, err
}

func TestGUISettingsLeaveAnUnchangedConfigurationAlone(t *testing.T) {
	gui := config.New(protocolID(testIdentity(t, t.TempDir()))).GUI
	gui.RawAddress = freeGUIAddress(t)
	gui.RawUseTLS = true
	gui.User = "me"
	gui.Password = "$2a$10$not-really-a-hash"
	gui.MetricsWithoutAuth = true
	gui.SendBasicAuthPrompt = true
	gui.SessionCookieDurationS = -1
	gui.SessionCookiePath = "/syncthing"
	gui.InsecureAdminAccess = true
	gui.InsecureSkipHostCheck = true
	gui.InsecureAllowFrameLoading = true
	gui.Theme = "my-own-theme"
	ldap := config.LDAPConfiguration{
		Address:            "ldap.example.com:636",
		BindDN:             "cn=%s,dc=example,dc=com",
		Transport:          config.LDAPTransportStartTLS,
		InsecureSkipVerify: true,
		SearchBaseDN:       "dc=example,dc=com",
		SearchFilter:       "(uid=%s)",
	}

	for _, mode := range []config.AuthMode{config.AuthModeStatic, config.AuthModeLDAP} {
		gui.AuthMode = mode
		settings := newGUISettings(gui, ldap)
		if settings.AuthMode != mode.String() || !settings.HasPassword {
			t.Fatalf("settings: %+v", settings)
		}
		toGUI, toLDAP := gui, config.LDAPConfiguration{}
		if err := settings.applyTo(&toGUI, &toLDAP); err != nil {
			t.Fatal(err)
		}
		if toGUI != gui || toLDAP != ldap {
			t.Fatalf("%s: changed to %+v, %+v", mode, toGUI, toLDAP)
		}
	}
}

func TestGUISettingsChangeThePassword(t *testing.T) {
	gui := guiForSettings(t)
	hash := gui.Password

	// Left empty, it stays
	kept, _, err := applyGUISettings(gui, func(s *GUISettings) { s.User = "you" })
	if err != nil || kept.Password != hash || kept.User != "you" {
		t.Fatalf("%s with a password %v (%v)", kept.User, kept.Password == hash, err)
	}

	changed, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Password = "another" })
	if err != nil || changed.CompareHashedPassword("another") != nil {
		t.Fatalf("the new password does not work (%v)", err)
	}
}

func TestGUISettingsWithoutAUserHaveNoPassword(t *testing.T) {
	gui := guiForSettings(t)
	open, _, err := applyGUISettings(gui, func(s *GUISettings) { s.User = "" })
	if err != nil || open.User != "" || open.Password != "" || open.IsAuthEnabled() {
		t.Fatalf("user %q, password left %v (%v)", open.User, open.Password != "", err)
	}

	// A user that comes next doesn't get the old password
	if _, _, err := applyGUISettings(open, func(s *GUISettings) { s.User = "me" }); !errors.Is(err, ErrGUIPasswordRequired) {
		t.Fatalf("a user without a password: %v", err)
	}
	// A new password isn't dropped for want of a user
	if _, _, err := applyGUISettings(open, func(s *GUISettings) { s.Password = "another" }); !errors.Is(err, ErrGUIUserRequired) {
		t.Fatalf("a password without a user: %v", err)
	}

	// Leaving LDAP with no user to return to also ends signing in, and says so
	ldap := open
	ldap.AuthMode = config.AuthModeLDAP
	back, _, err := applyGUISettings(ldap, func(s *GUISettings) { s.AuthMode = "static" })
	if err != nil || back.IsAuthEnabled() || !newGUISettings(back, config.LDAPConfiguration{}).IsUnprotected() {
		t.Fatalf("still signing in: %v (%v)", back.IsAuthEnabled(), err)
	}
}

func TestGUISettingsForLDAP(t *testing.T) {
	gui := guiForSettings(t)
	hash := gui.Password
	useLDAP := func(s *GUISettings) {
		s.AuthMode = "ldap"
		s.LDAPAddress = " ldap.example.com:389 "
		s.LDAPBindDN = "cn=%s,dc=example,dc=com"
	}
	for _, test := range []struct {
		change func(*GUISettings)
		want   error
	}{
		{func(s *GUISettings) { s.LDAPAddress = "" }, ErrGUILDAPAddressRequired},
		{func(s *GUISettings) { s.LDAPAddress = "ldap.example.com" }, ErrGUILDAPAddressRequired},
		{func(s *GUISettings) { s.LDAPBindDN = "" }, ErrGUILDAPBindDNRequired},
		{func(s *GUISettings) { s.LDAPSearchBaseDN = "dc=example,dc=com" }, ErrGUILDAPSearchIncomplete},
		{func(s *GUISettings) { s.LDAPSearchFilter = "(uid=%s)" }, ErrGUILDAPSearchIncomplete},
	} {
		_, _, err := applyGUISettings(gui, func(s *GUISettings) {
			useLDAP(s)
			test.change(s)
		})
		if !errors.Is(err, test.want) {
			t.Errorf("wanted %v, got %v", test.want, err)
		}
	}

	changed, ldap, err := applyGUISettings(gui, func(s *GUISettings) {
		useLDAP(s)
		s.User = ""
		s.Password = "left from static"
		s.LDAPTransport = "starttls"
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.AuthMode != config.AuthModeLDAP || !changed.IsAuthEnabled() {
		t.Fatalf("mode %s", changed.AuthMode)
	}
	if ldap.Address != "ldap.example.com:389" || ldap.Transport != config.LDAPTransportStartTLS {
		t.Fatalf("LDAP: %+v", ldap)
	}
	// Static's credentials wait for a return to static
	if changed.User != "me" || changed.Password != hash {
		t.Fatalf("user %q, password kept %v", changed.User, changed.Password == hash)
	}
}

func TestGUISettingsCheckTheAddress(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	gui := guiForSettings(t)
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = "  " }); !errors.Is(err, ErrGUIAddressRequired) {
		t.Fatalf("empty: %v", err)
	}
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = taken.Addr().String() }); !addressInUse(err) {
		t.Fatalf("in use: %v", err)
	}
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = "127.0.0.1" }); err == nil {
		t.Fatal("no port was accepted")
	}
	// Any port would do for the service, and the app could not tell which it got
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = "127.0.0.1:0" }); !errors.Is(err, ErrGUIPortRequired) {
		t.Fatalf("port 0: %v", err)
	}

	// The GUI's own address is not in use by something else, however it is written
	_, port, _ := net.SplitHostPort(taken.Addr().String())
	gui.RawAddress = taken.Addr().String()
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.UseTLS = true }); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	if moved, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = "localhost:" + port }); err != nil || moved.RawAddress != "localhost:"+port {
		t.Fatalf("another spelling: %s (%v)", moved.RawAddress, err)
	}

	// Nothing will listen yet
	gui = guiForSettings(t)
	gui.Enabled = false
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = taken.Addr().String() }); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = "127.0.0.1:0" }); !errors.Is(err, ErrGUIPortRequired) {
		t.Fatalf("port 0 while off: %v", err)
	}
}

func TestGUISettingsBoundTheSessionLifetime(t *testing.T) {
	gui := guiForSettings(t)
	limit := int(maxGUISessionS)
	for seconds, want := range map[int]error{
		limit: nil, limit + 1: ErrGUISessionTooLong, -limit: nil, -limit - 1: ErrGUISessionTooLong,
	} {
		_, _, err := applyGUISettings(gui, func(s *GUISettings) { s.SessionCookieDurationS = seconds })
		if !errors.Is(err, want) {
			t.Errorf("%d: wanted %v, got %v", seconds, want, err)
		}
	}
}

func TestGUISettingsKeepTheAddressOfTheConfiguration(t *testing.T) {
	gui := guiForSettings(t)
	address := gui.RawAddress
	t.Setenv("STGUIADDRESS", "127.0.0.1:1")
	settings := newGUISettings(gui, config.LDAPConfiguration{})
	if settings.Address != address {
		t.Fatalf("the override was taken for the address: %s", settings.Address)
	}
	if changed, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = freeGUIAddress(t) }); err != nil || changed.RawAddress == address {
		t.Fatalf("%s (%v)", changed.RawAddress, err)
	}
}

func TestGUISettingsTellWhenTheGUIIsExposed(t *testing.T) {
	const loopback, everywhere = "127.0.0.1:8384", "0.0.0.0:8384"
	static, ldap := config.AuthModeStatic.String(), config.AuthModeLDAP.String()
	for _, test := range []struct {
		settings    GUISettings
		unprotected bool
		exposed     bool
	}{
		// Users and programs of this computer can still get in
		{GUISettings{Address: loopback, AuthMode: static}, true, false},
		{GUISettings{Address: "localhost:8384", AuthMode: static}, true, false},
		{GUISettings{Address: "/tmp/synctrain.sock", AuthMode: static}, true, false},
		{GUISettings{Address: everywhere, AuthMode: static}, true, true},
		{GUISettings{Address: everywhere, AuthMode: static, User: "me"}, true, true},
		{GUISettings{Address: everywhere, AuthMode: static, HasPassword: true}, true, true},
		{GUISettings{Address: everywhere, AuthMode: static, User: "me", HasPassword: true}, false, false},
		{GUISettings{Address: everywhere, AuthMode: static, User: "me", Password: "password"}, false, false},
		{GUISettings{Address: everywhere, AuthMode: ldap}, false, false},
		// Without the host check, websites reach a loopback address through DNS rebinding
		{GUISettings{Address: loopback, AuthMode: static, InsecureSkipHostCheck: true}, true, true},
		{GUISettings{Address: loopback, AuthMode: static, InsecureSkipHostCheck: true, User: "me", HasPassword: true}, false, false},
	} {
		if test.settings.IsUnprotected() != test.unprotected {
			t.Errorf("%+v: unprotected %v", test.settings, !test.unprotected)
		}
		if test.settings.IsExposed() != test.exposed {
			t.Errorf("%+v: exposed %v", test.settings, !test.exposed)
		}
	}
}

func TestAnAddressThatCannotBeUsedTurnsTheGUIOff(t *testing.T) {
	// Not on this computer, so nothing can listen there and the engine could not start
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, "192.0.2.1:8384")
	if gui, notice := loadGUI(t, path, false); gui.Enabled || notice == "" {
		t.Fatalf("GUI on %v (%q)", gui.Enabled, notice)
	}
}

func TestAnAddressFromTheEnvironmentIsLeftAlone(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	t.Setenv("STGUIADDRESS", taken.Addr().String())
	dir := t.TempDir()
	address := freeGUIAddress(t)
	path := writeGUIConfig(t, dir, true, address)
	if gui, notice := loadGUI(t, path, false); gui.RawAddress != address || notice != "" {
		t.Fatalf("GUI at %s (%q)", gui.RawAddress, notice)
	}
}

func TestAGeneratedGUIPasswordIsKnownAsSuch(t *testing.T) {
	// A new configuration, and one that gets the GUI turned on
	dir := t.TempDir()
	testIdentity(t, dir)
	gui, _ := loadGUI(t, filepath.Join(dir, "config.xml"), false)
	if gui.User != guiUser || !guiPasswordIsUnknown(gui, dir) {
		t.Fatalf("user %q, password unknown %v", gui.User, guiPasswordIsUnknown(gui, dir))
	}
	dir = t.TempDir()
	gui, _ = loadGUI(t, writeGUIConfig(t, dir, false, freeGUIAddress(t)), false)
	if !guiPasswordIsUnknown(gui, dir) {
		t.Fatal("the password of a GUI turned on is not known to be generated")
	}

	// Another user keeps the password, which is still nobody's
	renamed, _, err := applyGUISettings(gui, func(s *GUISettings) { s.User = "you" })
	if err != nil || !guiPasswordIsUnknown(renamed, dir) {
		t.Fatalf("renaming the user: unknown %v (%v)", guiPasswordIsUnknown(renamed, dir), err)
	}

	// Each way of being able to sign in ends it
	chosen, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Password = "another" })
	if err != nil || guiPasswordIsUnknown(chosen, dir) {
		t.Fatalf("a chosen password: unknown %v (%v)", guiPasswordIsUnknown(chosen, dir), err)
	}
	open, _, err := applyGUISettings(gui, func(s *GUISettings) { s.User = "" })
	if err != nil || guiPasswordIsUnknown(open, dir) {
		t.Fatalf("no user: unknown %v (%v)", guiPasswordIsUnknown(open, dir), err)
	}
	ldap := gui
	ldap.AuthMode = config.AuthModeLDAP
	if guiPasswordIsUnknown(ldap, dir) {
		t.Fatal("an LDAP server decides, not the password")
	}
}

func TestAGUIThatHasAPasswordKeepsItsOwn(t *testing.T) {
	dir := t.TempDir()
	gui := config.GUIConfiguration{User: "me"}
	if err := gui.SetPassword("mine"); err != nil {
		t.Fatal(err)
	}
	hash := gui.Password
	protectGUI(&gui, dir)
	if gui.Password != hash || guiPasswordIsUnknown(gui, dir) {
		t.Fatalf("password kept %v, unknown %v", gui.Password == hash, guiPasswordIsUnknown(gui, dir))
	}
}

func TestTheGUICertificateIsHashedAsServed(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "https-cert.pem")
	before := locations.Get(locations.HTTPSCertFile)
	locations.Set(locations.HTTPSCertFile, certFile)
	t.Cleanup(func() { locations.Set(locations.HTTPSCertFile, before) })

	clt := &Client{}
	if hash := clt.GUICertificateSHA256(); hash != nil {
		t.Fatalf("a hash without a certificate: %x", hash)
	}
	cert, err := tlsutil.NewCertificate(certFile, filepath.Join(dir, "https-key.pem"), "syncthing", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(cert.Certificate[0])
	if got := clt.GUICertificateSHA256(); !bytes.Equal(got, want[:]) {
		t.Fatalf("hash %x, served %x", got, want)
	}

	// What comes before the certificate isn't it
	certificate, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, "https-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, append(key, certificate...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := clt.GUICertificateSHA256(); !bytes.Equal(got, want[:]) {
		t.Fatalf("with a key in front: hash %x, served %x", got, want)
	}
}
