// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

// Package fusefs shows the folders of a files.Service as a FUSE filesystem: a directory per folder at the root,
// files that are fetched as they are read, changes recorded before they are answered, and the kernel's caches
// dropped as the gateways' changes arrive.
package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
	"t-shaped.nl/sushitrain/v2/files"
)

const (
	// Extended attributes: the state of an item, and whether it is kept on this device ("1" or "0")
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
	"gdk-pixbuf-thumbnailer", "glycin-thumbnailer", "gst-video-thumbnailer", "gst-audio-thumbnailer",
	"totem-video-thumbnailer", "ffmpegthumbnailer",
	"evince-thumbnailer", "papers-thumbnailer", "atril-thumbnailer", "xreader-thumbnailer", "gnome-thumbnail-font",
	"gsf-office-thumbnailer", "gnome-epub-thumbnailer", "gnome-mobi-thumbnailer", "tumblerd", "localsearch-extractor",
	"tracker-extract", "tracker-miner-fs", "localsearch-3",
	"baloo_file", "baloo_file_extractor", "balooctl",
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
	nodes map[uint64]*gofs.Inode
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
	m := &Mount{service: service, log: opts.Logger, dir: dir, mounted: time.Now(), nodes: map[uint64]*gofs.Inode{}}
	root := &rootDir{m: m}
	entryTimeout, attrTimeout, negative := cacheTimeout, cacheTimeout, negativeTimeout
	server, err := gofs.Mount(dir, root, &gofs.Options{
		// Without default_permissions: the mount is its owner's alone (no allow_other), and the daemon decides what
		// can be written, so that the keep attribute can be set on a file that can't be written
		MountOptions: fuse.MountOptions{
			FsName: "synctrain",
			Name:   "synctrain",
			Debug:  opts.Debug,
			// Nothing in the mount has security labels; without this the kernel asks before every write
			IgnoreSecurityLabels: true,
		},
		EntryTimeout:    &entryTimeout,
		AttrTimeout:     &attrTimeout,
		NegativeTimeout: &negative,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		RootStableAttr:  &gofs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR},
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

func (m *Mount) remember(node uint64, inode *gofs.Inode) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.nodes[node] = inode
}

func (m *Mount) forget(node uint64, inode *gofs.Inode) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.nodes[node] == inode {
		delete(m.nodes, node)
	}
}

func (m *Mount) inode(node uint64) *gofs.Inode {
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
		if child := m.inode(e.Node); e.Gone && child != nil {
			// Also tells inotify watchers that it is gone. Only for what is: the kernel takes the node as removed (a
			// directory that was only moved would be dead for a shell in it).
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
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, fs.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, fs.ErrInvalid), errors.Is(err, files.ErrInvalidName):
		return syscall.EINVAL
	case errors.Is(err, files.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, files.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, files.ErrCrossFolder):
		return syscall.EXDEV
	case errors.Is(err, files.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, files.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, files.ErrVersionChanged):
		return syscall.ESTALE
	case errors.Is(err, files.ErrKeptAbove):
		return syscall.EPERM
	default:
		return syscall.EIO
	}
}

// owner owns everything in the mount: the user it is mounted for
var owner = fuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}

// attributes fills out with what the mount shows of an entry. The owner is always set: an answer without one (to a
// chmod, say) would make the kernel think root owns the file, and refuse its owner what only an owner may do.
func attributes(e files.Entry, out *fuse.Attr) {
	out.Owner = owner
	out.Ino = e.Node
	mtime := e.ModTime
	out.SetTimes(&mtime, &mtime, &mtime)
	if e.IsDir() {
		out.Mode = fuse.S_IFDIR | 0o555
		if e.Writable {
			out.Mode |= 0o200
		}
		out.Nlink = 2
		return
	}
	out.Mode = fuse.S_IFREG | 0o444
	if e.Writable {
		out.Mode |= 0o200
	}
	out.Nlink = 1
	out.Size = uint64(e.Size)
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 128 << 10
}

// access answers access(2), which the kernel asks the mount about without default_permissions: nothing in the
// mount can be executed, as its modes say, and only what may be written can be
func access(writable, dir bool, mask uint32) syscall.Errno {
	if mask&unix.W_OK != 0 && !writable {
		return syscall.EACCES
	}
	if mask&unix.X_OK != 0 && !dir {
		return syscall.EACCES
	}
	return 0
}

