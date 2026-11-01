// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sushitrain

import (
	"log/slog"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

// engineConfig is the configuration as Syncthing holds it, for its model and for the web GUI and REST API. They know
// nothing of what the app keeps next to the configuration, so their changes are made to fit here.
type engineConfig struct {
	config.Wrapper
	client *Client
}

func (c *engineConfig) Modify(change config.ModifyFunction) (config.Waiter, error) {
	return c.client.modifyFollowingPauses(c.Wrapper, func(conf *config.Configuration) {
		change(conf)
		// Refusing this would refuse all of it, and the web GUI's Resume All saves the whole configuration
		if folders := c.client.remoteManaged.enforce(conf); len(folders) > 0 {
			slog.Warn("Keeping folders accessed through a remote file service paused", "folders", folders)
		}
	})
}

// modifyFollowingPauses changes the configuration and tells the app which devices this paused or resumed, as the app
// sets every device's pause from its own list (AppState.updateDeviceSuspension) and would undo the change otherwise. Only
// that goes around this, with SetDevicesPaused: anything else that pauses a device is the user's choice.
func (clt *Client) modifyFollowingPauses(cfg config.Wrapper, change config.ModifyFunction) (config.Waiter, error) {
	var paused, resumed []string
	waiter, err := cfg.Modify(func(conf *config.Configuration) {
		before := conf.DeviceMap()
		change(conf)
		paused, resumed = devicePauseChanges(before, conf, cfg.MyID())
	})
	if err != nil {
		return waiter, err
	}
	if len(paused) > 0 || len(resumed) > 0 {
		clt.mutex.Lock()
		delegate := clt.Delegate
		clt.mutex.Unlock()
		if delegate != nil {
			delegate.OnDevicePausesChanged(List(paused), List(resumed))
		}
	}
	return waiter, nil
}

// devicePauseChanges lists the devices a change paused and resumed. A new device always counts, as the app may still
// list it as paused from before it was removed. This device has no pause of its own, whatever a Pause All sets for it,
// and an entry with no ID won't be kept.
func devicePauseChanges(before map[protocol.DeviceID]config.DeviceConfiguration, after *config.Configuration,
	ownID protocol.DeviceID) (paused []string, resumed []string) {
	for _, device := range after.Devices {
		previous, known := before[device.DeviceID]
		if device.DeviceID == ownID || device.DeviceID == protocol.EmptyDeviceID ||
			(known && previous.Paused == device.Paused) {
			continue
		}
		if device.Paused {
			paused = append(paused, device.DeviceID.String())
		} else {
			resumed = append(resumed, device.DeviceID.String())
		}
	}
	return paused, resumed
}
