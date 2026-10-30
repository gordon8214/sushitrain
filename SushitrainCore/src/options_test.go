// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/ur"
)

func TestEngineOptionsChangeNothingUnlessEdited(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	// None of these could be entered on the page
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.MinHomeDiskFree = config.Size{Value: 150, Unit: "KiB"}
		cfg.Options.KeepTemporariesH = -3
		cfg.Options.RawMaxFolderConcurrency = -8
		cfg.Options.RawMaxCIRequestKiB = 1024
		cfg.Options.TempIndexMinBlocks = -1
		cfg.Options.FeatureFlags = []string{" spaced "}
		cfg.Options.AuditFile = "relative.log"
		cfg.Options.URAccepted = 2
	}); err != nil {
		t.Fatal(err)
	}
	before := wrapper.RawCopy()
	if err := client.SetEngineOptions(client.EngineOptions()); err != nil {
		t.Fatal(err)
	}
	if after := wrapper.RawCopy(); !reflect.DeepEqual(before.Options, after.Options) {
		t.Fatalf("changed from %+v to %+v", before.Options, after.Options)
	}
}

func TestEngineOptionsAreSaved(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	auditFile := filepath.Join(t.TempDir(), "audit.log")
	options := client.EngineOptions()
	options.MinHomeDiskFree = " 2.5 "
	options.MinHomeDiskFreeUnit = "GB"
	options.KeepTemporariesH = "48"
	options.MaxFolderConcurrency = "-1"
	options.MaxIncomingRequestKiB = "524288"
	options.TempIndexMinBlocks = "0"
	options.SendFullIndexOnUpgrade = true
	options.FeatureFlags = " one\n\ntwo \n"
	options.AuditEnabled = true
	options.AuditFile = " " + auditFile + " "
	options.LowPriority = false
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}

	opts := wrapper.Options()
	if opts.MinHomeDiskFree != (config.Size{Value: 2.5, Unit: "GB"}) || opts.KeepTemporariesH != 48 ||
		opts.RawMaxFolderConcurrency != -1 || opts.RawMaxCIRequestKiB != 524288 || opts.TempIndexMinBlocks != 0 ||
		!opts.SendFullIndexOnUpgrade || !slices.Equal(opts.FeatureFlags, []string{"one", "two"}) || !opts.AuditEnabled ||
		opts.AuditFile != auditFile || opts.SetLowPriority {
		t.Fatalf("saved %+v", opts)
	}
	read := client.EngineOptions()
	if read.MinHomeDiskFree != "2.5" || read.FeatureFlags != "one\ntwo" {
		t.Fatalf("read %+v", read)
	}
}

func TestEngineOptionsLeaveChangesFromElsewhereAlone(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.URAccepted = 3
		cfg.Options.CREnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	options := client.EngineOptions()

	// The web GUI or the REST API, while the page has unsaved edits
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.URAccepted = -1
		cfg.Options.CREnabled = false
		cfg.Options.KeepTemporariesH = 72
		cfg.Options.FeatureFlags = []string{"elsewhere"}
		cfg.Options.SendFullIndexOnUpgrade = true
		cfg.Options.SetLowPriority = false
	}); err != nil {
		t.Fatal(err)
	}
	options.TempIndexMinBlocks = "20"
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}

	opts := wrapper.Options()
	if opts.URAccepted != -1 || opts.CREnabled {
		t.Fatalf("reporting came back on: usage %d, crash %v", opts.URAccepted, opts.CREnabled)
	}
	if opts.KeepTemporariesH != 72 || !slices.Equal(opts.FeatureFlags, []string{"elsewhere"}) ||
		!opts.SendFullIndexOnUpgrade || opts.SetLowPriority {
		t.Fatalf("changes were undone: %+v", opts)
	}
	if opts.TempIndexMinBlocks != 20 {
		t.Fatalf("the edit was lost: %d", opts.TempIndexMinBlocks)
	}
}

