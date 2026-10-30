// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// syncthingHome is a home directory as Syncthing.app's backend leaves it: identity, configuration (with the GUI at
// guiAddress) and database
func syncthingHome(t *testing.T, guiAddress string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Syncthing")
	must(t, os.MkdirAll(dir, 0o700))
	cert, _, err := LoadIdentity(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	must(t, err)
	myID := protocol.NewDeviceID(cert.Certificate[0])
	cfg := config.New(myID)
	cfg.GUI.Enabled = true
	cfg.GUI.RawAddress = guiAddress
	cfg.GUI.APIKey = "secret-key"
	cfg.Folders = []config.FolderConfiguration{{ID: "default", Label: "Default", Path: "~/Sync"},
		{ID: "photos", Path: "/Volumes/Photos", Paused: true}}
	peer := protocol.NewDeviceID([]byte("peer"))
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: peer, Name: "peer", Paused: true})
	var buf bytes.Buffer
	must(t, cfg.WriteXML(&buf))
	must(t, os.WriteFile(filepath.Join(dir, "config.xml"), buf.Bytes(), 0o600))
	sdb, err := syncthing.OpenDatabase(filepath.Join(dir, "index-v2"), time.Hour)
	must(t, err)
	sdb.Close()
	return dir, myID.String()
}

// tree describes every file below dir, in the way snapshot does
func tree(t *testing.T, dir string) string {
	t.Helper()
	out := snapshot(t, dir)
	if entries, err := os.ReadDir(filepath.Join(dir, "index-v2")); err == nil && len(entries) > 0 {
		out += "index-v2:\n" + snapshot(t, filepath.Join(dir, "index-v2"))
	}
	return out
}

func TestSummarizeWritesNothing(t *testing.T) {
	dir, deviceID := syncthingHome(t, "127.0.0.1:8384")
	before := tree(t, dir)
	summary, err := Summarize(dir)
	must(t, err)
	if after := tree(t, dir); after != before {
		t.Fatalf("written:\n%s\nwas:\n%s", after, before)
	}
	if summary.DeviceID != deviceID || summary.ConfigVersion != config.CurrentVersion || len(summary.Folders) != 2 ||
		summary.Devices != 1 || !summary.GUIEnabled || summary.GUIAddress != "127.0.0.1:8384" ||
		summary.SchemaVersion != SupportedSchemaVersion || summary.TooNew || summary.Locked {
		t.Fatalf("summary %+v", summary)
	}
	if len(summary.PausedDevices) != 1 || summary.PausedDevices[0] != protocol.NewDeviceID([]byte("peer")).String() {
		t.Fatalf("paused devices %v", summary.PausedDevices)
	}
	if folder := summary.Folders[1]; folder.ID != "photos" || folder.Path != "/Volumes/Photos" || !folder.Paused {
		t.Fatalf("folder %+v", folder)
	}
}

func TestSummarizeAnEmptyDirectory(t *testing.T) {
	summary, err := Summarize(t.TempDir())
	must(t, err)
	if summary.DeviceID != "" || summary.ConfigVersion != 0 || summary.Locked {
		t.Fatalf("summary %+v", summary)
	}
}

func TestLockHeld(t *testing.T) {
	dir, _ := syncthingHome(t, "127.0.0.1:8384")
	if LockHeld(dir) {
		t.Fatal("held with no lock file")
	}
	if _, err := os.Stat(filepath.Join(dir, "syncthing.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the probe made a lock file")
	}
	other := flock.New(filepath.Join(dir, "syncthing.lock"))
	locked, err := other.TryLock()
	must(t, err)
	if !locked {
		t.Fatal("not locked")
	}
	if !LockHeld(dir) {
		t.Fatal("not seen as held")
	}
	must(t, other.Unlock())
	if LockHeld(dir) {
		t.Fatal("still seen as held")
	}
}

func TestAddressInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	if !AddressInUse(address) {
		t.Fatal("a listening address is free")
	}
	listener.Close()
	if AddressInUse(address) {
		t.Fatal("a closed address is in use")
	}
	for _, invalid := range []string{"/tmp/gui.sock", "tcp://0.0.0.0:1", ""} {
		if AddressInUse(invalid) {
			t.Errorf("%q is in use", invalid)
		}
	}
}

func TestImportCopiesTheHomeIntact(t *testing.T) {
	src, deviceID := syncthingHome(t, "127.0.0.1:8384")
	must(t, os.WriteFile(filepath.Join(src, "unrelated.log"), []byte("x"), 0o600))
	before := tree(t, src)
	dst := filepath.Join(t.TempDir(), "Synctrain")
	// As the app creates it
	must(t, os.MkdirAll(dst, 0o700))

	must(t, Import(src, dst))
	// Holding the lock makes its file, which Syncthing uses anyway
	must(t, os.Remove(filepath.Join(src, "syncthing.lock")))
	if after := tree(t, src); after != before {
		t.Fatalf("the source changed:\n%s\nwas:\n%s", after, before)
	}
	for _, item := range []string{"config.xml", "cert.pem", "key.pem"} {
		original, err := os.ReadFile(filepath.Join(src, item))
		must(t, err)
		copied, err := os.ReadFile(filepath.Join(dst, item))
		must(t, err)
		if !bytes.Equal(original, copied) {
			t.Fatalf("%s differs", item)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "unrelated.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copied more than the engine's data")
	}
	if _, err := os.Stat(dst + ".importing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staging folder is left")
	}
	summary, err := Summarize(dst)
	must(t, err)
	if summary.DeviceID != deviceID || summary.SchemaVersion != SupportedSchemaVersion || len(summary.Folders) != 2 {
		t.Fatalf("copy %+v", summary)
	}
	info, err := os.Stat(filepath.Join(dst, "key.pem"))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", info.Mode().Perm())
	}

	// Never over an existing copy
	if err := Import(src, dst); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("second import: %v", err)
	}
}

