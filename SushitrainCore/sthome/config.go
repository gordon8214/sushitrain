// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
)

var (
	// ErrConfiguration means the configuration file can't be parsed: it is damaged, empty or truncated
	ErrConfiguration = errors.New("The configuration file is damaged")

	// ErrNewerConfiguration means the configuration file comes from a newer Syncthing, which this one can't read
	ErrNewerConfiguration = errors.New("The configuration file is from a newer version of Syncthing")

	// ErrUnreadableConfiguration means the configuration file exists but can't be read, for example for lack of
	// permission; the file itself may be fine
	ErrUnreadableConfiguration = errors.New("The configuration file can't be read")
)

// LoadConfig loads the configuration as Syncthing does at startup, and reports whether it created a new one.
//
// Only a missing file leads to a new configuration, with free ports probed when probePorts is set and adjustNew (when
// not nil) applied before it is first saved. Otherwise the file is never replaced: an error wraps ErrConfiguration,
// ErrNewerConfiguration or ErrUnreadableConfiguration, or comes from archiving an older configuration. A configuration
// from an older Syncthing is first archived as <path>.v<version>, then migrated and saved.
func LoadConfig(path string, cert tls.Certificate, evLogger events.Logger, probePorts bool,
	adjustNew func(*config.Configuration)) (config.Wrapper, bool, error) {
	myID := protocol.NewDeviceID(cert.Certificate[0])
	missing, err := isMissing(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnreadableConfiguration, err)
	}
	if missing {
		cfg, err := newConfig(path, myID, evLogger, probePorts, adjustNew)
		return cfg, err == nil, err
	}

	cfg, version, err := config.Load(path, myID, evLogger)
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		return nil, false, fmt.Errorf("%w: %w", ErrUnreadableConfiguration, err)
	case errors.Is(err, io.EOF):
		return nil, false, fmt.Errorf("%w: it is empty or truncated", ErrConfiguration)
	case err != nil:
		return nil, false, fmt.Errorf("%w: %w", ErrConfiguration, err)
	case version > config.CurrentVersion:
		return nil, false, fmt.Errorf("%w (format %d; this version reads up to %d). Use a newer version of the app.",
			ErrNewerConfiguration, version, config.CurrentVersion)
	case version < config.CurrentVersion:
		if err := archive(path, fmt.Sprintf("%s.v%d", path, version)); err != nil {
			return nil, false, fmt.Errorf("archiving the configuration from Syncthing format %d: %w", version, err)
		}
		if err := cfg.Save(); err != nil {
			return nil, false, fmt.Errorf("saving the migrated configuration: %w", err)
		}
	}
	return cfg, false, nil
}

// newConfig creates and saves a default configuration
func newConfig(path string, myID protocol.DeviceID, evLogger events.Logger, probePorts bool,
	adjust func(*config.Configuration)) (config.Wrapper, error) {
	cfg, err := syncthing.DefaultConfig(path, myID, evLogger, !probePorts)
	if err != nil {
		return nil, fmt.Errorf("creating a configuration: %w", err)
	}
	if adjust != nil {
		raw := cfg.RawCopy()
		adjust(&raw)
		cfg = config.Wrap(path, raw, myID, evLogger)
	}
	if err := cfg.Save(); err != nil {
		return nil, fmt.Errorf("saving a new configuration: %w", err)
	}
	return cfg, nil
}

// archive copies the configuration file to a private archive copy
func archive(path, archivePath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(archivePath, data, 0o600)
}