func (m *Mount) child(ctx context.Context, parent *gofs.Inode, e files.Entry, out *fuse.EntryOut) *gofs.Inode {
	attributes(e, &out.Attr)
	var node gofs.InodeEmbedder
	mode := uint32(fuse.S_IFREG)
	if e.IsDir() {
		node, mode = &dir{m: m, item: e.Item, node: e.Node}, fuse.S_IFDIR
	} else {
		node = &file{m: m, entry: e}
	}
	inode := parent.NewInode(ctx, node, gofs.StableAttr{Mode: mode, Ino: e.Node})
	m.remember(e.Node, inode)
	return inode
}

func (m *Mount) readdir(entries []files.Entry, extra ...fuse.DirEntry) gofs.DirStream {
	list := slices.Clone(extra)
	for _, e := range entries {
		mode := uint32(fuse.S_IFREG)
		if e.IsDir() {
			mode = fuse.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: mode, Ino: e.Node})
	}
	return gofs.NewListDirStream(list)
}

// statfs reports the space where edits wait to be sent: what can be written here
func (m *Mount) statfs(out *fuse.StatfsOut) syscall.Errno {
	var st unix.Statfs_t
	if err := unix.Statfs(m.service.StateDir(), &st); err != nil {
		return errno(err)
	}
	out.Blocks, out.Bfree, out.Bavail = st.Blocks, st.Bfree, st.Bavail
	out.Files, out.Ffree = st.Files, st.Ffree
	out.Bsize, out.Frsize, out.NameLen = uint32(st.Bsize), uint32(st.Frsize), 255
	return 0
}

// readOnlyFile is a file the mount makes up, which can be read only (.trackerignore)
type readOnlyFile struct {
	gofs.Inode
	data  []byte
	mtime time.Time
}

func (f *readOnlyFile) attributes(out *fuse.Attr) {
	out.Owner = owner
	out.Mode = fuse.S_IFREG | 0o444
	out.Nlink = 1
	out.Size = uint64(len(f.data))
	out.SetTimes(&f.mtime, &f.mtime, &f.mtime)
}

func (f *readOnlyFile) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.attributes(&out.Attr)
	return 0
}

func (f *readOnlyFile) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn,
	out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

func (f *readOnlyFile) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(false, false, mask)
}

func (f *readOnlyFile) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *readOnlyFile) Read(ctx context.Context, fh gofs.FileHandle, dest []byte, off int64) (fuse.ReadResult,
	syscall.Errno) {
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(f.data[off:min(int64(len(f.data)), off+int64(len(dest)))]), 0
}

// rootDir is the mount's root: a directory per folder, and .trackerignore
type rootDir struct {
	gofs.Inode
	m *Mount
}

var (
	_ gofs.NodeLookuper  = (*rootDir)(nil)
	_ gofs.NodeReaddirer = (*rootDir)(nil)
	_ gofs.NodeGetattrer = (*rootDir)(nil)
	_ gofs.NodeAccesser  = (*rootDir)(nil)
	_ gofs.NodeStatfser  = (*rootDir)(nil)
	_ gofs.NodeUnlinker  = (*rootDir)(nil)
	_ gofs.NodeRmdirer   = (*rootDir)(nil)
	_ gofs.NodeMkdirer   = (*rootDir)(nil)
	_ gofs.NodeRenamer   = (*rootDir)(nil)
	_ gofs.NodeFsyncer   = (*rootDir)(nil)
	_ gofs.NodeFsyncer   = (*dir)(nil)
	_ gofs.NodeAccesser  = (*readOnlyFile)(nil)
	_ gofs.NodeOpener    = (*readOnlyFile)(nil)
	_ gofs.NodeGetattrer = (*readOnlyFile)(nil)
	_ gofs.NodeSetattrer = (*readOnlyFile)(nil)
)

func (r *rootDir) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(false, true, mask)
}

// The root holds the folders, which are added and removed through Synctrain only
func (r *rootDir) Unlink(ctx context.Context, name string) syscall.Errno { return syscall.EROFS }
func (r *rootDir) Rmdir(ctx context.Context, name string) syscall.Errno  { return syscall.EROFS }
func (r *rootDir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode,
	syscall.Errno) {
	return nil, syscall.EROFS
}
func (r *rootDir) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string,
	flags uint32) syscall.Errno {
	return syscall.EROFS
}
func (r *rootDir) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode,
	syscall.Errno) {
	return nil, syscall.EROFS
}
func (r *rootDir) Mknod(ctx context.Context, name string, mode, dev uint32, out *fuse.EntryOut) (*gofs.Inode,
	syscall.Errno) {
	return nil, syscall.EROFS
}
func (r *rootDir) Link(ctx context.Context, target gofs.InodeEmbedder, name string, out *fuse.EntryOut) (
	*gofs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}

