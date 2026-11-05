// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

func (entry *Entry) DesktopIdentity() string {
	if !entry.IsLocallyPresent() {
		return ""
	}
	path, err := entry.LocalNativePath()
	if err != nil {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", historyFileIdentity(info), info.Size(), info.ModTime().UnixNano())
}
func renameRootExclusive(root *os.Root, from, to string) error {
	source, err := root.OpenRoot(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := root.OpenRoot(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer target.Close()
	a, err := source.Open(".")
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := target.Open(".")
	if err != nil {
		return err
	}
	defer b.Close()
	return unix.Renameat2(int(a.Fd()), filepath.Base(from), int(b.Fd()), filepath.Base(to), unix.RENAME_NOREPLACE)
}

// DesktopFileAction performs user-requested changes in the folder coordinator,
// retaining directory handles and refusing replacement of an existing target.
func (f *Folder) DesktopFileAction(ctx context.Context, action, name, target, source, expected string) error {
	if !f.CanRestoreDownloadedVersion() || !finderPathAllowed(name) {
		return errors.New("this folder cannot be changed")
	}
	return f.client.internals().RunInFolderLoop(ctx, f.FolderID, func(ctx context.Context, h syncthing.FolderHandle) error {
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
			parent, e := root.OpenRoot(filepath.Dir(name))
			if e != nil {
				return e
			}
			defer parent.Close()
			input, e := safecopy.OpenRegular(source)
			if e != nil {
				return e
			}
			defer input.Close()
			info, e := input.Stat()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return errors.New("choose a regular file to import")
			}
			temporary := ".syncthing.import-" + uuid.NewString() + ".tmp"
			output, e := parent.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			defer output.Close()
			owned, e := output.Stat()
			if e != nil {
				return e
			}
			defer func() {
				if current, err := parent.Lstat(temporary); err == nil && os.SameFile(owned, current) {
					parent.Remove(temporary)
				}
			}()
			_, e = io.Copy(output, historyContextReader{ctx, input})
			e = errors.Join(e, output.Sync())
			if e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			err = publishDesktopImport(parent, output, filepath.Base(name))
		case "rename", "move", "trashFile", "deleteFile":
			if expected == "" || action != "trashFile" && action != "deleteFile" && (!finderPathAllowed(target) || h.IsIgnored(target)) {
				return errors.New("refresh this item before changing it")
			}
			if action == "rename" && filepath.Dir(target) != filepath.Dir(name) {
				return errors.New("enter a file name without a directory")
			}
			if e := osutil.TraversesSymlink(h.Filesystem(), filepath.Dir(target)); e != nil {
				return e
			}
			info, e := root.Lstat(name)
			if e != nil {
				return e
			}
			actual := fmt.Sprintf("%s:%d:%d", historyFileIdentity(info), info.Size(), info.ModTime().UnixNano())
			if actual != expected {
				return errors.New("this file changed; refresh before changing it")
			}
			if action == "deleteFile" {
				err = h.Filesystem().Remove(name)
			} else if action == "trashFile" {
				err = trashFromRoot(ctx, root, name)
			} else {
				err = renameRootExclusive(root, name, target)
			}
		default:
			return errors.New("unknown file operation")
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

func publishDesktopImport(parent *os.Root, output *os.File, name string) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	if err = unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", output.Fd()), int(directory.Fd()), name, unix.AT_SYMLINK_FOLLOW); err != nil {
		return err
	}
	return directory.Sync()
}

// DesktopExportVersion keeps native Save a Copy exclusive and cancellable.
func (f *Folder) DesktopExportVersion(ctx context.Context, name string, v *FileVersion, destination string) error {
	return f.DesktopExportVersionChecked(ctx, name, v, destination, "")
}
