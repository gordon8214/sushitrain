// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux

package linuxdesktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"t-shaped.nl/sushitrain/v2/previewcache"
)

// Files starts Files mode, rolling back the old service if startup fails.
func Files(fromSynchronize bool) error {
	unlock, err := transitionLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	return filesMode(fromSynchronize)
}
func filesMode(fromSynchronize bool) error {
	if fromSynchronize {
		if err := systemctl("disable", "--now", "synctrain-gateway.service"); err != nil {
			return err
		}
	}
	err := systemctl("enable", "--now", "synctrain-files.service")
	if err == nil {
		err = waitForService("synctrain-files.service", FilesSocket(), time.Minute)
	}
	if err == nil {
		err = WriteMode(modeFiles)
	}
	if err != nil {
		stopUnit("synctrain-files.service")
		if fromSynchronize {
			_ = systemctl("enable", "--now", "synctrain-gateway.service")
		}
	}
	return err
}

func Service(command string) error {
	if command != "start" && command != "stop" && command != "restart" {
		return errors.New("invalid service command")
	}
	unit := "synctrain-gateway.service"
	if ReadMode() == modeFiles {
		unit = "synctrain-files.service"
	}
	return systemctl(command, unit)
}

func Autostart(enabled bool) error {
	path := filepath.Join(configDir(), "autostart", "synctrain.desktop")
	if !enabled {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("[Desktop Entry]\nType=Application\nName=Synctrain\nExec=synctrain --background\nIcon=com.gordonbyrnes.Synctrain\n"), 0600)
}

// Dispatch exposes desktop-owned operations without starting an engine or a daemon.
// A cross-process lock serializes mode transitions from either native frontend.
func Dispatch(ctx context.Context, method string, data json.RawMessage) (any, error) {
	var p struct {
		Mode, Command, Location, Name string
		Enabled, External             bool
		Limit                         int64
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
	}
	switch method {
	case "desktop.inspect":
		check, err := Inspect()
		return map[string]any{"Mode": ReadMode(), "Check": check, "FilesSocket": FilesSocket(), "Linger": Lingering(), "Autostart": fileExists(filepath.Join(configDir(), "autostart", "synctrain.desktop"))}, err
	case "desktop.switch":
		if p.Mode != modeSynchronize && p.Mode != modeFiles {
			return nil, errors.New("choose Synchronize or Files")
		}
		unlock, err := transitionLock(ctx)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if p.Mode == modeFiles {
			return nil, filesMode(ReadMode() == modeSynchronize)
		}
		check, err := Inspect()
		if err != nil {
			return nil, err
		}
		if check.Refusal != "" {
			return nil, errors.New(check.Refusal)
		}
		result, err := synchronize(check.Home, ReadMode() == modeFiles)
		if err != nil {
			note := result.Notes()
			if result.GatewayStarted && result.Backup != "" {
				note += " syncthing.service stays masked because the engine may have migrated its database. Restore the backup before starting an older engine."
			}
			if note != "" {
				err = fmt.Errorf("%w. %s", err, note)
			}
		}
		return result, err
	case "desktop.service":
		return nil, Service(p.Command)
	case "desktop.autostart":
		return nil, Autostart(p.Enabled)
	case "desktop.linger":
		command := "disable-linger"
		if p.Enabled {
			command = "enable-linger"
		}
		return nil, run("loginctl", command)
	case "desktop.stop":
		return nil, systemctl("stop", "synctrain-gateway.service", "synctrain-files.service")
	case "desktop.preview":
		if p.Limit < 1 || p.Limit > 512<<20 {
			return nil, errors.New("invalid preview size limit")
		}
		local, err := PreviewCopy(ctx, p.Location, p.Name, p.Limit)
		if err == nil && p.External {
			err = previewcache.RetainExternal(filepath.Dir(local))
			if err != nil {
				os.RemoveAll(filepath.Dir(local))
			}
		}
		return local, err
	}
	return nil, errors.New("unsupported desktop operation")
}

func transitionLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(modePath()), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(modePath() + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("a mode change is already running")
	}
	if err := ctx.Err(); err != nil {
		lock.Unlock()
		return nil, err
	}
	return func() { _ = lock.Unlock() }, nil
}
