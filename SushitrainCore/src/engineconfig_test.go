// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sushitrain

import (
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

// pauseRecorder is a client delegate that keeps what it is told about paused devices
type pauseRecorder struct {
	eventCounter
	mutex   sync.Mutex
	paused  []string
	resumed []string
}

func (r *pauseRecorder) OnDevicePausesChanged(paused *ListOfStrings, resumed *ListOfStrings) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.paused = append(r.paused, paused.data...)
	r.resumed = append(r.resumed, resumed.data...)
}

// expectPauses checks what the delegate was told since the last check
func (r *pauseRecorder) expectPauses(t *testing.T, paused []string, resumed []string) {
	t.Helper()
	r.mutex.Lock()
	defer r.mutex.Unlock()
	slices.Sort(r.paused)
	slices.Sort(r.resumed)
	if !slices.Equal(r.paused, paused) || !slices.Equal(r.resumed, resumed) {
		t.Fatalf("told about paused %v and resumed %v; expected %v and %v", r.paused, r.resumed, paused, resumed)
	}
	r.paused, r.resumed = nil, nil
}

func testDeviceID(name string) protocol.DeviceID {
	return protocol.NewDeviceID([]byte(name))
}

// wrapEngineConfig returns a running configuration with this device and the given others, as Syncthing holds it
func wrapEngineConfig(t *testing.T, set *remoteManagedSet, devices []protocol.DeviceID,
	folders ...config.FolderConfiguration) (*engineConfig, *Client, *pauseRecorder) {
	t.Helper()
	id := testDeviceID("this device")
	cfg := config.New(id)
	for _, device := range devices {
		cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: device})
	}
	cfg.Folders = folders
	wrapper := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), cfg, id, events.NoopLogger)
	serveConfiguration(t, wrapper)
	wrapper.Subscribe(set)
	recorder := &pauseRecorder{}
	clt := (&Client{remoteManaged: set, Delegate: recorder}).withTestRun(wrapper, nil, nil)
	return &engineConfig{Wrapper: wrapper, client: clt}, clt, recorder
}

