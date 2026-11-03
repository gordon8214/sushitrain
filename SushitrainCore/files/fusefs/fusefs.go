// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

// Package fusefs shows the folders of a files.Service as a FUSE filesystem: a directory per folder at the root,
// files that are fetched as they are read, and the kernel's caches dropped as the gateways' changes arrive.
package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
	"t-shaped.nl/sushitrain/v2/files"
)

const (
	// Extended attributes: the state of an item's content, and whether it is kept on this device ("1" or "0")
	xattrState = "user.synctrain.state"
	xattrKeep  = "user.synctrain.keep"

	// The kernel keeps names and attributes this long; changes from the gateways drop them earlier
	cacheTimeout    = time.Hour
	negativeTimeout = 10 * time.Second
)

// trackerIgnore is the content of the root's .trackerignore
const trackerIgnore = "Synctrain: files here are fetched from a gateway when read, so they are not indexed.\n"

// readerNames are programs that read every file they are shown (thumbnailers, metadata extractors): they may read
// content that is here, but are refused content that would have to be fetched, so that browsing doesn't download
// everything. Matched against the start of the name of the opening process's executable.
var readerNames = []string{
	"gdk-pixbuf-thumbnailer", "glycin-thumbnailer", "totem-video-thumbnailer", "ffmpegthumbnailer",
	"evince-thumbnailer", "papers-thumbnailer", "atril-thumbnailer", "xreader-thumbnailer", "gnome-thumbnail-font",
	"gsf-office-thumbnailer", "gnome-epub-thumbnailer", "gnome-mobi-thumbnailer", "tumblerd", "localsearch-extractor",
	"tracker-extract", "tracker-miner-fs", "localsearch-3",
}

// Options of the mount
type Options struct {
	Logger *slog.Logger
	// Log every FUSE request
	Debug bool
}

// Mount is a mounted service
type Mount struct {
	server  *fuse.Server
	service *files.Service
	log     *slog.Logger
	dir     string

	// When it was mounted: the time of the mount's root, which is the daemon's own
	mounted time.Time

	mutex sync.Mutex
	nodes map[uint64]*fs.Inode
}

// MountService mounts the service at dir, which is created if needed. A mount left by a daemon that stopped without
// unmounting is unmounted first.
func MountService(dir string, service *files.Service, opts Options) (*Mount, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if _, err := os.Stat(dir); errors.Is(err, syscall.ENOTCONN) {
		opts.Logger.Info("Unmounting a mount left behind", "path", dir)
		if out, err := exec.Command("fusermount3", "-uz", dir).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("unmounting the stale mount at %s: %v: %s", dir, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Mount{service: service, log: opts.Logger, dir: dir, mounted: time.Now(), nodes: map[uint64]*fs.Inode{}}
	root := &rootDir{m: m}
	entryTimeout, attrTimeout, negative := cacheTimeout, cacheTimeout, negativeTimeout
	server, err := fs.Mount(dir, root, &fs.Options{
		// Without default_permissions: the mount is its owner's alone (no allow_other), and the daemon decides what
		// can be written, so that the keep attribute can be set on a file that can't be written
		MountOptions: fuse.MountOptions{
			FsName: "synctrain",
			Name:   "synctrain",
			Debug:  opts.Debug,
		},
		EntryTimeout:    &entryTimeout,
		AttrTimeout:     &attrTimeout,
		NegativeTimeout: &negative,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		RootStableAttr:  &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR},
	})
	if err != nil {
		return nil, err
	}
	m.server = server
	m.remember(1, root.EmbeddedInode())
	service.SetInvalidator(m.invalidate)
	return m, nil
}

// Wait waits until the mount is unmounted (by Unmount, or from outside)
func (m *Mount) Wait() {
	m.server.Wait()
}

// Unmount unmounts, failing while files in the mount are open
func (m *Mount) Unmount() error {
	m.service.SetInvalidator(nil)
	return m.server.Unmount()
}

func (m *Mount) remember(node uint64, inode *fs.Inode) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.nodes[node] = inode
}

func (m *Mount) forget(node uint64, inode *fs.Inode) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.nodes[node] == inode {
		delete(m.nodes, node)
	}
}

func (m *Mount) inode(node uint64) *fs.Inode {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.nodes[node]
}

// invalidate drops what the kernel cached of names and attributes that changed. Directories the kernel doesn't know
// have nothing to drop.
func (m *Mount) invalidate(c files.Changes) {
	for _, e := range c.Entries {
		parent := m.inode(e.Parent)
		if parent == nil {
			continue
		}
		if child := m.inode(e.Node); e.Node != 0 && child != nil {
			// Also tells inotify watchers that it is gone
			parent.NotifyDelete(e.Name, child)
		} else {
			parent.NotifyEntry(e.Name)
		}
	}
	for _, node := range c.Attrs {
		if inode := m.inode(node); inode != nil {
			inode.NotifyContent(-1, 0)
		}
	}
}

// errno is the error a request answers for err
func errno(err error) syscall.Errno {
	var e syscall.Errno
	switch {
	case err == nil:
		return 0
	case errors.As(err, &e):
		return e
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, files.ErrVersionChanged):
		return syscall.ESTALE
	default:
		return syscall.EIO
	}
}

