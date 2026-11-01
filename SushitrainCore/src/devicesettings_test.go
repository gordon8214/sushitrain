// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

var settingsTestDeviceID = protocol.DeviceID{4, 2}

func settingsTestDevice(t *testing.T) (*Client, *Peer) {
	t.Helper()
	client, _ := advancedTestClient(t)
	device := client.cfg().DefaultDevice()
	device.DeviceID = settingsTestDeviceID
	if err := client.changeConfiguration(func(cfg *config.Configuration) { cfg.SetDevice(device) }); err != nil {
		t.Fatal(err)
	}
	return client, &Peer{client: client, deviceID: settingsTestDeviceID}
}

func deviceConfiguration(t *testing.T, client *Client) config.DeviceConfiguration {
	t.Helper()
	device, ok := client.cfg().Device(settingsTestDeviceID)
	if !ok {
		t.Fatal("the device is gone")
	}
	return device
}

func TestDeviceSettingsChangeNothingUnlessEdited(t *testing.T) {
	client, peer := settingsTestDevice(t)
	before := deviceConfiguration(t, client)
	if err := peer.SetSettings(peer.Settings()); err != nil {
		t.Fatal(err)
	}
	if after := deviceConfiguration(t, client); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
}

func TestDeviceSettingsAreSaved(t *testing.T) {
	client, peer := settingsTestDevice(t)
	settings := peer.Settings()
	settings.Compression = "always"
	settings.AutoAcceptFolders = true
	settings.MaxSendMbits = "10"
	settings.MaxRecvMbits = "100"
	settings.AllowedNetworks = "192.168.0.0/16\n!10.0.0.0/8"
	settings.NumConnections = "4"
	settings.SkipIntroductionRemovals = true
	settings.MaxRequestKiB = "2048"
	settings.CertName = " example.org "
	settings.RemoteGUIPort = "8385"
	if err := peer.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	after := deviceConfiguration(t, client)
	if after.Compression != config.CompressionAlways || !after.AutoAcceptFolders ||
		after.MaxSendKbps != mbitsPerSecondToKiBPerSecond(10) || after.MaxRecvKbps != mbitsPerSecondToKiBPerSecond(100) ||
		!slices.Equal(after.AllowedNetworks, []string{"192.168.0.0/16", "!10.0.0.0/8"}) ||
		after.RawNumConnections != 4 || !after.SkipIntroductionRemovals || after.MaxRequestKiB != 2048 ||
		after.CertName != "example.org" || after.RemoteGUIPort != 8385 {
		t.Fatalf("got %+v", after)
	}
}

// A limit in KiB/s that the web GUI set needn't be a whole number of Mbit/s
func TestDeviceSettingsKeepALimitThatWasNotEdited(t *testing.T) {
	client, peer := settingsTestDevice(t)
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		device, _, _ := cfg.Device(settingsTestDeviceID)
		device.MaxSendKbps, device.MaxRecvKbps = 100, 1234
		cfg.SetDevice(device)
	}); err != nil {
		t.Fatal(err)
	}

	settings := peer.Settings()
	if settings.MaxSendMbits != "1" {
		t.Fatalf("a small limit reads as %s Mbit/s, and 0 is no limit", settings.MaxSendMbits)
	}
	settings.MaxRecvMbits = "1"
	if err := peer.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	after := deviceConfiguration(t, client)
	if after.MaxSendKbps != 100 || after.MaxRecvKbps != mbitsPerSecondToKiBPerSecond(1) {
		t.Fatalf("got %d, %d", after.MaxSendKbps, after.MaxRecvKbps)
	}
}

func TestDeviceSettingsLeaveChangesFromElsewhereAlone(t *testing.T) {
	client, peer := settingsTestDevice(t)
	settings := peer.Settings()
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		device, _, _ := cfg.Device(settingsTestDeviceID)
		device.RemoteGUIPort = 9000
		cfg.SetDevice(device)
	}); err != nil {
		t.Fatal(err)
	}
	settings.NumConnections = "2"
	if err := peer.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	after := deviceConfiguration(t, client)
	if after.RemoteGUIPort != 9000 || after.RawNumConnections != 2 {
		t.Fatalf("got %+v", after)
	}
}

func TestDeviceSettingsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		edit func(*DeviceSettings)
	}{
		{"compression", func(s *DeviceSettings) { s.Compression = "maximum" }},
		{"negative limit", func(s *DeviceSettings) { s.MaxSendMbits = "-1" }},
		{"a limit that doesn't fit", func(s *DeviceSettings) { s.MaxRecvMbits = "17592187" }},
		{"no number", func(s *DeviceSettings) { s.NumConnections = "several" }},
		{"negative request size", func(s *DeviceSettings) { s.MaxRequestKiB = "-1" }},
		{"port", func(s *DeviceSettings) { s.RemoteGUIPort = "65536" }},
		{"network without a mask", func(s *DeviceSettings) { s.AllowedNetworks = "192.168.1.1" }},
		{"negated nonsense", func(s *DeviceSettings) { s.AllowedNetworks = "192.168.0.0/16\n!home" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, peer := settingsTestDevice(t)
			before := deviceConfiguration(t, client)
			settings := peer.Settings()
			settings.CertName = "changed"
			tt.edit(settings)
			if err := peer.SetSettings(settings); err == nil {
				t.Fatal("saved")
			}
			if after := deviceConfiguration(t, client); !reflect.DeepEqual(before, after) {
				t.Fatal("something was changed")
			}
		})
	}
}

// The engine would turn it off again without saying so
func TestDeviceSettingsRefuseAutoAcceptFromAnUntrustedDevice(t *testing.T) {
	client, peer := settingsTestDevice(t)
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		device, _, _ := cfg.Device(settingsTestDeviceID)
		device.Untrusted = true
		cfg.SetDevice(device)
	}); err != nil {
		t.Fatal(err)
	}
	settings := peer.Settings()
	if !settings.Untrusted {
		t.Fatal("untrusted doesn't show")
	}
	settings.AutoAcceptFolders = true
	if err := peer.SetSettings(settings); !errors.Is(err, errAutoAcceptUntrusted) {
		t.Fatalf("got %v", err)
	}
}

func TestDeviceSettingsOfADeviceThatIsGone(t *testing.T) {
	client, peer := settingsTestDevice(t)
	settings := peer.Settings()
	gone := &Peer{client: client, deviceID: protocol.DeviceID{9}}
	if gone.Settings() != nil {
		t.Fatal("got settings")
	}
	if err := gone.SetSettings(settings); !errors.Is(err, errDeviceConfigNotFound) {
		t.Fatalf("got %v", err)
	}
}