func TestEngineOptionsCompareWithoutWhatTheyWereReadAs(t *testing.T) {
	client, _ := advancedTestClient(t)
	first, second := client.EngineOptions(), client.EngineOptions()
	if !first.Equal(second) {
		t.Fatal("the same options differ")
	}
	second.KeepTemporariesH = "1"
	if first.Equal(second) {
		t.Fatal("an edit makes no difference")
	}
}

func TestEngineOptionsAreChecked(t *testing.T) {
	regularFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(regularFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	withUnit := func(value, unit string) func(*EngineOptions) {
		return func(o *EngineOptions) { o.MinHomeDiskFree, o.MinHomeDiskFreeUnit = value, unit }
	}
	withAuditFile := func(path string) func(*EngineOptions) {
		return func(o *EngineOptions) { o.AuditEnabled, o.AuditFile = true, path }
	}
	for name, test := range map[string]struct {
		edit func(*EngineOptions)
		want string
	}{
		"free space that is no number":   {withUnit("much", "GB"), "free disk space"},
		"infinite free space":            {withUnit("Inf", "GB"), "free disk space"},
		"free space that is not a value": {withUnit("NaN", "GB"), "free disk space"},
		"free space past what fits":      {withUnit("1e308", "TB"), "smaller number"},
		"negative free space":            {withUnit("-1", "GB"), "free disk space"},
		"all of the disk free":           {withUnit("100", "%"), "less than 100"},
		"an unknown unit":                {withUnit("5", "bogus"), "one of the units"},
		"temporaries for negative hours": {func(o *EngineOptions) { o.KeepTemporariesH = "-1" }, "temporary files"},
		"temporaries for too long":       {func(o *EngineOptions) { o.KeepTemporariesH = "2562048" }, "temporary files"},
		"half a folder":                  {func(o *EngineOptions) { o.MaxFolderConcurrency = "1.5" }, "folders working"},
		"too many folders":               {func(o *EngineOptions) { o.MaxFolderConcurrency = "2147483648" }, "folders working"},
		"too little incoming data":       {func(o *EngineOptions) { o.MaxIncomingRequestKiB = "32767" }, "needs at least 32768 KiB"},
		"negative blocks":                {func(o *EngineOptions) { o.TempIndexMinBlocks = "-1" }, "temporary index"},
		"relative audit log":             {func(o *EngineOptions) { o.AuditFile = "audit.log" }, "full path"},
		"audit log on standard output":   {func(o *EngineOptions) { o.AuditFile = "-" }, "full path"},
		"audit log nowhere to write":     {withAuditFile(filepath.Join(regularFile, "audit.log")), "can't be written"},
		"audit log on a named pipe":      {withAuditFile(pipe), "regular file"},
		"audit log on a directory":       {withAuditFile(t.TempDir()), "regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			client, wrapper := advancedTestClient(t)
			before := wrapper.RawCopy()
			options := client.EngineOptions()
			test.edit(options)
			err := client.SetEngineOptions(options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error: %v", err)
			}
			if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
				t.Fatal("changed")
			}
		})
	}
}

func TestEngineOptionsAcceptTheirLimits(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	options := client.EngineOptions()
	options.KeepTemporariesH = "2562047"
	options.MaxFolderConcurrency = " 5 "
	options.MaxIncomingRequestKiB = "32768"
	options.MinHomeDiskFree = "99.5"
	options.MinHomeDiskFreeUnit = "%"
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	opts := wrapper.Options()
	if opts.KeepTemporariesH != 2562047 || opts.RawMaxFolderConcurrency != 5 || opts.RawMaxCIRequestKiB != 32768 ||
		opts.MinHomeDiskFree != (config.Size{Value: 99.5, Unit: "%"}) {
		t.Fatalf("saved %+v", opts)
	}

	// Past 100 is no problem in a unit other than percent, and the unit can change by itself
	options = client.EngineOptions()
	options.MinHomeDiskFree = "150"
	options.MinHomeDiskFreeUnit = "GB"
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	options = client.EngineOptions()
	options.MinHomeDiskFreeUnit = "TB"
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	if size := wrapper.Options().MinHomeDiskFree; size != (config.Size{Value: 150, Unit: "TB"}) {
		t.Fatalf("saved %+v", size)
	}
}