// Fsync of a directory has nothing to do: what changed in it was recorded before it was answered
func (r *rootDir) Fsync(ctx context.Context, f gofs.FileHandle, flags uint32) syscall.Errno { return 0 }

func (r *rootDir) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	return r.m.statfs(out)
}

func (r *rootDir) Getattr(ctx context.Context, f gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Owner = owner
	out.Mode = fuse.S_IFDIR | 0o555
	out.Ino = 1
	out.Nlink = 2
	out.SetTimes(&r.m.mounted, &r.m.mounted, &r.m.mounted)
	return 0
}

func (r *rootDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if name == ".trackerignore" {
		f := &readOnlyFile{data: []byte(trackerIgnore), mtime: r.m.mounted}
		f.attributes(&out.Attr)
		return r.NewInode(ctx, f, gofs.StableAttr{Ino: 2 << 62}), 0
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

func (r *rootDir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	folders, err := r.m.service.Children(0)
	if err != nil {
		return nil, errno(err)
	}
	return r.m.readdir(folders, fuse.DirEntry{Name: ".trackerignore", Mode: fuse.S_IFREG, Ino: 2 << 62}), 0
}

// itemOf is the item of a directory node (0: the mount's root)
func itemOf(node gofs.InodeEmbedder) (int64, bool) {
	switch n := node.(type) {
	case *dir:
		return n.item, true
	case *rootDir:
		return 0, true
	}
	return 0, false
}

// dir is a folder's root or a directory in it
type dir struct {
	gofs.Inode
	m    *Mount
	item int64
	node uint64
}

var (
	_ gofs.NodeLookuper      = (*dir)(nil)
	_ gofs.NodeReaddirer     = (*dir)(nil)
	_ gofs.NodeGetattrer     = (*dir)(nil)
	_ gofs.NodeSetattrer     = (*dir)(nil)
	_ gofs.NodeGetxattrer    = (*dir)(nil)
	_ gofs.NodeSetxattrer    = (*dir)(nil)
	_ gofs.NodeListxattrer   = (*dir)(nil)
	_ gofs.NodeRemovexattrer = (*dir)(nil)
	_ gofs.NodeOnForgetter   = (*dir)(nil)
	_ gofs.NodeAccesser      = (*dir)(nil)
	_ gofs.NodeStatfser      = (*dir)(nil)
	_ gofs.NodeCreater       = (*dir)(nil)
	_ gofs.NodeMkdirer       = (*dir)(nil)
	_ gofs.NodeUnlinker      = (*dir)(nil)
	_ gofs.NodeRmdirer       = (*dir)(nil)
	_ gofs.NodeRenamer       = (*dir)(nil)
	_ gofs.NodeSymlinker     = (*dir)(nil)
	_ gofs.NodeLinker        = (*dir)(nil)
	_ gofs.NodeMknoder       = (*dir)(nil)
)

func (d *dir) OnForget() {
	d.m.forget(d.node, d.EmbeddedInode())
}

func (d *dir) entry() (files.Entry, syscall.Errno) {
	e, ok, err := d.m.service.Item(d.item)
	if err != nil {
		return files.Entry{}, errno(err)
	}
	if !ok {
		return files.Entry{}, syscall.ENOENT
	}
	return e, 0
}

func (d *dir) Access(ctx context.Context, mask uint32) syscall.Errno {
	e, status := d.entry()
	if status != 0 {
		return status
	}
	return access(e.Writable, true, mask)
}

func (d *dir) Fsync(ctx context.Context, f gofs.FileHandle, flags uint32) syscall.Errno { return 0 }

func (d *dir) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	return d.m.statfs(out)
}

func (d *dir) Getattr(ctx context.Context, f gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	e, status := d.entry()
	if status != 0 {
		return status
	}
	attributes(e, &out.Attr)
	return 0
}

// Setattr of a directory: its times and modes are the gateway's, so changing them does nothing
func (d *dir) Setattr(ctx context.Context, f gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return d.Getattr(ctx, f, out)
}

func (d *dir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	e, ok, err := d.m.service.Lookup(d.item, name)
	if err != nil {
		return nil, errno(err)
	}
	if !ok {
		return nil, syscall.ENOENT
	}
	return d.m.child(ctx, d.EmbeddedInode(), e, out), 0
}

func (d *dir) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	children, err := d.m.service.Children(d.item)
	if err != nil {
		return nil, errno(err)
	}
	// What is listed is about to be shown: its thumbnails come from the gateway
	if rel, err := d.m.service.ItemPath(d.item); err == nil {
		d.m.service.FillThumbnails(filepath.Join(d.m.dir, filepath.FromSlash(rel)), children)
	}
	return d.m.readdir(children), 0
}

