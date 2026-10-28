// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux || darwin

package gateway

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// dir is an open directory inside a folder. Mutations go through directory file descriptors and never follow
// symbolic links, so that a link swapped in by another process cannot redirect them outside the folder; string
// prefix checks on paths would not prevent that.
type dir struct {
	fd   int
	path string
}

var errSymlink = errors.New("path goes through a symbolic link")

// openRoot opens a folder's directory
func openRoot(path string) (*dir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &dir{fd: fd, path: path}, nil
}

// openDir opens the directory at the folder-relative path rel below d, refusing symbolic links on the way
func (d *dir) openDir(rel string) (*dir, error) {
	current := &dir{fd: -1, path: d.path}
	fd := d.fd
	if rel != "" {
		for component := range strings.SplitSeq(rel, "/") {
			if component == "" || component == "." || component == ".." {
				current.close()
				return nil, fmt.Errorf("invalid path component %q", component)
			}
			next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				current.close()
				if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) && d.isSymlink(fd, component) {
					return nil, errSymlink
				}
				return nil, &os.PathError{Op: "openat", Path: rel, Err: err}
			}
			current.close()
			current = &dir{fd: next, path: current.path + "/" + component}
			fd = next
		}
	}
	if current.fd == -1 {
		dup, err := unix.Dup(d.fd)
		if err != nil {
			return nil, err
		}
		unix.CloseOnExec(dup)
		return &dir{fd: dup, path: d.path}, nil
	}
	return current, nil
}

func (d *dir) isSymlink(fd int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}

func (d *dir) close() {
	if d != nil && d.fd >= 0 {
		unix.Close(d.fd)
		d.fd = -1
	}
}

type fileIdentity struct {
	dev, ino uint64
}

// entryStat is what the coordinator needs to know about a directory entry
type entryStat struct {
	exists  bool
	kind    uint32 // unix.S_IFREG, S_IFDIR, S_IFLNK, …
	size    int64
	modTime time.Time
	id      fileIdentity
}

func (d *dir) lstat(name string) (entryStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryStat{}, nil
		}
		return entryStat{}, err
	}
	return entryStat{
		exists:  true,
		kind:    uint32(st.Mode) & unix.S_IFMT,
		size:    st.Size,
		modTime: time.Unix(statMtime(&st)),
		id:      fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)},
	}, nil
}

// createExclusive creates a new file in d, failing if the name exists
func (d *dir) createExclusive(name string, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), d.path+"/"+name), nil
}

// openFile opens an existing regular file in d for reading
func (d *dir) openFile(name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), d.path+"/"+name), nil
}

func (d *dir) mkdir(name string, mode uint32) error {
	return unix.Mkdirat(d.fd, name, mode)
}

func (d *dir) remove(name string) error {
	return unix.Unlinkat(d.fd, name, 0)
}

func (d *dir) chmod(name string, mode uint32) error {
	return unix.Fchmodat(d.fd, name, mode, 0)
}

func (d *dir) chtimes(name string, mtime time.Time) error {
	ts := unix.NsecToTimespec(mtime.UnixNano())
	return unix.UtimesNanoAt(d.fd, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

func (d *dir) sync() error {
	return unix.Fsync(d.fd)
}

// renameNoReplace renames from (in fromDir) to to (in toDir), failing with EEXIST when to exists
func renameNoReplace(fromDir *dir, from string, toDir *dir, to string) error {
	err := renameNoReplaceSyscall(fromDir.fd, from, toDir.fd, to)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	// The filesystem has no exclusive rename. A hard link is exclusive for files; directories are checked first,
	// which only another process changing the folder at the same moment could race.
	var st unix.Stat_t
	if err := unix.Fstatat(fromDir.fd, from, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Linkat(fromDir.fd, from, toDir.fd, to, 0); err != nil {
			return err
		}
		return unix.Unlinkat(fromDir.fd, from, 0)
	}
	if err := unix.Fstatat(toDir.fd, to, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	}
	return unix.Renameat(fromDir.fd, from, toDir.fd, to)
}

// renameExchange atomically swaps two entries. errExchangeUnsupported means the filesystem cannot.
func renameExchange(aDir *dir, a string, bDir *dir, b string) error {
	err := renameExchangeSyscall(aDir.fd, a, bDir.fd, b)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return errExchangeUnsupported
	}
	return err
}

var errExchangeUnsupported = errors.New("the filesystem cannot swap entries")

func isExists(err error) bool {
	return errors.Is(err, unix.EEXIST) || errors.Is(err, os.ErrExist)
}
