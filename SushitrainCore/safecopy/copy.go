// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package safecopy publishes exclusive exports through a held destination directory.
package safecopy

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Destination struct {
	File      *os.File
	root      *os.Root
	name      string
	temporary string
	identity  os.FileInfo
	committed bool
}

func Create(path string, forbidden ...string) (*Destination, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("choose a full local destination path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			root.Close()
		}
	}()
	handle, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	actual, err := directoryPath(handle, parent)
	handle.Close()
	if err != nil {
		return nil, err
	}
	for _, blocked := range forbidden {
		if blocked == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(blocked)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(real, actual)
		if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return nil, errors.New("choose a destination outside Synctrain's mounted folders")
		}
	}
	name := filepath.Base(path)
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, os.ErrExist
		}
		return nil, err
	}
	temporary := ".syncthing.export-" + uuid.NewString() + ".tmp"
	f, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	identity, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	ok = true
	return &Destination{File: f, root: root, name: name, temporary: temporary, identity: identity}, nil
}
func (d *Destination) Commit() error {
	if err := d.File.Sync(); err != nil {
		return err
	}
	if err := publish(d.root, d.temporary, d.name, d.File); err != nil {
		return err
	}
	dir, err := d.root.Open(".")
	if err != nil {
		return err
	}
	err = errors.Join(d.File.Close(), dir.Sync())
	err = errors.Join(err, dir.Close())
	if err != nil {
		if info, e := d.root.Lstat(d.name); e == nil && os.SameFile(info, d.identity) {
			d.root.Remove(d.name)
		}
		return err
	}
	d.committed = true
	return nil
}
func (d *Destination) Close() {
	d.File.Close()
	if info, err := d.root.Lstat(d.temporary); err == nil && os.SameFile(info, d.identity) {
		d.root.Remove(d.temporary)
	}
	d.root.Close()
}

func Copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if e := ctx.Err(); e != nil {
				return total, e
			}
			written, e := dst.Write(buf[:n])
			total += int64(written)
			if e != nil {
				return total, e
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
