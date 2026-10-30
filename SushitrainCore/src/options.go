// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/ur"
)

// The units the minimum free disk space can have, as in the web GUI
var minHomeDiskFreeUnits = []string{"%", "kB", "MB", "GB", "TB"}

const (
	// At this many percent no disk has enough space, and every folder stops with an error
	maxMinHomeDiskFreePercent = 100
	// Beyond this, the engine's calculation of how long to keep a temporary file overflows
	maxKeepTemporariesH = int(math.MaxInt64 / int64(time.Hour))
)

// The least the engine works with, whatever is set
var minIncomingRequestKiB = config.OptionsConfiguration{RawMaxCIRequestKiB: 1}.MaxConcurrentIncomingRequestKiB()

var (
	errAuditFileNotAbsolute = errors.New("enter the full path of the audit log, or leave it empty")
	errAuditFileNotRegular  = errors.New("the audit log has to be a regular file")
	errAuditFileInFolder    = errors.New("the audit log has secrets in it, so it can't be in a synchronized folder")
)

// EngineOptions are the engine's general and advanced options that have no setting of their own in the app, which it
// shows and saves together. Numbers are text, as they were entered, for SetEngineOptions to check.
type EngineOptions struct {
	// How much space to keep free on the disk that has the database: a number, and one of MinHomeDiskFreeUnits
	MinHomeDiskFree     string
	MinHomeDiskFreeUnit string
	KeepTemporariesH    string
	// 0 is one per CPU core, and below 0 there is no limit
	MaxFolderConcurrency string
	// 0 is Syncthing's default of 256 MiB, and below 0 there is no limit
	MaxIncomingRequestKiB  string
	TempIndexMinBlocks     string
	SendFullIndexOnUpgrade bool
	// One per line
	FeatureFlags string
	AuditEnabled bool
	// Where the audit log goes; empty for a new file in the configuration folder at each engine start
	AuditFile      string
	LowPriority    bool
	UsageReporting bool
	CrashReporting bool

	// The options as they were read, which tell what was edited
	saved *EngineOptions
}

func newEngineOptions(opts config.OptionsConfiguration) *EngineOptions {
	options := &EngineOptions{
		MinHomeDiskFree:        strconv.FormatFloat(opts.MinHomeDiskFree.Value, 'f', -1, 64),
		MinHomeDiskFreeUnit:    opts.MinHomeDiskFree.Unit,
		KeepTemporariesH:       strconv.Itoa(opts.KeepTemporariesH),
		MaxFolderConcurrency:   strconv.Itoa(opts.RawMaxFolderConcurrency),
		MaxIncomingRequestKiB:  strconv.Itoa(opts.RawMaxCIRequestKiB),
		TempIndexMinBlocks:     strconv.Itoa(opts.TempIndexMinBlocks),
		SendFullIndexOnUpgrade: opts.SendFullIndexOnUpgrade,
		FeatureFlags:           strings.Join(opts.FeatureFlags, "\n"),
		AuditEnabled:           opts.AuditEnabled,
		AuditFile:              opts.AuditFile,
		LowPriority:            opts.SetLowPriority,
		UsageReporting:         opts.URAccepted > 0,
		CrashReporting:         opts.CREnabled,
	}
	read := *options
	options.saved = &read
	return options
}

// Equal reports whether both hold the same options
func (o *EngineOptions) Equal(other *EngineOptions) bool {
	first, second := *o, *other
	first.saved, second.saved = nil, nil
	return first == second
}

// MinHomeDiskFreeUnits are the units to choose from, with the one the configuration has if that is another
func (o *EngineOptions) MinHomeDiskFreeUnits() *ListOfStrings {
	units := minHomeDiskFreeUnits
	if o.saved != nil && !slices.Contains(units, o.saved.MinHomeDiskFreeUnit) {
		units = append(slices.Clone(units), o.saved.MinHomeDiskFreeUnit)
	}
	return List(units)
}

