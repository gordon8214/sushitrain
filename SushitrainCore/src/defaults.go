// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"fmt"
	"os"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
)

const ignoreIncludePrefix = "#include"

// changeDefaults changes the configuration only if block returns no error, which then comes back as it is
func (clt *Client) changeDefaults(block func(defaults *config.Defaults) error) error {
	var invalid error
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		defaults := cfg.Defaults
		defaults.Folder = cfg.Defaults.Folder.Copy()
		defaults.Device = cfg.Defaults.Device.Copy()
		if invalid = block(&defaults); invalid == nil {
			cfg.Defaults = defaults
		}
	})
	if invalid != nil {
		return invalid
	}
	return err
}

// DefaultFolderSettings are the settings new folders start from; nil while the configuration is not loaded
func (clt *Client) DefaultFolderSettings() *FolderSettings {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newFolderSettings(cfg.DefaultFolder())
}

// SetDefaultFolderSettings saves what was edited. The engine only checks a folder's settings when it starts one, so these
// are held to what any new folder can work with.
func (clt *Client) SetDefaultFolderSettings(settings *FolderSettings) error {
	wrapper := clt.cfg()
	if wrapper == nil {
		return ErrStillLoading
	}
	if settings.saved == nil {
		settings.saved = newFolderSettings(wrapper.DefaultFolder())
	}
	return clt.changeDefaults(func(defaults *config.Defaults) error {
		return settings.applyDefaultsTo(&defaults.Folder)
	})
}

// DefaultVersioningSettings are how new folders keep old versions; nil while the configuration is not loaded
func (clt *Client) DefaultVersioningSettings() *VersioningSettings {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newVersioningSettings(cfg.DefaultFolder().Versioning)
}

// SetDefaultVersioningSettings saves what was edited, apart from a path, which every new folder would then share
func (clt *Client) SetDefaultVersioningSettings(settings *VersioningSettings) error {
	wrapper := clt.cfg()
	if wrapper == nil {
		return ErrStillLoading
	}
	if settings.saved == nil {
		settings.saved = newVersioningSettings(wrapper.DefaultFolder().Versioning)
	}
	if settings.Path != settings.saved.Path {
		return errDefaultVersionsPath
	}
	return clt.changeDefaults(func(defaults *config.Defaults) error {
		return settings.applyTo(&defaults.Folder.Versioning)
	})
}

// DefaultDeviceSettings are the settings new devices start from; nil while the configuration is not loaded
func (clt *Client) DefaultDeviceSettings() *DeviceSettings {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newDeviceSettings(cfg.DefaultDevice())
}

// SetDefaultDeviceSettings saves what was edited
func (clt *Client) SetDefaultDeviceSettings(settings *DeviceSettings) error {
	wrapper := clt.cfg()
	if wrapper == nil {
		return ErrStillLoading
	}
	if settings.saved == nil {
		settings.saved = newDeviceSettings(wrapper.DefaultDevice())
	}
	return clt.changeDefaults(func(defaults *config.Defaults) error {
		return settings.applyTo(&defaults.Device)
	})
}

// DefaultIgnoreLines are the ignore patterns of new folders, one per line
func (clt *Client) DefaultIgnoreLines() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return strings.Join(cfg.DefaultIgnores().Lines, "\n")
}

// SetDefaultIgnoreLines saves the ignore patterns of new folders. Empty lines and spaces are kept, as in a folder's own
// patterns. A pattern the engine can't read would stop every new folder, so these are refused.
func (clt *Client) SetDefaultIgnoreLines(text string) error {
	lines := advancedListItems(text, true)
	if err := checkIgnoreLines(lines); err != nil {
		return err
	}
	return clt.changeDefaults(func(defaults *config.Defaults) error {
		defaults.Ignores.Lines = lines
		return nil
	})
}

// checkIgnoreLines has the engine read the patterns. Files to include can only be there in a folder, so those are skipped.
func checkIgnoreLines(lines []string) error {
	patterns := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), ignoreIncludePrefix) {
			patterns = append(patterns, line)
		}
	}
	matcher := ignore.New(fs.NewFilesystem(fs.FilesystemTypeBasic, os.TempDir()))
	if err := matcher.Parse(strings.NewReader(strings.Join(patterns, "\n")), ignoreFileName); err != nil {
		return fmt.Errorf("files to ignore: %w", err)
	}
	return nil
}

// placeDefaultIgnores gives a new folder the default ignore patterns before the engine sees the folder, as its first scan
// would otherwise index what they ignore. A folder that already has patterns keeps them, as in the web GUI.
func placeDefaultIgnores(folder *config.FolderConfiguration, lines []string) error {
	if err := folder.CreateRoot(); err != nil {
		return err
	}
	ffs := folder.Filesystem()
	// With no lines, writing removes the file, so this has to be sure that there is none
	if _, err := ffs.Lstat(ignoreFileName); !fs.IsNotExist(err) {
		return err
	}
	return ignore.WriteIgnores(ffs, ignoreFileName, lines)
}
