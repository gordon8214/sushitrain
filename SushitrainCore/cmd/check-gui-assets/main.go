// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Command check-gui-assets compares the web GUI built into the engine with a checkout of the fork's gui folder.
//
// The pages come from the fork's generated lib/api/auto/gui.files.go, not from gui/, so a commit that changes gui/ without
// regenerating that file would bring the old pages along unnoticed. Scripts/update-fork.sh runs this on the commit it
// moves to. Module zips leave out gui/default/vendor, so the folder has to come from the fork's repository.
//
//	go run ./cmd/check-gui-assets <gui folder>
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/syncthing/syncthing/lib/api/auto"
	"github.com/syncthing/syncthing/lib/assets"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: check-gui-assets <gui folder>")
		os.Exit(2)
	}

	built := auto.Assets()
	found, err := differences(built, os.DirFS(os.Args[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	for _, difference := range found {
		fmt.Fprintln(os.Stderr, difference)
	}
	if len(found) > 0 {
		os.Exit(1)
	}
	fmt.Printf("All %d files of the web GUI match.\n", len(built))
}

// differences returns what sets the built-in assets apart from the folder, sorted.
func differences(built map[string]assets.Asset, folder fs.FS) ([]string, error) {
	var found []string
	seen := make(map[string]bool, len(built))

	err := fs.WalkDir(folder, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// What genassets skips: it leaves out dotfiles, and anything that isn't a regular file, such as a symbolic link
		if !entry.Type().IsRegular() || strings.HasPrefix(path.Base(name), ".") {
			return nil
		}

		asset, ok := built[name]
		if !ok {
			found = append(found, "not built in: "+name)
			return nil
		}
		seen[name] = true

		inFolder, err := fs.ReadFile(folder, name)
		if err != nil {
			return err
		}
		inEngine, err := content(asset)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !bytes.Equal(inFolder, inEngine) {
			found = append(found, "differs: "+name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for name := range built {
		if !seen[name] {
			found = append(found, "not in the folder: "+name)
		}
	}
	slices.Sort(found)
	return found, nil
}

func content(asset assets.Asset) ([]byte, error) {
	if !asset.Gzipped {
		return []byte(asset.Content), nil
	}
	reader, err := gzip.NewReader(strings.NewReader(asset.Content))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
