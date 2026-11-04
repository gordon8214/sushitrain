// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !linux

package sushitrain

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

func (entry *Entry) DesktopIdentity() string {
	if !entry.IsLocallyPresent() {
		return ""
	}
	name, err := entry.LocalNativePath()
	if err != nil {
		return ""
	}
	info, err := os.Lstat(name)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", historyFileIdentity(info), info.Size(), info.ModTime().UnixNano())
}
func (f *Folder) DesktopExportVersion(ctx context.Context, name string, v *FileVersion, destination string) error {
	return f.DesktopExportVersionChecked(ctx, name, v, destination, "")
}

// DesktopFileAction uses the same engine folder loop and containment checks as
// restores, so scanning and pulling cannot interleave with a native mutation.
func (f *Folder) DesktopFileAction(ctx context.Context, action, name, target, source, expected string) error {
	if !f.CanRestoreDownloadedVersion() || !finderPathAllowed(name) {
		return errors.New("this folder cannot be changed")
	}
	internals := f.client.internals()
	if internals == nil {
		return ErrStillLoading
	}
	return internals.RunInFolderLoop(ctx, f.FolderID, func(ctx context.Context, h syncthing.FolderHandle) error {
		base, err := historyHandleRoot(h)
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return err
		}
		defer root.Close()
		if h.IsIgnored(name) {
			return errors.New("the destination is excluded by ignore patterns")
		}
		if err = osutil.TraversesSymlink(h.Filesystem(), filepath.Dir(name)); err != nil {
			return err
		}
		switch action {
		case "mkdir":
			err = root.Mkdir(name, 0755)
		case "import":
			input, e := safecopy.OpenRegular(source)
			if e != nil {
				return e
			}
			defer input.Close()
			parent, e := root.OpenRoot(filepath.Dir(name))
			if e != nil {
				return e
			}
			defer parent.Close()
			temporary := ".syncthing.import-" + rand.Text() + ".tmp"
			output, e := parent.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			defer output.Close()
			defer parent.Remove(temporary)
			if _, e = safecopy.Copy(ctx, output, input); e != nil {
				return e
			}
			if e = output.Sync(); e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			held, e := output.Stat()
			if e != nil {
				return e
			}
			actual, e := parent.Lstat(temporary)
			if e != nil {
				return e
			}
			if !os.SameFile(held, actual) {
				return errors.New("import temporary file changed")
			}
			err = parent.Link(temporary, filepath.Base(name))

		case "rename", "move":
			if expected == "" || !finderPathAllowed(target) || h.IsIgnored(target) {
				return errors.New("refresh this file before moving it")
			}
			if err = osutil.TraversesSymlink(h.Filesystem(), filepath.Dir(target)); err != nil {
				return err
			}
			info, e := root.Lstat(name)
			if e != nil {
				return e
			}
			actual := fmt.Sprintf("%s:%d:%d", historyFileIdentity(info), info.Size(), info.ModTime().UnixNano())
			if actual != expected {
				return errors.New("this file changed; refresh before moving it")
			}
			err = desktopRenameExclusive(root, name, target)
		default:
			return errors.New("unsupported native file operation")
		}
		if err != nil {
			return err
		}
		subs := []string{name}
		if target != "" {
			subs = append(subs, target)
		}
		return h.Scan(ctx, subs)
	})
}