func (d *dir) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (
	*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	e, err := d.m.service.Create(d.item, name, false)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	h, status := d.m.openHandle(e, flags|syscall.O_TRUNC)
	if status != 0 {
		// The caller gets no file: neither does the folder
		if _, err := d.m.service.Remove(d.item, name, false); err != nil {
			d.m.log.Warn("Could not take back a file that couldn't be opened", "name", name, "error", err)
		}
		return nil, nil, 0, status
	}
	return d.m.child(ctx, d.EmbeddedInode(), e, out), h, 0, 0
}

func (d *dir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	e, err := d.m.service.Create(d.item, name, true)
	if err != nil {
		return nil, errno(err)
	}
	return d.m.child(ctx, d.EmbeddedInode(), e, out), 0
}

func (d *dir) Unlink(ctx context.Context, name string) syscall.Errno {
	_, err := d.m.service.Remove(d.item, name, false)
	return errno(err)
}

func (d *dir) Rmdir(ctx context.Context, name string) syscall.Errno {
	_, err := d.m.service.Remove(d.item, name, true)
	return errno(err)
}

func (d *dir) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string,
	flags uint32) syscall.Errno {
	if flags&^unix.RENAME_NOREPLACE != 0 {
		// Exchanging two items can't be one operation on a gateway
		return syscall.EINVAL
	}
	to, ok := itemOf(newParent)
	if !ok {
		return syscall.EXDEV
	}
	_, err := d.m.service.Rename(d.item, name, to, newName, flags&unix.RENAME_NOREPLACE != 0)
	return errno(err)
}

// Symbolic links, hard links and device nodes can't be in a folder of a gateway
func (d *dir) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dir) Link(ctx context.Context, target gofs.InodeEmbedder, name string, out *fuse.EntryOut) (*gofs.Inode,
	syscall.Errno) {
	return nil, syscall.EPERM
}

func (d *dir) Mknod(ctx context.Context, name string, mode uint32, dev uint32, out *fuse.EntryOut) (*gofs.Inode,
	syscall.Errno) {
	return nil, syscall.EPERM
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
	gofs.Inode
	m     *Mount
	entry files.Entry
}

var (
	_ gofs.NodeOpener        = (*file)(nil)
	_ gofs.NodeReader        = (*file)(nil)
	_ gofs.NodeWriter        = (*file)(nil)
	_ gofs.NodeFsyncer       = (*file)(nil)
	_ gofs.NodeFlusher       = (*file)(nil)
	_ gofs.NodeReleaser      = (*file)(nil)
	_ gofs.NodeGetattrer     = (*file)(nil)
	_ gofs.NodeSetattrer     = (*file)(nil)
	_ gofs.NodeGetxattrer    = (*file)(nil)
	_ gofs.NodeSetxattrer    = (*file)(nil)
	_ gofs.NodeListxattrer   = (*file)(nil)
	_ gofs.NodeRemovexattrer = (*file)(nil)
	_ gofs.NodeOnForgetter   = (*file)(nil)
	_ gofs.NodeAccesser      = (*file)(nil)
)

func (f *file) OnForget() {
	f.m.forget(f.entry.Node, f.EmbeddedInode())
}

// current is the entry as it is now, while the node shows the item, and as it was otherwise
func (f *file) current() files.Entry {
	if e, ok, err := f.m.service.Item(f.entry.Item); err == nil && ok && e.Node == f.entry.Node {
		return e
	}
	return f.m.service.WithWorkingCopy(f.entry)
}

func (f *file) Access(ctx context.Context, mask uint32) syscall.Errno {
	return access(f.current().Writable, false, mask)
}

func (f *file) Getattr(ctx context.Context, h gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	attributes(f.current(), &out.Attr)
	return 0
}

