// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type linuxTrash struct{}

func (linuxTrash) Trash(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return trashFromRoot(context.Background(), root, filepath.Base(path))
}
func privateTrash(path string) (*os.Root, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return privateTrashAt(parent, filepath.Base(path))
}
func privateTrashAt(parent *os.File, name string) (*os.Root, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "trash")
	defer f.Close()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Uid != uint32(os.Getuid()) || stat.Mode&0077 != 0 {
		return nil, errors.New("the Trash directory must be private and owned by this user")
	}
	return os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
}
func trashDirectory(root *os.Root, name string) (*os.File, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	child, err := privateTrashAt(parent, name)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	return child.Open(".")
}
func mountIdentity(f *os.File) (uint64, error) {
	var stat unix.Statx_t
	err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stat)
	if err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("mount identity is unavailable")
	}
	return stat.Mnt_id, nil
}
func mountRoot(parent *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(parent.Fd()))
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "trash-volume")
	for {
		var here, up unix.Stat_t
		if err = unix.Fstat(int(current.Fd()), &here); err != nil {
			current.Close()
			return nil, err
		}
		nextFD, err := unix.Openat(int(current.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			current.Close()
			return nil, err
		}
		next := os.NewFile(uintptr(nextFD), "trash-volume-parent")
		if err = unix.Fstat(nextFD, &up); err != nil {
			next.Close()
			current.Close()
			return nil, err
		}
		hereMount, e := mountIdentity(current)
		if e != nil {
			next.Close()
			current.Close()
			return nil, e
		}
		upMount, e := mountIdentity(next)
		if e != nil {
			next.Close()
			current.Close()
			return nil, e
		}
		if hereMount != upMount || here.Dev != up.Dev || here.Ino == up.Ino {
			next.Close()
			return current, nil
		}
		current.Close()
		current = next
	}
}

// trashFromRoot publishes a freedesktop Trash entry without reopening the source
// through an absolute path. An unavailable same-volume Trash never becomes deletion.
func trashFromRoot(ctx context.Context, root *os.Root, name string) error {
	sourceRoot, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	source, err := sourceRoot.Open(".")
	if err != nil {
		return err
	}
	defer source.Close()
	sourcePath, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", source.Fd()))
	if err != nil {
		return err
	}
	if strings.HasSuffix(sourcePath, " (deleted)") {
		return errors.New("the source directory was removed")
	}
	original := filepath.Join(sourcePath, filepath.Base(name))
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		data = filepath.Join(home, ".local", "share")
	}
	homeTrash, err := privateTrash(filepath.Join(data, "Trash"))
	if err != nil {
		return err
	}
	err = publishTrash(ctx, homeTrash, source, filepath.Base(name), original)
	homeTrash.Close()
	if !errors.Is(err, unix.EXDEV) {
		return err
	}
	volume, err := mountRoot(source)
	if err != nil {
		return err
	}
	defer volume.Close()
	var volumeTrash *os.Root
	sharedFD, sharedErr := unix.Openat(int(volume.Fd()), ".Trash", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if sharedErr == nil {
		shared := os.NewFile(uintptr(sharedFD), "shared-trash")
		var stat unix.Stat_t
		if unix.Fstat(sharedFD, &stat) == nil && stat.Mode&unix.S_ISVTX != 0 {
			volumeTrash, err = privateTrashAt(shared, fmt.Sprint(os.Getuid()))
		}
		shared.Close()
	}
	if volumeTrash == nil {
		volumeTrash, err = privateTrashAt(volume, fmt.Sprintf(".Trash-%d", os.Getuid()))
	}
	if err != nil {
		return err
	}
	defer volumeTrash.Close()
	top, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", volume.Fd()))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(top, original)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return errors.New("the volume's Trash cannot represent this file")
	}
	return publishTrash(ctx, volumeTrash, source, filepath.Base(name), relative)
}
func publishTrash(ctx context.Context, trash *os.Root, source *os.File, name, original string) error {
	files, err := trashDirectory(trash, "files")
	if err != nil {
		return err
	}
	defer files.Close()
	info, err := trashDirectory(trash, "info")
	if err != nil {
		return err
	}
	defer info.Close()
	short := name
	for len(short) > 180 {
		_, n := utf8.DecodeLastRuneInString(short)
		short = short[:len(short)-n]
	}
	id := uuid.NewString() + "-" + short
	metadata, err := trash.OpenFile("info/"+id+".trashinfo", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	escaped := (&url.URL{Path: original}).EscapedPath()
	_, err = fmt.Fprintf(metadata, "[Trash Info]\nPath=%s\nDeletionDate=%s\n", escaped, time.Now().Format("2006-01-02T15:04:05"))
	err = errors.Join(err, metadata.Sync(), metadata.Close(), info.Sync())
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = unix.Renameat2(int(source.Fd()), name, int(files.Fd()), id, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		trash.Remove("info/" + id + ".trashinfo")
		return err
	}
	return errors.Join(files.Sync(), source.Sync())
}