func TestImportRefusesADestinationWithFilesBeforeCopying(t *testing.T) {
	src, _ := syncthingHome(t, "127.0.0.1:8384")
	dst := filepath.Join(t.TempDir(), "Synctrain")
	must(t, os.MkdirAll(dst, 0o700))
	must(t, os.WriteFile(filepath.Join(dst, "RemoteFileService"), nil, 0o600))
	if err := Import(src, dst); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dst + ".importing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copied anyway")
	}
	// Finder's own file doesn't count
	must(t, os.Remove(filepath.Join(dst, "RemoteFileService")))
	must(t, os.WriteFile(filepath.Join(dst, ".DS_Store"), nil, 0o600))
	must(t, Import(src, dst))
}

func TestImportNeedsTheDeviceIdentity(t *testing.T) {
	for name, damage := range map[string]func(dir string){
		"missing": func(dir string) { must(t, os.Remove(filepath.Join(dir, "key.pem"))) },
		"damaged": func(dir string) { must(t, os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("x"), 0o600)) },
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := syncthingHome(t, "127.0.0.1:8384")
			damage(src)
			summary, err := Summarize(src)
			must(t, err)
			if summary.DeviceID != "" || summary.IdentityError == "" {
				t.Fatalf("summary %+v", summary)
			}
			dst := filepath.Join(t.TempDir(), "Synctrain")
			if err := Import(src, dst); !errors.Is(err, ErrIdentity) {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("copied without the identity")
			}
		})
	}
}

func TestImportStopsAtAnythingButFilesInTheDatabase(t *testing.T) {
	src, _ := syncthingHome(t, "127.0.0.1:8384")
	other := filepath.Join(t.TempDir(), "folder.0001-abcdefgh.db")
	must(t, os.WriteFile(other, []byte("db"), 0o600))
	must(t, os.Symlink(other, filepath.Join(src, "index-v2", "folder.0001-abcdefgh.db")))
	dst := filepath.Join(t.TempDir(), "Synctrain")
	if err := Import(src, dst); err == nil {
		t.Fatal("imported without a folder database")
	}
	for _, path := range []string{dst, dst + ".importing"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left", path)
		}
	}
}

func TestLockHomeKeepsSyncthingOut(t *testing.T) {
	dir, _ := syncthingHome(t, "127.0.0.1:8384")
	lock, err := LockHome(dir)
	must(t, err)
	if !LockHeld(dir) {
		t.Fatal("not seen as held")
	}
	if _, err := LockHome(dir); !errors.Is(err, ErrHomeLocked) {
		t.Fatalf("locked twice: %v", err)
	}
	must(t, lock.Close())
	if LockHeld(dir) {
		t.Fatal("still held")
	}
}

func TestLockHeldWhenItCannotTell(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	dir, _ := syncthingHome(t, "127.0.0.1:8384")
	must(t, os.WriteFile(filepath.Join(dir, "syncthing.lock"), nil, 0))
	if !LockHeld(dir) {
		t.Fatal("an unopenable lock file seen as free")
	}
}

func TestImportRefusesWhileSyncthingRuns(t *testing.T) {
	src, _ := syncthingHome(t, "127.0.0.1:8384")
	other := flock.New(filepath.Join(src, "syncthing.lock"))
	locked, err := other.TryLock()
	must(t, err)
	if !locked {
		t.Fatal("not locked")
	}
	defer other.Unlock()
	dst := filepath.Join(t.TempDir(), "Synctrain")
	if err := Import(src, dst); !errors.Is(err, ErrHomeLocked) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the destination was made")
	}
}

func TestImportRefusesNestedDirectories(t *testing.T) {
	src, _ := syncthingHome(t, "127.0.0.1:8384")
	for _, dst := range []string{src, filepath.Join(src, "inside"), filepath.Dir(src)} {
		if err := Import(src, dst); !errors.Is(err, ErrHomeNested) {
			t.Errorf("%s: got %v", dst, err)
		}
	}
}

// fakeSyncthing answers as the Syncthing with the given device ID would, and records whether it was asked to shut down
func fakeSyncthing(t *testing.T, deviceID func() string) (*httptest.Server, chan string) {
	t.Helper()
	asked := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Syncthing-ID", deviceID())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/noauth/health":
		case r.Method == http.MethodPost && r.URL.Path == "/rest/system/shutdown" && r.Header.Get("X-API-Key") == "secret-key":
			asked <- r.URL.Path
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(server.Close)
	return server, asked
}

func TestRequestShutdownAsksTheLocalSyncthing(t *testing.T) {
	var deviceID string
	server, asked := fakeSyncthing(t, func() string { return deviceID })
	dir, id := syncthingHome(t, server.Listener.Addr().String())
	deviceID = id
	must(t, RequestShutdown(dir))
	select {
	case <-asked:
	default:
		t.Fatal("not asked")
	}
}

func TestRequestShutdownSendsTheKeyNowhereElse(t *testing.T) {
	// Another program on the port
	server, asked := fakeSyncthing(t, func() string { return "someone else" })
	dir, _ := syncthingHome(t, server.Listener.Addr().String())
	if err := RequestShutdown(dir); !errors.Is(err, ErrNoGUIToReach) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-asked:
		t.Fatal("the key was sent")
	default:
	}
	// Not this computer's
	for _, address := range []string{"192.0.2.1:8384", "nonexistent-host.invalid:8384", "/tmp/gui.sock"} {
		remote, _ := syncthingHome(t, address)
		if err := RequestShutdown(remote); !errors.Is(err, ErrNoGUIToReach) {
			t.Errorf("%s: got %v", address, err)
		}
	}
}
