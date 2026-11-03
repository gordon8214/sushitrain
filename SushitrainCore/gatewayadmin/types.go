// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package gatewayadmin holds what synctrain-gateway's admin socket answers, for its clients: the gateway's own
// command-line interface, and Synctrain's window on Linux, which doesn't link the engine.
package gatewayadmin

import "time"

// Status summarizes the service for the host's status displays
type Status struct {
	ServiceID           string         `json:"serviceID"`
	ServiceName         string         `json:"serviceName"`
	DeviceID            string         `json:"deviceID"`
	Address             string         `json:"address"`
	SPKI                string         `json:"spki,omitempty"`
	AuthorityGeneration int64          `json:"authorityGeneration"`
	Exports             []ExportStatus `json:"exports"`
	Clients             int            `json:"clients"`
	PendingOperations   int            `json:"pendingOperations"`
	PendingPairings     int            `json:"pendingPairings"`
	ContentCacheBytes   int64          `json:"contentCacheBytes"`
	ContentCacheQuota   int64          `json:"contentCacheQuota"`
}

type ExportStatus struct {
	Export
	Label string `json:"label"`
	State string `json:"state"`
	Items int64  `json:"items"`
}

type Export struct {
	FolderID   string `json:"folderID"`
	AllowWrite bool   `json:"allowWrite"`
}

type Client struct {
	ClientID    string            `json:"clientID"`
	Name        string            `json:"name"`
	Platform    string            `json:"platform"`
	Created     time.Time         `json:"created"`
	LastSeen    time.Time         `json:"lastSeen"`
	Revoked     bool              `json:"revoked"`
	Permissions map[string]string `json:"permissions"`
}

// PendingOperation describes an operation that has not finished, for the host's display. File names are left out.
type PendingOperation struct {
	Operation string    `json:"operation"`
	ClientID  string    `json:"clientID"`
	Kind      string    `json:"kind"`
	FolderID  string    `json:"folderID"`
	Journal   string    `json:"journal"`
	Created   time.Time `json:"created"`
}

// PairingSession is a started pairing, for showing to the person approving it
type PairingSession struct {
	SessionID string    `json:"sessionID"`
	Secret    string    `json:"secret"`
	URL       string    `json:"url"`
	Expires   time.Time `json:"expires"`
}

// PendingPairing is a client waiting for approval
type PendingPairing struct {
	RequestID  string    `json:"requestID"`
	SessionID  string    `json:"sessionID"`
	ClientID   string    `json:"clientID"`
	ClientName string    `json:"clientName"`
	Platform   string    `json:"platform"`
	Requested  time.Time `json:"requested"`
	// The name of the paired client this request would replace (pairing again with the same identity), if any
	Replaces string `json:"replaces,omitempty"`
}

// The pairing.wait method's answer when no request came while it waited
const NoPairingRequestYet = "no pairing request yet"

// ServiceSettings are the remote file service's settings that can change while the gateway runs (service.set)
type ServiceSettings struct {
	Enabled    bool   `json:"enabled"`
	Listen     string `json:"listen,omitempty"`
	PublicHost string `json:"publicHost,omitempty"`
	PublicPort int    `json:"publicPort,omitempty"`
}

// ServiceStatus is what service.status answers. The service's other methods answer ErrServiceOff while it is off.
type ServiceStatus struct {
	ServiceSettings
	// Whether the service runs now, and why not when it should
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
	// The public key hash pairing codes pin, while it runs
	SPKI string `json:"spki,omitempty"`
	// Settings that serve's flags fix, which service.set can't change
	Fixed []string `json:"fixed,omitempty"`
}

// What the service's methods answer while it is off
const ErrServiceOff = "the remote file service is off"

// Device is a Syncthing device, other than the gateway's own (devices.list)
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Connected bool     `json:"connected"`
	Paused    bool     `json:"paused"`
}

// Folder is a Syncthing folder (folders.list)
type Folder struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Path    string   `json:"path"`
	Type    string   `json:"type"`
	Paused  bool     `json:"paused"`
	State   string   `json:"state"`
	Devices []string `json:"devices"`
}

// Home describes Syncthing's home on this machine, for setting Synchronize mode up (synctrain-gateway home --json)
type Home struct {
	Dir string `json:"dir"`
	// Whether a Syncthing configuration or identity is there, to be taken over
	Exists   bool     `json:"exists"`
	DeviceID string   `json:"deviceID,omitempty"`
	Folders  []string `json:"folders,omitempty"`
	Devices  int      `json:"devices"`
	// Whether synctrain-gateway has run on it, which took it over (or made it) already
	Taken bool `json:"taken"`
	// Whether a Syncthing runs on it now
	Locked bool `json:"locked"`
	// Why it can't be taken over, when it can't
	Problem string `json:"problem,omitempty"`
	// The admin socket of a gateway running on it
	Socket string `json:"socket"`
	// Its web GUI, when it has one on this machine
	GUIURL string `json:"guiURL,omitempty"`
}