func TestEngineOptionsTakeTheEnginesOwnMeanings(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	save := func(concurrency, incomingKiB string) config.OptionsConfiguration {
		t.Helper()
		options := client.EngineOptions()
		options.MaxFolderConcurrency = concurrency
		options.MaxIncomingRequestKiB = incomingKiB
		if err := client.SetEngineOptions(options); err != nil {
			t.Fatal(err)
		}
		return wrapper.Options()
	}

	// Below 0 there is no limit, which the engine spells zero
	opts := save("-5", "-5")
	if opts.MaxFolderConcurrency() != 0 || opts.MaxConcurrentIncomingRequestKiB() != 0 {
		t.Fatalf("no limits: %d, %d", opts.MaxFolderConcurrency(), opts.MaxConcurrentIncomingRequestKiB())
	}
	opts = save("7", "65536")
	if opts.MaxFolderConcurrency() != 7 || opts.MaxConcurrentIncomingRequestKiB() != 65536 {
		t.Fatalf("set limits: %d, %d", opts.MaxFolderConcurrency(), opts.MaxConcurrentIncomingRequestKiB())
	}
	// 0 is the engine's default
	opts = save("0", "0")
	if opts.MaxFolderConcurrency() <= 0 || opts.MaxConcurrentIncomingRequestKiB() != 256*1024 {
		t.Fatalf("defaults: %d, %d", opts.MaxFolderConcurrency(), opts.MaxConcurrentIncomingRequestKiB())
	}
}

func TestAnUnusualUnitOfFreeSpaceStaysAChoice(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if units := client.EngineOptions().MinHomeDiskFreeUnits().data; !slices.Equal(units, minHomeDiskFreeUnits) {
		t.Fatalf("units: %v", units)
	}
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.MinHomeDiskFree = config.Size{Value: 1, Unit: "KiB"}
	}); err != nil {
		t.Fatal(err)
	}

	options := client.EngineOptions()
	options.MinHomeDiskFreeUnit = "GB"
	if units := options.MinHomeDiskFreeUnits().data; !slices.Contains(units, "KiB") {
		t.Fatalf("after choosing another: %v", units)
	}
	if slices.Contains(minHomeDiskFreeUnits, "KiB") {
		t.Fatal("the list everyone starts from changed")
	}
	options.MinHomeDiskFree = "2"
	options.MinHomeDiskFreeUnit = "KiB"
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	if size := wrapper.Options().MinHomeDiskFree; size != (config.Size{Value: 2, Unit: "KiB"}) {
		t.Fatalf("saved %+v", size)
	}
}

func TestReportingIsTurnedOnAndOff(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.URAccepted = -1
		cfg.Options.URSeen = 0
		cfg.Options.URURL = ""
		cfg.Options.CRURL = ""
		cfg.Options.CREnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	options := client.EngineOptions()
	if options.UsageReporting || options.CrashReporting {
		t.Fatalf("on: %+v", options)
	}

	options.UsageReporting = true
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	opts := wrapper.Options()
	if opts.URAccepted != ur.Version || opts.URSeen != ur.Version || opts.URURL == "" || opts.URUniqueID == "" {
		t.Fatalf("usage reporting: %+v", opts)
	}
	// Syncthing sends failure reports with usage reporting on, to this address
	if opts.CRURL == "" || opts.CREnabled {
		t.Fatalf("crash reporting: %q, %v", opts.CRURL, opts.CREnabled)
	}

	options = client.EngineOptions()
	options.UsageReporting = false
	options.CrashReporting = true
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	opts = wrapper.Options()
	if opts.URAccepted != -1 || opts.URSeen != ur.Version || !opts.CREnabled {
		t.Fatalf("after turning usage reporting off: %+v", opts)
	}
}

