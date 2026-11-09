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
	// How far a folder that is syncing has come (nil in any other state)
	Sync *FolderSync `json:",omitempty"`
}

// FolderSync is how much of what the other devices have a syncing folder has here, by size
type FolderSync struct {
	LocalBytes, GlobalBytes, NeedBytes int64
	NeedFiles                          int
	// LocalBytes of GlobalBytes, between 0 and 1
	Fraction float64
}

// FileTransfer is one file being received from or sent to another device
type FileTransfer struct {
	Folder, FolderLabel, Path string
	// The device a file is sent to (empty for one being received)
	Device, DeviceName    string `json:",omitempty"`
	BytesDone, BytesTotal int64
	// BytesDone of BytesTotal, between 0 and 1
	Percentage float64
	// Over the last few seconds (0 while not known)
	BytesPerSecond int64
}

// TransferTotals is every file being received, or every file being sent
type TransferTotals struct {
	FilesTotal            int64
	BytesDone, BytesTotal int64
	Percentage            float64
	// Over the last few seconds, counting files that finished in them (0 while not known)
	BytesPerSecond int64
	// At that speed (-1 while not known)
	SecondsRemaining int64
}

// Transfers is what the engine receives and sends at the moment. The totals are nil when nothing goes that way.
type Transfers struct {
	Download, Upload   *TransferTotals
	Downloads, Uploads []FileTransfer
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
	// A file the folder's ignore patterns leave out, so it never arrives here (absent from engines before it)
	Ignored bool `json:",omitempty"`
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
	IsAdvanced                      bool
}
type Editor struct {
	Token, Section string
	Fields         []Field
}
