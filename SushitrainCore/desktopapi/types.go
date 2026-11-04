// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package desktopapi contains the engine-free types used by native desktop clients.
package desktopapi

type Request struct {
	ID, Folder, Device, Path, Name, Action, Password, Section, Token string
	Enabled                                                          bool
	Offset                                                           int64
	Limit                                                            int64
	Values                                                           map[string]string
}
type Folder struct {
	ID, Label, Path, Type, State, Error string
	Paused, CanRemoveFiles              bool
	Failed                              int
	Devices                             []string
}
type Device struct {
	ID, Name, Address string
	Connected, Paused bool
}
type Entry struct {
	Path, Name, MIME, URL, NativePath string
	Identity                          string
	Size                              int64
	Directory, Local                  bool
}
type Version struct {
	ID                       string
	Revision                 string
	Size, Archived, Modified int64
}

type Field struct {
	Path, Label, Group, Kind, Value string
	IsSecret, IsReadOnly            bool
	Options                         []string
}
type Editor struct {
	Token, Section string
	Fields         []Field
}
