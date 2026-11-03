// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"t-shaped.nl/sushitrain/v2/gatewayadmin"
	"t-shaped.nl/sushitrain/v2/sthome"
)

// homeCommand describes Syncthing's home before the gateway takes it over, and helps with that: it asks the Syncthing
// running there to stop, and copies the home aside. Synctrain's window on Linux runs it when it sets Synchronize mode
// up, as it doesn't link the engine.
func homeCommand(args []string) error {
	fs, cf := newFlagSet("home")
	stop := fs.Bool("stop", false, "ask the Syncthing running on the home to shut down, through its REST API, and wait until it has")
	backup := fs.String("backup", "", "copy the home's configuration, identity and database to this new directory (as reflinks where the volume can), while no Syncthing runs on it")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	configDir, dataDir := baseDirs(cf.home)
	if configDir != dataDir && (*stop || *backup != "") {
		return errors.New("Syncthing's configuration and database are in different directories: give the home with --home")
	}
	switch {
	case *stop:
		return stopSyncthing(configDir)
	case *backup != "":
		if err := sthome.Import(configDir, *backup); err != nil {
			return err
		}
		fmt.Println("Copied to", *backup)
		return nil
	}

	h := describeHome(configDir, dataDir, adminSocket(cf.socket, dataDir))
	if *asJSON {
		return printJSON(h)
	}
	fmt.Println("Syncthing's home:", h.Dir)
	switch {
	case !h.Exists:
		fmt.Println("No Syncthing configuration yet: serve makes a new device.")
	default:
		fmt.Printf("Device %s, %d folders, %d other devices\n", h.DeviceID, len(h.Folders), h.Devices)
	}
	if h.Locked {
		fmt.Println("A Syncthing runs on it now.")
	}
	if h.Problem != "" {
		fmt.Println("It can't be taken over:", h.Problem)
	}
	return nil
}

// describeHome summarizes Syncthing's home for the window
func describeHome(configDir, dataDir, socket string) gatewayadmin.Home {
	h := gatewayadmin.Home{Dir: configDir, Socket: socket}
	if _, err := os.Stat(serviceHome(dataDir)); err == nil {
		h.Taken = true
	}
	if configDir != dataDir {
		h.Problem = "its configuration and database are in different directories"
	}
	summary, err := sthome.Summarize(configDir)
	h.Exists = summary.ConfigVersion > 0 || summary.DeviceID != ""
	h.DeviceID, h.Devices, h.Locked = summary.DeviceID, summary.Devices, summary.Locked
	for _, f := range summary.Folders {
		label := f.Label
		if label == "" {
			label = f.ID
		}
		h.Folders = append(h.Folders, label)
	}
	switch {
	case err != nil:
		h.Exists = true
		h.Problem = err.Error()
	case summary.TooNew:
		h.Problem = "it is from a newer Syncthing than Synctrain's engine"
	case summary.ConfigVersion > 0 && summary.IdentityError != "":
		h.Problem = "its device identity can't be used: " + summary.IdentityError
	}
	if summary.GUIEnabled && summary.GUINetwork == "tcp" {
		if host, port, err := net.SplitHostPort(summary.GUIAddress); err == nil {
			if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
				host = "127.0.0.1"
			}
			scheme := "http"
			if summary.GUITLS {
				scheme = "https"
			}
			h.GUIURL = scheme + "://" + net.JoinHostPort(host, port) + "/"
		}
	}
	return h
}

// stopSyncthing asks the Syncthing running on a home to shut down, and waits until it has let go of the home
func stopSyncthing(dir string) error {
	if !sthome.LockHeld(dir) {
		return nil
	}
	if err := sthome.RequestShutdown(dir); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Minute)
	for sthome.LockHeld(dir) {
		if time.Now().After(deadline) {
			return errors.New("Syncthing didn't stop within a minute")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}
