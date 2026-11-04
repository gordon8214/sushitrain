// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package previewcache

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Reserve serializes quota admission and construction across daemon/UI processes.
func Reserve(ctx context.Context, root string, needed int64) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, ".quota"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = lock(f, true, true)
		if err == nil {
			break
		}
		if !lockBusy(err) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err = Ensure(root, needed); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Pin keeps an open native preview usable until its window closes.
func Pin(directory string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(directory, ".in-use"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lock(f, false, false); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// RetainExternal keeps copies handed to another app until normal day-old expiry.
func RetainExternal(directory string) error {
	return os.WriteFile(filepath.Join(directory, ".external"), nil, 0600)
}
func evictionLease(directory string) (func(), bool) {
	if info, err := os.Stat(filepath.Join(directory, ".external")); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return nil, false
	}
	f, err := os.OpenFile(filepath.Join(directory, ".in-use"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false
	}
	if err = lock(f, true, true); err != nil {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}
