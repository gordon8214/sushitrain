// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build darwin

package sushitrain

import (
	"bytes"
	"errors"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"golang.org/x/sys/unix"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
)

// Capture metadata from the same descriptor as the selected archive's bytes.
// A renamed or replaced archive pathname cannot redirect this read.
func (fld *Folder) archiveAttributes(file fs.File) (func(string) error, error) {
	// The engine's metrics wrapper intentionally hides descriptors. Retain a
	// second regular-file handle solely for metadata and match it to the actual
	// selected input before and after reading attributes.
	cfg := fld.folderConfiguration()
	if cfg == nil {
		return nil, errFolderConfigNotFound
	}
	base := cfg.Versioning.FSPath
	if base == "" {
		base = filepath.Join(cfg.Filesystem().URI(), ".stversions")
	} else {
		expanded, err := fs.ExpandTilde(base)
		if err != nil {
			return nil, err
		}
		base = expanded
		if !filepath.IsAbs(base) {
			base = filepath.Join(cfg.Filesystem().URI(), base)
		}
	}
	metadata, err := safecopy.OpenRegular(filepath.Join(base, file.Name()))
	if err != nil {
		return nil, err
	}
	defer metadata.Close()
	selected, err := file.Stat()
	if err != nil {
		return nil, err
	}
	same := func() error {
		actual, err := metadata.Stat()
		if err != nil {
			return err
		}
		if historyFileIdentityFromSnapshot(actual.Sys()) != historyFileIdentityFromSnapshot(selected.Sys()) || actual.Size() != selected.Size() || !actual.ModTime().Equal(selected.ModTime()) {
			return errors.New("the selected archive metadata changed")
		}
		return nil
	}
	if err := same(); err != nil {
		return nil, err
	}
	fd := int(metadata.Fd())
	size, err := unix.Flistxattr(fd, nil)
	if err != nil {
		return nil, err
	}
	if size > versionsnapshot.MaxManifestBytes {
		return nil, errors.New("archive attribute names exceed the metadata limit")
	}
	names := make([]byte, size)
	n, err := unix.Flistxattr(fd, names)
	if err != nil {
		return nil, err
	}
	attrs := []protocol.Xattr{}
	total := 0
	for _, raw := range bytes.Split(names[:n], []byte{0}) {
		name := string(raw)
		if name == "" || name == "user.syncthing.platform-history" {
			continue
		}
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		total += size
		if total > versionsnapshot.MaxAttributeBytes || len(attrs) >= versionsnapshot.MaxAttributes {
			return nil, errors.New("archive attributes exceed the metadata limit")
		}
		value := make([]byte, size)
		n, err := unix.Fgetxattr(fd, name, value)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, protocol.Xattr{Name: name, Value: value[:n]})
	}
	if err := versionsnapshot.ValidAttributes(attrs); err != nil {
		return nil, err
	}
	if err := same(); err != nil {
		return nil, err
	}
	return func(destination string) error { return versionsnapshot.ApplyAttributes(destination, attrs) }, nil
}