func TestEngineOptionsAreNotSavedWhileLoading(t *testing.T) {
	client := &Client{}
	if client.EngineOptions() != nil {
		t.Fatal("options without a configuration")
	}
	if err := client.SetEngineOptions(&EngineOptions{}); !errors.Is(err, ErrStillLoading) {
		t.Fatalf("error: %v", err)
	}
	if message := client.AuditStatusMessage(); message != "" {
		t.Fatalf("status: %q", message)
	}
}

func TestAuditLogCanBeTurnedOffWithAFileThatCantBeUsed(t *testing.T) {
	for _, path := range []string{"-", "relative.log", filepath.Join(t.TempDir(), "missing", "audit.log")} {
		client, wrapper := advancedTestClient(t)
		if err := client.changeConfiguration(func(cfg *config.Configuration) {
			cfg.Options.AuditEnabled = true
			cfg.Options.AuditFile = path
		}); err != nil {
			t.Fatal(err)
		}
		options := client.EngineOptions()
		options.AuditEnabled = false
		if err := client.SetEngineOptions(options); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if opts := wrapper.Options(); opts.AuditEnabled || opts.AuditFile != path {
			t.Fatalf("%s: enabled %v, file %q", path, opts.AuditEnabled, opts.AuditFile)
		}
	}
}

func TestAuditFileOnlyHasToBeWritableWhenInUse(t *testing.T) {
	client, _ := advancedTestClient(t)
	options := client.EngineOptions()
	options.AuditFile = filepath.Join(t.TempDir(), "missing", "audit.log")
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
	options = client.EngineOptions()
	options.AuditEnabled = true
	if err := client.SetEngineOptions(options); err == nil {
		t.Fatal("a folder that doesn't exist")
	}
}

func TestAuditLogStaysOutOfSynchronizedFolders(t *testing.T) {
	root := t.TempDir()
	client, wrapper := advancedTestClient(t)
	if err := client.changeConfiguration(func(cfg *config.Configuration) {
		folder := cfg.Defaults.Folder.Copy()
		folder.ID = "synchronized"
		folder.Path = root
		folder.FilesystemType = config.FilesystemTypeBasic
		cfg.SetFolder(folder)
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{filepath.Join(root, "audit.log"), filepath.Join(root, "deeper", "audit.log")} {
		options := client.EngineOptions()
		options.AuditFile = path
		if err := client.SetEngineOptions(options); !errors.Is(err, errAuditFileInFolder) {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if file := wrapper.Options().AuditFile; file != "" {
		t.Fatalf("saved %s", file)
	}

	options := client.EngineOptions()
	options.AuditFile = filepath.Join(t.TempDir(), "audit.log")
	if err := client.SetEngineOptions(options); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogAddsToAGivenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	opts := config.OptionsConfiguration{AuditEnabled: true, AuditFile: path}
	write := func(line string) {
		t.Helper()
		run := &engineRun{}
		var appOptions syncthing.Options
		run.startAuditLog(opts, &appOptions)
		if appOptions.AuditWriter == nil {
			t.Fatalf("no audit log: %s", run.auditNotice)
		}
		if _, err := io.WriteString(appOptions.AuditWriter, line); err != nil {
			t.Fatal(err)
		}
		run.closeAuditLog()
		if _, err := io.WriteString(appOptions.AuditWriter, "after closing"); err == nil {
			t.Fatal("the file is still open")
		}
	}

	write("first\n")
	write("second\n")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("audit log: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("others can read it: %o", mode)
	}
}

func TestARunWithoutAnAuditLogSaysNothing(t *testing.T) {
	run := &engineRun{}
	var appOptions syncthing.Options
	run.startAuditLog(config.OptionsConfiguration{}, &appOptions)
	if appOptions.AuditWriter != nil || run.auditNotice != "" {
		t.Fatalf("writer %v, notice %q", appOptions.AuditWriter, run.auditNotice)
	}
	run.closeAuditLog()
}