// applyTo checks the options that were edited, and puts them in opts, which is a copy to throw away if this fails. What
// wasn't edited stays as it is by now: what the web GUI or the REST API changed since the options were read, and a value
// the engine would not take from here, such as one in an unusual unit.
func (o *EngineOptions) applyTo(opts *config.OptionsConfiguration) error {
	saved := o.saved
	if o.MinHomeDiskFree != saved.MinHomeDiskFree || o.MinHomeDiskFreeUnit != saved.MinHomeDiskFreeUnit {
		size, err := o.minHomeDiskFree()
		if err != nil {
			return err
		}
		opts.MinHomeDiskFree = size
	}
	if err := errors.Join(
		setNumberOption("keep temporary files", o.KeepTemporariesH, saved.KeepTemporariesH, &opts.KeepTemporariesH,
			0, maxKeepTemporariesH),
		setNumberOption("folders working at once", o.MaxFolderConcurrency, saved.MaxFolderConcurrency,
			&opts.RawMaxFolderConcurrency, math.MinInt32, math.MaxInt32),
		setNumberOption("data for incoming requests", o.MaxIncomingRequestKiB, saved.MaxIncomingRequestKiB,
			&opts.RawMaxCIRequestKiB, math.MinInt32, math.MaxInt32),
		setNumberOption("minimum blocks for a temporary index", o.TempIndexMinBlocks, saved.TempIndexMinBlocks,
			&opts.TempIndexMinBlocks, 0, math.MaxInt32),
	); err != nil {
		return err
	}
	if changed := o.MaxIncomingRequestKiB != saved.MaxIncomingRequestKiB; changed && opts.RawMaxCIRequestKiB > 0 &&
		opts.RawMaxCIRequestKiB < minIncomingRequestKiB {
		return fmt.Errorf("data for incoming requests: the engine needs at least %d KiB", minIncomingRequestKiB)
	}
	if o.AuditEnabled != saved.AuditEnabled {
		opts.AuditEnabled = o.AuditEnabled
	}
	if o.AuditFile != saved.AuditFile {
		opts.AuditFile = strings.TrimSpace(o.AuditFile)
	}
	if o.FeatureFlags != saved.FeatureFlags {
		opts.FeatureFlags = advancedListItems(o.FeatureFlags, false)
	}
	if o.SendFullIndexOnUpgrade != saved.SendFullIndexOnUpgrade {
		opts.SendFullIndexOnUpgrade = o.SendFullIndexOnUpgrade
	}
	if o.LowPriority != saved.LowPriority {
		opts.SetLowPriority = o.LowPriority
	}
	if o.CrashReporting != saved.CrashReporting {
		opts.CREnabled = o.CrashReporting
	}
	if o.UsageReporting != saved.UsageReporting {
		// As in the web GUI: it asks again about versions newer than the one seen
		opts.URSeen = ur.Version
		opts.URAccepted = -1
		if o.UsageReporting {
			opts.URAccepted = ur.Version
		}
	}
	fillReportingURLs(opts)
	return nil
}

func (o *EngineOptions) minHomeDiskFree() (config.Size, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(o.MinHomeDiskFree), 64)
	if err != nil || math.IsNaN(value) || value < 0 {
		return config.Size{}, errors.New("minimum free disk space: enter a number of 0 or more")
	}
	if !slices.Contains(o.MinHomeDiskFreeUnits().data, o.MinHomeDiskFreeUnit) {
		return config.Size{}, errors.New("minimum free disk space: choose one of the units")
	}
	size := config.Size{Value: value, Unit: o.MinHomeDiskFreeUnit}
	if math.IsInf(size.BaseValue(), 0) {
		return config.Size{}, errors.New("minimum free disk space: enter a smaller number")
	}
	if size.Percentage() && value >= maxMinHomeDiskFreePercent {
		return config.Size{}, fmt.Errorf("minimum free disk space: enter less than %d %%", maxMinHomeDiskFreePercent)
	}
	return size, nil
}

// setNumberOption puts an edited number in its option, if it is one from minimum to maximum
func setNumberOption(label string, text string, saved string, option *int, minimum int, maximum int) error {
	if text == saved {
		return nil
	}
	number, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || number < minimum || number > maximum {
		return fmt.Errorf("%s: enter a whole number from %d to %d", label, minimum, maximum)
	}
	*option = number
	return nil
}

