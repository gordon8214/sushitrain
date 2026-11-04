// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build darwin

package versionsnapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	for _, attrs := range [][]protocol.Xattr{{}, {
		{Name: "com.apple.ResourceFork", Value: bytes.Repeat([]byte{0, 1, 255, 42}, 2<<20)},
		{Name: "com.apple.FinderInfo", Value: append([]byte("TEXTttxt"), make([]byte, 24)...)},
		{Name: "com.synctrain.binary", Value: []byte{0, 255, 1}},
	}} {
		root := t.TempDir()
		name := filepath.Join(root, "source")
		if err := os.WriteFile(name, []byte("data fork"), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := fs.NewFilesystem(fs.FilesystemTypeBasic, root).Open("source")
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if err := Write(context.Background(), &buffer, file, protocol.PlatformData{Darwin: &protocol.XattrData{Xattrs: attrs}}, "folder", "Café/file", "ver_12"); err != nil {
			t.Fatal(err)
		}
		file.Close()
		destination := filepath.Join(root, "restored")
		if _, err := Extract(context.Background(), bytes.NewReader(buffer.Bytes()), destination, "folder", "Café/file", "ver_12"); err != nil {
			t.Fatal(err)
		}
		actual, err := Attributes(destination)
		if err != nil {
			t.Fatal(err)
		}
		values := map[string][]byte{}
		for _, a := range actual {
			values[a.Name] = a.Value
		}
		for _, a := range attrs {
			if !bytes.Equal(values[a.Name], a.Value) {
				t.Fatalf("lost %s", a.Name)
			}
		}
		if len(values) != len(attrs) {
			t.Fatalf("unexpected attributes: %v", values)
		}
		bad := filepath.Join(root, "bad")
		if _, err := Extract(context.Background(), bytes.NewReader(buffer.Bytes()), bad, "folder", "other", "ver_12"); err == nil {
			t.Fatal("accepted wrong path")
		}
		if _, err := os.Stat(bad); !os.IsNotExist(err) {
			t.Fatal("published invalid snapshot")
		}
		corrupt := append([]byte(nil), buffer.Bytes()...)
		location := bytes.Index(corrupt, []byte("data fork"))
		corrupt[location] = '!'
		if _, err := Extract(context.Background(), bytes.NewReader(corrupt), bad, "folder", "Café/file", "ver_12"); err == nil {
			t.Fatal("accepted corrupt content")
		}
		if _, err := os.Stat(bad); !os.IsNotExist(err) {
			t.Fatal("kept incomplete snapshot")
		}
	}
}
func TestSnapshotRejectsUnexpectedTarEntry(t *testing.T) {
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	tw.WriteHeader(&tar.Header{Name: "../escape", Size: 1, Mode: 0600})
	tw.Write([]byte("x"))
	tw.Close()
	if _, err := Extract(context.Background(), &buffer, filepath.Join(t.TempDir(), "bad"), "f", "p", "v"); err == nil {
		t.Fatal("accepted traversal")
	}
}
func TestSnapshotMetadataValidator(t *testing.T) {
	a := protocol.PlatformData{Darwin: &protocol.XattrData{Xattrs: []protocol.Xattr{{Name: "tag", Value: []byte("a")}}}}
	b := a
	b.Darwin = &protocol.XattrData{Xattrs: []protocol.Xattr{{Name: "tag", Value: []byte("b")}}}
	x, _ := Tag("content", a)
	y, _ := Tag("content", b)
	if reflect.DeepEqual(x, y) {
		t.Fatal("metadata-only edit retained validator")
	}
	if _, err := Tag("content", protocol.PlatformData{}); err == nil {
		t.Fatal("missing capture was treated as empty")
	}
	if err := ValidAttributes([]protocol.Xattr{{Name: "tag"}, {Name: "tag"}}); err == nil {
		t.Fatal("accepted duplicate attribute")
	}
}
