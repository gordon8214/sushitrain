// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/syncthing/syncthing/lib/assets"
)

func gzipped(t *testing.T, content string) assets.Asset {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return assets.Asset{Content: buffer.String(), Gzipped: true}
}

func file(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

func TestDifferences(t *testing.T) {
	built := map[string]assets.Asset{
		"default/index.html":     gzipped(t, "<html>"),
		"default/assets/img.png": {Content: "picture"},
	}

	tests := []struct {
		name   string
		folder fstest.MapFS
		want   []string
	}{
		{
			name: "the same files, compressed or not",
			folder: fstest.MapFS{
				"default/index.html":     file("<html>"),
				"default/assets/img.png": file("picture"),
			},
		},
		{
			name: "dotfiles aren't built in, and symbolic links neither",
			folder: fstest.MapFS{
				"default/index.html":     file("<html>"),
				"default/assets/img.png": file("picture"),
				"default/.gitignore":     file("*"),
				"default/link":           {Mode: fs.ModeSymlink},
			},
		},
		{
			name: "a changed file",
			folder: fstest.MapFS{
				"default/index.html":     file("<html lang>"),
				"default/assets/img.png": file("picture"),
			},
			want: []string{"differs: default/index.html"},
		},
		{
			name: "a changed file that isn't compressed",
			folder: fstest.MapFS{
				"default/index.html":     file("<html>"),
				"default/assets/img.png": file("another picture"),
			},
			want: []string{"differs: default/assets/img.png"},
		},
		{
			name: "a new file",
			folder: fstest.MapFS{
				"default/index.html":     file("<html>"),
				"default/assets/img.png": file("picture"),
				"default/new.js":         file("new"),
			},
			want: []string{"not built in: default/new.js"},
		},
		{
			name:   "a removed file",
			folder: fstest.MapFS{"default/index.html": file("<html>")},
			want:   []string{"not in the folder: default/assets/img.png"},
		},
		{
			name:   "an empty folder, which is what a wrong path looks like",
			folder: fstest.MapFS{},
			want:   []string{"not in the folder: default/assets/img.png", "not in the folder: default/index.html"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := differences(built, test.folder)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestDifferencesOnAnAssetThatWontDecompress(t *testing.T) {
	built := map[string]assets.Asset{"index.html": {Content: "not gzip", Gzipped: true}}
	if _, err := differences(built, fstest.MapFS{"index.html": file("<html>")}); err == nil {
		t.Error("expected an error")
	}
}
