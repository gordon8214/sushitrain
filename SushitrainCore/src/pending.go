// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"cmp"
	"slices"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

// ObservedDevice is a device that tried to connect without being configured, as far as it told who it is
type ObservedDevice struct {
	DeviceID string
	Name     string
	Address  string
	time     time.Time
}

// Time is when the device last tried
func (d *ObservedDevice) Time() *Date {
	return &Date{time: d.time}
}

// ObservedDevices is a list of devices, the most recent first
type ObservedDevices struct {
	items []*ObservedDevice
}

func newObservedDevices(items []*ObservedDevice) *ObservedDevices {
	slices.SortFunc(items, func(a, b *ObservedDevice) int {
		return cmp.Or(b.time.Compare(a.time), cmp.Compare(a.DeviceID, b.DeviceID))
	})
	return &ObservedDevices{items: items}
}

func (l *ObservedDevices) Count() int {
	return len(l.items)
}

func (l *ObservedDevices) ItemAt(index int) *ObservedDevice {
	return l.items[index]
}

// PendingDevices are the devices that tried to connect without being configured
func (clt *Client) PendingDevices() (*ObservedDevices, error) {
	internals := clt.internals()
	if internals == nil {
		return nil, ErrStillLoading
	}
	pending, err := internals.PendingDevices()
	if err != nil {
		return nil, err
	}
	items := make([]*ObservedDevice, 0, len(pending))
	for id, device := range pending {
		items = append(items, &ObservedDevice{DeviceID: id.String(), Name: device.Name, Address: device.Address,
			time: device.Time})
	}
	return newObservedDevices(items), nil
}

// DismissPendingDevice forgets a request to connect. The device comes back if it tries again.
func (clt *Client) DismissPendingDevice(deviceID string) error {
	id, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}
	internals := clt.internals()
	if internals == nil {
		return ErrStillLoading
	}
	return internals.DismissPendingDevice(id)
}

// IgnoredDevices are the devices whose requests to connect are ignored
func (clt *Client) IgnoredDevices() *ObservedDevices {
	wrapper := clt.cfg()
	if wrapper == nil {
		return newObservedDevices(nil)
	}
	ignored := wrapper.RawCopy().IgnoredDevices
	items := make([]*ObservedDevice, 0, len(ignored))
	for _, device := range ignored {
		items = append(items, &ObservedDevice{DeviceID: device.ID.String(), Name: device.Name, Address: device.Address,
			time: device.Time})
	}
	return newObservedDevices(items)
}

// IgnoreDevice has the engine turn the device away from now on, which also ends its request to connect
func (clt *Client) IgnoreDevice(deviceID string) error {
	id, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}
	ignored := config.ObservedDevice{Time: time.Now().Round(time.Second), ID: id}
	if internals := clt.internals(); internals != nil {
		if pending, err := internals.PendingDevices(); err == nil {
			ignored.Name, ignored.Address = pending[id].Name, pending[id].Address
		}
	}
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		isIgnored := func(device config.ObservedDevice) bool { return device.ID == id }
		if !slices.ContainsFunc(cfg.IgnoredDevices, isIgnored) {
			cfg.IgnoredDevices = append(cfg.IgnoredDevices, ignored)
		}
	})
}

// UnignoreDevice lets the device ask to connect again
func (clt *Client) UnignoreDevice(deviceID string) error {
	id, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.IgnoredDevices = slices.DeleteFunc(slices.Clone(cfg.IgnoredDevices), func(device config.ObservedDevice) bool {
			return device.ID == id
		})
	})
}

// IgnoredFolder is a folder a device offered that won't be asked about anymore
type IgnoredFolder struct {
	FolderID string
	Label    string
	time     time.Time
}

// Time is when the device offered it
func (f *IgnoredFolder) Time() *Date {
	return &Date{time: f.time}
}

// IgnoredFolders is a list of folders, by label
type IgnoredFolders struct {
	items []*IgnoredFolder
}

func (l *IgnoredFolders) Count() int {
	return len(l.items)
}

func (l *IgnoredFolders) ItemAt(index int) *IgnoredFolder {
	return l.items[index]
}

// IgnoredFolders are the folders this device offered that won't be asked about anymore
func (peer *Peer) IgnoredFolders() *IgnoredFolders {
	items := []*IgnoredFolder{}
	if wrapper := peer.client.cfg(); wrapper != nil {
		if device, ok := wrapper.Device(peer.deviceID); ok {
			for _, folder := range device.IgnoredFolders {
				items = append(items, &IgnoredFolder{FolderID: folder.ID, Label: folder.Label, time: folder.Time})
			}
		}
	}
	slices.SortFunc(items, func(a, b *IgnoredFolder) int {
		return cmp.Or(cmp.Compare(a.Label, b.Label), cmp.Compare(a.FolderID, b.FolderID))
	})
	return &IgnoredFolders{items: items}
}

// UnignoreFolder lets the device offer the folder again
func (peer *Peer) UnignoreFolder(folderID string) error {
	return peer.changeDeviceCopy(func(device *config.DeviceConfiguration) error {
		device.IgnoredFolders = slices.DeleteFunc(slices.Clone(device.IgnoredFolders), func(folder config.ObservedFolder) bool {
			return folder.ID == folderID
		})
		return nil
	})
}
