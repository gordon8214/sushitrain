// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build darwin

package versionsnapshot

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"os"
	"time"
)

type allAttributes struct{}

func (allAttributes) Permit(name string) bool    { return name != "user.syncthing.platform-history" }
func (allAttributes) GetMaxSingleEntrySize() int { return 0 }
func (allAttributes) GetMaxTotalSize() int       { return 0 }
func timeFromNs(ns int64) time.Time              { return time.Unix(0, ns) }
func Attributes(path string) ([]protocol.Xattr, error) {
	return fs.NewFilesystem(fs.FilesystemTypeBasic, "/").GetXattr(path, allAttributes{})
}
func ApplyAttributes(path string, attrs []protocol.Xattr) error {
	if err := ValidAttributes(attrs); err != nil {
		return err
	}
	filesystem := fs.NewFilesystem(fs.FilesystemTypeBasic, "/")
	if err := filesystem.SetXattr(path, attrs, allAttributes{}); err != nil {
		return err
	}
	actual, err := Attributes(path)
	if err != nil {
		return err
	}
	want := map[string][]byte{}
	for _, a := range attrs {
		want[a.Name] = a.Value
	}
	if len(actual) != len(want) {
		return fmt.Errorf("destination attributes %v do not match archived attributes %v", attributeNames(actual), attributeNames(attrs))
	}
	for _, a := range actual {
		v, ok := want[a.Name]
		if !ok || !bytes.Equal(v, a.Value) {
			return errors.New("destination changed archived metadata")
		}
	}
	return nil
}
func CopyAttributes(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("snapshot is not a regular file")
	}
	attrs, err := Attributes(source)
	if err != nil {
		return err
	}
	return ApplyAttributes(destination, attrs)
}

func attributeNames(attrs []protocol.Xattr) []string {
	var names []string
	for _, a := range attrs {
		names = append(names, a.Name)
	}
	return names
}