// attributes fills out with what the mount shows of an entry. The mount can't be written yet, so nothing is writable.
func attributes(e files.Entry, out *fuse.Attr) {
	out.Ino = e.Node
	mtime := e.ModTime
	out.SetTimes(&mtime, &mtime, &mtime)
	if e.IsDir() {
		out.Mode = fuse.S_IFDIR | 0o555
		out.Nlink = 2
		return
	}
	out.Mode = fuse.S_IFREG | 0o444
	out.Nlink = 1
	out.Size = uint64(e.Size)
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 128 << 10
}

func (m *Mount) child(ctx context.Context, parent *fs.Inode, e files.Entry, out *fuse.EntryOut) *fs.Inode {
	attributes(e, &out.Attr)
	var node fs.InodeEmbedder
	mode := uint32(fuse.S_IFREG)
	if e.IsDir() {
		node, mode = &dir{m: m, item: e.Item, node: e.Node}, fuse.S_IFDIR
	} else {
		node = &file{m: m, entry: e}
	}
	inode := parent.NewInode(ctx, node, fs.StableAttr{Mode: mode, Ino: e.Node})
	m.remember(e.Node, inode)
	return inode
}

func (m *Mount) readdir(entries []files.Entry, extra ...fuse.DirEntry) fs.DirStream {
	list := slices.Clone(extra)
	for _, e := range entries {
		mode := uint32(fuse.S_IFREG)
		if e.IsDir() {
			mode = fuse.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: mode, Ino: e.Node})
	}
	return fs.NewListDirStream(list)
}

// rootDir is the mount's root: a directory per folder, and .trackerignore
type rootDir struct {
	fs.Inode
	m *Mount
}

var (
	_ fs.NodeLookuper  = (*rootDir)(nil)
	_ fs.NodeReaddirer = (*rootDir)(nil)
	_ fs.NodeGetattrer = (*rootDir)(nil)
	_ fs.NodeAccesser  = (*rootDir)(nil)
)

// access answers access(2), which the kernel asks the mount about without default_permissions: nothing in it can be
// written yet
func access(mask uint32) syscall.Errno {
	if mask&unix.W_OK != 0 {
		return syscall.EACCES
	}
	return 0
}

func (r *rootDir) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(mask)
}

func (r *rootDir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	out.Ino = 1
	out.Nlink = 2
	out.SetTimes(&r.m.mounted, &r.m.mounted, &r.m.mounted)
	return 0
}

func (r *rootDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name == ".trackerignore" {
		attr := fuse.Attr{Mode: fuse.S_IFREG | 0o444, Nlink: 1, Size: uint64(len(trackerIgnore))}
		attr.SetTimes(&r.m.mounted, &r.m.mounted, &r.m.mounted)
		out.Attr = attr
		return r.NewInode(ctx, &fs.MemRegularFile{Data: []byte(trackerIgnore), Attr: attr},
			fs.StableAttr{Ino: 2 << 62}), 0
	}
	e, ok, err := r.m.service.Lookup(0, name)
	if err != nil {
		return nil, errno(err)
	}
	if !ok {
		return nil, syscall.ENOENT
	}
	return r.m.child(ctx, r.EmbeddedInode(), e, out), 0
}

func (r *rootDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	folders, err := r.m.service.Children(0)
	if err != nil {
		return nil, errno(err)
	}
	return r.m.readdir(folders, fuse.DirEntry{Name: ".trackerignore", Mode: fuse.S_IFREG, Ino: 2 << 62}), 0
}

// dir is a folder's root or a directory in it
type dir struct {
	fs.Inode
	m    *Mount
	item int64
	node uint64
}

var (
	_ fs.NodeLookuper      = (*dir)(nil)
	_ fs.NodeReaddirer     = (*dir)(nil)
	_ fs.NodeGetattrer     = (*dir)(nil)
	_ fs.NodeGetxattrer    = (*dir)(nil)
	_ fs.NodeSetxattrer    = (*dir)(nil)
	_ fs.NodeListxattrer   = (*dir)(nil)
	_ fs.NodeOnForgetter   = (*dir)(nil)
	_ fs.NodeRemovexattrer = (*dir)(nil)
	_ fs.NodeAccesser      = (*dir)(nil)
)

func (d *dir) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(mask)
}

func (d *dir) OnForget() {
	d.m.forget(d.node, d.EmbeddedInode())
}

func (d *dir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	e, ok, err := d.m.service.Item(d.item)
	if err != nil {
		return errno(err)
	}
	if !ok {
		return syscall.ENOENT
	}
	attributes(e, &out.Attr)
	return 0
}

func (d *dir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	e, ok, err := d.m.service.Lookup(d.item, name)
	if err != nil {
		return nil, errno(err)
	}
	if !ok {
		return nil, syscall.ENOENT
	}
	return d.m.child(ctx, d.EmbeddedInode(), e, out), 0
}

func (d *dir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, err := d.m.service.Children(d.item)
	if err != nil {
		return nil, errno(err)
	}
	return d.m.readdir(children), 0
}

