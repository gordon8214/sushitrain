// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

// What synctrain-files answers on its admin socket, beyond the service's own types, for its command-line interface
// and Synctrain's window

// Status is what the status command shows
type Status struct {
	Version  string    `json:"version"`
	Mount    string    `json:"mount"`
	Gateways []Gateway `json:"gateways"`
	Folders  []Folder  `json:"folders"`
}

// RemoteFolder is a folder a gateway lets this client access, and whether it is here
type RemoteFolder struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Write     bool   `json:"write"`
	Added     bool   `json:"added"`
	DirName   string `json:"dirName,omitempty"`
	Selective bool   `json:"selective,omitempty"`
}

// Removal is where files that were only here went when their folder was removed
type Removal struct {
	Leftovers string `json:"leftovers,omitempty"`
	Moved     int    `json:"moved"`
}

// ItemState is the state of an item in the mount
type ItemState struct {
	Path  string `json:"path"`
	State string `json:"state"`
	Keep  bool   `json:"keep"`
}