func (f *file) Setattr(ctx context.Context, h gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	e := f.current()
	if size, ok := in.GetSize(); ok {
		fh, _ := h.(*handle)
		if fh != nil && fh.w != nil {
			if err := fh.w.Truncate(ctx, int64(size)); err != nil {
				return errno(err)
			}
		} else {
			// truncate(2) on a path, or an open that truncates before it is sent
			w, err := f.m.service.OpenWriter(e, false)
			if err != nil {
				return errno(err)
			}
			err = w.Truncate(ctx, int64(size))
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return errno(err)
			}
		}
	}
	if mtime, ok := in.GetMTime(); ok {
		if err := f.m.service.SetModTime(f.entry.Node, mtime); err != nil {
			return errno(err)
		}
	}
	// Modes and owners are the mount's: changing them does nothing
	attributes(f.current(), &out.Attr)
	return 0
}

// openHandle opens a handle on what a file node shows, for writing too when flags say so
func (m *Mount) openHandle(e files.Entry, flags uint32) (*handle, syscall.Errno) {
	h := &handle{}
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&syscall.O_TRUNC != 0 {
		w, err := m.service.OpenWriter(e, flags&syscall.O_TRUNC != 0)
		if err != nil {
			return nil, errno(err)
		}
		h.w = w
	}
	// Writable handles read too: the kernel reads pages that writes only partly cover
	r, err := m.service.OpenNode(e)
	if err != nil {
		if h.w != nil {
			h.w.Close()
		}
		return nil, errno(err)
	}
	h.r = r
	return h, 0
}

func (f *file) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	e := f.current()
	writing := flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&syscall.O_TRUNC != 0
	if !writing {
		if state, err := f.m.service.State(e); err == nil && (state == "remote" || state == "partial") {
			if name, ok := f.m.bulkReader(ctx); ok {
				f.m.log.Debug("Refused to fetch a file for a program that reads everything", "program", name,
					"item", e.Item)
				return nil, 0, syscall.EPERM
			}
		}
	}
	h, status := f.m.openHandle(e, flags)
	if status != 0 {
		return nil, 0, status
	}
	if writing || !h.r.FromGateway() {
		// Pages of content written here aren't kept between opens
		return h, 0, 0
	}
	// Content from the gateway is the same as long as the node is: the node changes with the content
	return h, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *file) Read(ctx context.Context, h gofs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
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

func (f *file) Write(ctx context.Context, h gofs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	fh, ok := h.(*handle)
	if !ok || fh.w == nil {
		return 0, syscall.EBADF
	}
	n, err := fh.w.WriteAt(ctx, data, off)
	if err != nil && n == 0 {
		return 0, errno(err)
	}
	return uint32(n), 0
}

func (f *file) Fsync(ctx context.Context, h gofs.FileHandle, flags uint32) syscall.Errno {
	if fh, ok := h.(*handle); ok && fh.w != nil {
		return errno(fh.w.Sync())
	}
	return 0
}

func (f *file) Flush(ctx context.Context, h gofs.FileHandle) syscall.Errno {
	return 0
}

func (f *file) Release(ctx context.Context, h gofs.FileHandle) syscall.Errno {
	if fh, ok := h.(*handle); ok {
		fh.r.Close()
		if fh.w != nil {
			return errno(fh.w.Close())
		}
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
	w *files.Writer
}

func (m *Mount) getxattr(item int64, attr string, dest []byte) (uint32, syscall.Errno) {
	// Others (security.capability, which the kernel asks about before writes) answer at once
	if attr != xattrState && attr != xattrKeep {
		return 0, syscall.ENODATA
	}
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
	switch attr {
	case xattrKeep:
	case xattrState:
		// What the mount reports, not something to set: programs that copy a file's attributes (vim, cp -a) copy
		// it along, which does nothing
		return 0
	default:
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
	// KIO uses the same executable for copying files and making thumbnails.
	// Only its thumbnail protocol worker is a bulk reader.
	if name == "kioworker" || name == "kioslave5" {
		args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", caller.Pid))
		if err == nil && thumbnailWorker(strings.Split(string(args), "\x00")) {
			return name + " (thumbnail)", true
		}
	}
	for _, prefix := range readerNames {
		if strings.HasPrefix(name, prefix) {
			return name, true
		}
	}
	return name, false
}

func thumbnailWorker(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "thumbnail" || filepath.Base(arg) == "thumbnail.so" {
			return true
		}
	}
	return false
}