func (d *dir) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return d.m.getxattr(d.item, attr, dest)
}

func (d *dir) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	return d.m.setxattr(d.item, attr, data)
}

func (d *dir) Removexattr(ctx context.Context, attr string) syscall.Errno {
	return d.m.setxattr(d.item, attr, []byte("0"))
}

func (d *dir) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	return listxattr(dest)
}

// file is a file as it was when looked up: a change of its content on the gateway gives the path a new node, and
// this one keeps reading what it had
type file struct {
	fs.Inode
	m     *Mount
	entry files.Entry
}

var (
	_ fs.NodeOpener        = (*file)(nil)
	_ fs.NodeReader        = (*file)(nil)
	_ fs.NodeReleaser      = (*file)(nil)
	_ fs.NodeGetattrer     = (*file)(nil)
	_ fs.NodeGetxattrer    = (*file)(nil)
	_ fs.NodeSetxattrer    = (*file)(nil)
	_ fs.NodeListxattrer   = (*file)(nil)
	_ fs.NodeRemovexattrer = (*file)(nil)
	_ fs.NodeOnForgetter   = (*file)(nil)
	_ fs.NodeAccesser      = (*file)(nil)
)

func (f *file) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(mask)
}

func (f *file) OnForget() {
	f.m.forget(f.entry.Node, f.EmbeddedInode())
}

func (f *file) current() files.Entry {
	if e, ok, err := f.m.service.Item(f.entry.Item); err == nil && ok && e.Node == f.entry.Node {
		return e
	}
	return f.entry
}

func (f *file) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	attributes(f.current(), &out.Attr)
	return 0
}

func (f *file) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	if state, err := f.m.service.State(f.entry); err == nil && state != "local" && state != "kept" {
		if name, ok := f.m.bulkReader(ctx); ok {
			f.m.log.Debug("Refused to fetch a file for a program that reads everything", "program", name,
				"item", f.entry.Item)
			return nil, 0, syscall.EPERM
		}
	}
	r, err := f.m.service.OpenFile(f.entry.Item)
	if err != nil {
		return nil, 0, errno(err)
	}
	if r.ContentID() != f.entry.ContentID {
		// The file changed since it was looked up: its new node has the new content
		r.Close()
		return nil, 0, syscall.ESTALE
	}
	// The pages stay valid as long as the node does: the node changes with the content
	return &handle{r: r}, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *file) Read(ctx context.Context, h fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	fh, ok := h.(*handle)
	if !ok {
		return nil, syscall.EBADF
	}
	n, err := fh.r.ReadAt(ctx, dest, off)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *file) Release(ctx context.Context, h fs.FileHandle) syscall.Errno {
	if fh, ok := h.(*handle); ok {
		fh.r.Close()
	}
	return 0
}

func (f *file) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return f.m.getxattr(f.entry.Item, attr, dest)
}

func (f *file) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	return f.m.setxattr(f.entry.Item, attr, data)
}

func (f *file) Removexattr(ctx context.Context, attr string) syscall.Errno {
	return f.m.setxattr(f.entry.Item, attr, []byte("0"))
}

func (f *file) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	return listxattr(dest)
}

type handle struct {
	r *files.Reader
}

func (m *Mount) getxattr(item int64, attr string, dest []byte) (uint32, syscall.Errno) {
	e, ok, err := m.service.Item(item)
	if err != nil {
		return 0, errno(err)
	}
	if !ok {
		return 0, syscall.ENOENT
	}
	var value string
	switch attr {
	case xattrState:
		state, err := m.service.State(e)
		if err != nil {
			return 0, errno(err)
		}
		value = state
	case xattrKeep:
		value = "0"
		if e.Keep {
			value = "1"
		}
	default:
		return 0, syscall.ENODATA
	}
	if len(dest) == 0 {
		return uint32(len(value)), 0
	}
	if len(dest) < len(value) {
		return uint32(len(value)), syscall.ERANGE
	}
	return uint32(copy(dest, value)), 0
}

func (m *Mount) setxattr(item int64, attr string, data []byte) syscall.Errno {
	if attr != xattrKeep {
		return syscall.ENOTSUP
	}
	var keep bool
	switch strings.TrimSpace(string(data)) {
	case "1", "true", "yes":
		keep = true
	case "0", "false", "no", "":
	default:
		return syscall.EINVAL
	}
	return errno(m.service.SetKeep(item, keep))
}

func listxattr(dest []byte) (uint32, syscall.Errno) {
	names := xattrState + "\x00" + xattrKeep + "\x00"
	if len(dest) == 0 {
		return uint32(len(names)), 0
	}
	if len(dest) < len(names) {
		return uint32(len(names)), syscall.ERANGE
	}
	return uint32(copy(dest, names)), 0
}

// bulkReader reports whether the request comes from a program that reads every file it is shown, and its name
func (m *Mount) bulkReader(ctx context.Context) (string, bool) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return "", false
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", caller.Pid))
	if err != nil {
		return "", false
	}
	name := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
	for _, prefix := range readerNames {
		if strings.HasPrefix(name, prefix) {
			return name, true
		}
	}
	return name, false
}
