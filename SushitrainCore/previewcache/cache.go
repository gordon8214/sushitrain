// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package previewcache bounds disposable preview copies; synchronized data and pinned files never enter it.
package previewcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const Budget int64 = 1 << 30
const MaxFile int64 = 512 << 20

// Ensure keeps recent copies for external applications, expires day-old copies,
// and refuses a new preview rather than evicting a recently opened copy.
func Ensure(root string, needed int64) error {
	if needed < 0 || needed > MaxFile {
		return errors.New("this file is too large for an internal preview; use Save a Copy")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type candidate struct {
		path string
		size int64
		when time.Time
	}
	list := []candidate{}
	var used int64
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		path := filepath.Join(root, e.Name())
		var size int64
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type().IsRegular() {
				i, err := d.Info()
				if err != nil {
					return err
				}
				size += i.Size()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) > 24*time.Hour {
			release, ok := evictionLease(path)
			if !ok {
				used += size
				list = append(list, candidate{path, size, info.ModTime()})
				continue
			}
			if err = os.RemoveAll(path); err != nil {
				release()
				return err
			}
			release()
			continue
		}
		used += size
		list = append(list, candidate{path, size, info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].when.Before(list[j].when) })
	for _, c := range list {
		if used+needed <= Budget {
			break
		}
		if time.Since(c.when) < 5*time.Minute {
			continue
		}
		release, ok := evictionLease(c.path)
		if !ok {
			continue
		}
		if err = os.RemoveAll(c.path); err != nil {
			release()
			return err
		}
		release()
		used -= c.size
	}
	if used+needed > Budget {
		return errors.New("the preview cache is full; close recent previews and try again later")
	}
	return nil
}