func modifyDevice(t *testing.T, cfg config.Wrapper, deviceID protocol.DeviceID, change func(*config.DeviceConfiguration)) {
	t.Helper()
	waiter, err := cfg.Modify(func(conf *config.Configuration) {
		device, _, _ := conf.Device(deviceID)
		change(&device)
		conf.SetDevice(device)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
}

func TestDevicePausedElsewhereReachesTheApp(t *testing.T) {
	first, second := testDeviceID("first"), testDeviceID("second")
	cfg, _, recorder := wrapEngineConfig(t, &remoteManagedSet{}, []protocol.DeviceID{first, second})
	pause := func(paused bool) func(*config.DeviceConfiguration) {
		return func(device *config.DeviceConfiguration) { device.Paused = paused }
	}

	modifyDevice(t, cfg, first, pause(true))
	recorder.expectPauses(t, []string{first.String()}, nil)

	// Nothing to tell
	modifyDevice(t, cfg, first, pause(true))
	modifyDevice(t, cfg, first, func(device *config.DeviceConfiguration) { device.Name = "renamed" })
	recorder.expectPauses(t, nil, nil)

	// The web GUI saves the whole configuration
	waiter, err := cfg.Modify(func(conf *config.Configuration) {
		to := conf.Copy()
		for i := range to.Devices {
			to.Devices[i].Paused = to.Devices[i].DeviceID == second
		}
		*conf = to
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	recorder.expectPauses(t, []string{second.String()}, []string{first.String()})

	// A Pause All also sets this device, which isn't one of the app's
	modifyDevice(t, cfg, cfg.MyID(), pause(true))
	recorder.expectPauses(t, nil, nil)
}

// A device the app still lists as paused, from before it was removed, doesn't come back paused
func TestNewDevicesReachTheApp(t *testing.T) {
	cfg, _, recorder := wrapEngineConfig(t, &remoteManagedSet{}, nil)
	resumed, paused := testDeviceID("resumed"), testDeviceID("paused")
	modifyDevice(t, cfg, resumed, func(device *config.DeviceConfiguration) { device.DeviceID = resumed })
	recorder.expectPauses(t, nil, []string{resumed.String()})
	modifyDevice(t, cfg, paused, func(device *config.DeviceConfiguration) {
		device.DeviceID = paused
		device.Paused = true
	})
	recorder.expectPauses(t, []string{paused.String()}, nil)
}

// An entry with no ID is dropped when the change is applied, so there is no such device to tell about
func TestAnEntryWithNoIDDoesNotReachTheApp(t *testing.T) {
	cfg, _, recorder := wrapEngineConfig(t, &remoteManagedSet{}, nil)
	waiter, err := cfg.Modify(func(conf *config.Configuration) {
		conf.Devices = append(conf.Devices, config.DeviceConfiguration{Paused: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	recorder.expectPauses(t, nil, nil)
}

// The app ignores events while it sleeps, when it still has to keep up with these
func TestPausesReachTheAppWhileItIgnoresEvents(t *testing.T) {
	device := testDeviceID("device")
	cfg, clt, recorder := wrapEngineConfig(t, &remoteManagedSet{}, []protocol.DeviceID{device})
	clt.IgnoreEvents = true
	modifyDevice(t, cfg, device, func(found *config.DeviceConfiguration) { found.Paused = true })
	recorder.expectPauses(t, []string{device.String()}, nil)
}

func TestARefusedChangeDoesNotReachTheApp(t *testing.T) {
	device := testDeviceID("device")
	cfg, _, recorder := wrapEngineConfig(t, &remoteManagedSet{}, []protocol.DeviceID{device})
	cfg.Subscribe(rejectAll{})
	_, err := cfg.Modify(func(conf *config.Configuration) {
		found, _, _ := conf.Device(device)
		found.Paused = true
		conf.SetDevice(found)
	})
	if err == nil {
		t.Fatal("the change was not refused")
	}
	recorder.expectPauses(t, nil, nil)
}

// The app's list goes by its own changes too, as through Peer.SetPaused for the devices table, but not by the pauses it
// sets from that list, or automatic pauses would end up as the user's
func TestOnlyTheAppsOwnPausesStayUnreported(t *testing.T) {
	device := testDeviceID("device")
	_, clt, recorder := wrapEngineConfig(t, &remoteManagedSet{}, []protocol.DeviceID{device})

	if err := clt.SetDevicesPaused(List(nil), false); err != nil {
		t.Fatal(err)
	}
	if !clt.PeerWithID(device.String()).IsPaused() {
		t.Fatal("the device was not paused")
	}
	if err := clt.SetDevicesPaused(List([]string{device.String()}), false); err != nil {
		t.Fatal(err)
	}
	if clt.PeerWithID(device.String()).IsPaused() {
		t.Fatal("the device was not resumed")
	}
	recorder.expectPauses(t, nil, nil)

	if err := clt.PeerWithID(device.String()).SetPaused(true); err != nil {
		t.Fatal(err)
	}
	recorder.expectPauses(t, []string{device.String()}, nil)
}

// The web GUI's Resume All saves every folder, so a remote-managed one must not get in the way of the others
func TestChangesFromElsewhereLeaveRemoteManagedFoldersPaused(t *testing.T) {
	set := &remoteManagedSet{path: filepath.Join(t.TempDir(), remoteManagedFileName), folders: []string{"remote"}}
	cfg, clt, _ := wrapEngineConfig(t, set, nil,
		config.FolderConfiguration{ID: "remote", Path: t.TempDir(), Paused: true},
		config.FolderConfiguration{ID: "local", Path: t.TempDir(), Paused: true})

	waiter, err := cfg.Modify(func(conf *config.Configuration) {
		to := conf.Copy()
		for i := range to.Folders {
			to.Folders[i].Paused = false
			to.Folders[i].Label = "changed"
		}
		*conf = to
	})
	if err != nil {
		t.Fatalf("resuming all folders: %v", err)
	}
	waiter.Wait()
	if local, _ := cfg.Folder("local"); local.Paused {
		t.Fatal("the ordinary folder was not resumed")
	}
	remote, _ := cfg.Folder("remote")
	if !remote.Paused {
		t.Fatal("the remote-managed folder was resumed")
	}
	if remote.Label != "changed" {
		t.Fatal("the rest of the change was lost for the remote-managed folder")
	}

	// The app says why instead
	if err := clt.FolderWithID("remote").SetPaused(false); !errors.Is(err, errRemoteManaged) {
		t.Fatalf("resuming it in the app: %v", err)
	}
}