// checkAuditFile reports what keeps the audit log from being written where the options say, as that would otherwise
// only show after a restart of the engine. Syncthing's "-" and "--" (standard output and error) go nowhere in an app, and
// neither does a relative path, which would be relative to the root of the disk. A file can stay as it is while the log
// is turned off, so that one that can't be used doesn't keep the log on.
func (o *EngineOptions) checkAuditFile(folders map[string]config.FolderConfiguration) error {
	if o.AuditFile == o.saved.AuditFile && (!o.AuditEnabled || o.saved.AuditEnabled) {
		return nil
	}
	path := strings.TrimSpace(o.AuditFile)
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return errAuditFileNotAbsolute
	}
	if isInSynchronizedFolder(path, folders) {
		return errAuditFileInFolder
	}
	if !o.AuditEnabled {
		return nil
	}
	file, err := openAuditFile(path)
	if err != nil {
		return fmt.Errorf("the audit log can't be written there: %w", err)
	}
	return file.Close()
}

// isInSynchronizedFolder reports whether a path is in one of the folders, from which it would reach their devices
func isInSynchronizedFolder(path string, folders map[string]config.FolderConfiguration) bool {
	for _, folder := range folders {
		root, err := fs.ExpandTilde(folder.Path)
		if err != nil || !isNativeFilesystem(folder.FilesystemType) {
			continue
		}
		if relative, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(relative) {
			return true
		}
	}
	return false
}

// openAuditFile opens the audit log as Syncthing's command line does: to add to a given file, or a new file of its own.
// Opening anything but a file could wait forever, as for a named pipe nothing reads.
func openAuditFile(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(locations.GetTimestamped(locations.AuditLog), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if !filepath.IsAbs(path) {
		return nil, errAuditFileNotAbsolute
	}
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errAuditFileNotRegular
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// startAuditLog has a run write the audit log if the options ask for it. Only Syncthing's command line does that by
// itself. Where Syncthing stops when it can't, the run goes on without it, and AuditStatusMessage says so.
func (r *engineRun) startAuditLog(opts config.OptionsConfiguration, appOptions *syncthing.Options) {
	if !opts.AuditEnabled {
		return
	}
	file, err := openAuditFile(opts.AuditFile)
	if err != nil {
		slog.Error("could not open the audit log", "path", opts.AuditFile, "cause", err)
		r.auditNotice = fmt.Sprintf("The audit log can't be written: %v.", err)
		return
	}
	r.auditLog = file
	r.auditNotice = fmt.Sprintf("The audit log is written to %s.", file.Name())
	appOptions.AuditWriter = file
}

func (r *engineRun) closeAuditLog() {
	if r.auditLog != nil {
		_ = r.auditLog.Close()
	}
}

// EngineOptions are the options the app has a page for; nil while the configuration is not loaded
func (clt *Client) EngineOptions() *EngineOptions {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newEngineOptions(cfg.Options())
}

// SetEngineOptions saves the options that were edited. When one can't be used, nothing changes and the error says why.
func (clt *Client) SetEngineOptions(options *EngineOptions) error {
	wrapper := clt.cfg()
	if wrapper == nil {
		return ErrStillLoading
	}
	if options.saved == nil {
		options.saved = newEngineOptions(wrapper.Options())
	}
	// Not while changing the configuration: a slow disk would hold up every other change
	if err := options.checkAuditFile(wrapper.Folders()); err != nil {
		return err
	}
	var invalid error
	err := clt.changeConfiguration(func(cfg *config.Configuration) {
		opts := cfg.Options.Copy()
		if invalid = options.applyTo(&opts); invalid == nil {
			cfg.Options = opts
		}
	})
	if invalid != nil {
		return invalid
	}
	return err
}

// AuditStatusMessage says where the running engine writes its audit log, or why it can't; empty when it writes none
func (clt *Client) AuditStatusMessage() string {
	if r := clt.currentRun(); r != nil {
		return r.auditNotice
	}
	return ""
}
