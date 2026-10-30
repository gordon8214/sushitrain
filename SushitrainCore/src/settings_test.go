// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"maps"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestBandwidthLimitConversion(t *testing.T) {
	for mbits := 0; mbits <= 1000; mbits++ {
		if got := kibPerSecondToMbitsPerSecond(mbitsPerSecondToKiBPerSecond(mbits)); got != mbits {
			t.Fatalf("%d Mbit/s reads back as %d", mbits, got)
		}
	}
	// 10 Mbit/s is 1,250,000 bytes/s
	if kib := mbitsPerSecondToKiBPerSecond(10); kib != 1221 {
		t.Errorf("10 Mbit/s is %d KiB/s, want 1221", kib)
	}
	// A limit below 1 Mbit/s is still a limit
	if mbits := kibPerSecondToMbitsPerSecond(50); mbits != 1 {
		t.Errorf("50 KiB/s reads as %d Mbit/s, want 1", mbits)
	}
}

func TestSetBandwidthLimitsKeepsUnchangedLimit(t *testing.T) {
	folder := newFolderBaseTestFolder(t, filepath.Join(t.TempDir(), "folder"), false)
	clt := folder.client
	if err := clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.Options.MaxSendKbps = 500
		cfg.Options.MaxRecvKbps = 0
	}); err != nil {
		t.Fatal(err)
	}

	up := clt.GetBandwidthLimitUpMbitsPerSec()
	if up != 4 {
		t.Fatalf("500 KiB/s reads as %d Mbit/s, want 4", up)
	}
	if err := clt.SetBandwidthLimitsMbitsPerSec(10, up); err != nil {
		t.Fatal(err)
	}

	opts := clt.config.Options()
	if opts.MaxSendKbps != 500 {
		t.Errorf("unchanged send limit became %d KiB/s, want 500", opts.MaxSendKbps)
	}
	if opts.MaxRecvKbps != 1221 {
		t.Errorf("receive limit is %d KiB/s, want 1221", opts.MaxRecvKbps)
	}
}

func TestSetVersioningKeepsUnmanagedSettings(t *testing.T) {
	tests := []struct {
		name       string
		before     config.VersioningConfiguration
		setType    string
		wantParams map[string]string
		wantFSPath string
	}{
		{
			name: "platform history survives a staggered edit",
			before: config.VersioningConfiguration{
				Type:   VersioningTypeStaggered,
				Params: map[string]string{"maxAge": "31536000", "preservePlatformData": "true"},
				FSType: config.FilesystemTypeBasic,
			},
			setType:    VersioningTypeStaggered,
			wantParams: map[string]string{"maxAge": "2592000", "preservePlatformData": "true"},
		},
		{
			name: "platform history is dropped for another type",
			before: config.VersioningConfiguration{
				Type:   VersioningTypeStaggered,
				Params: map[string]string{"maxAge": "31536000", "preservePlatformData": "true"},
				FSType: config.FilesystemTypeBasic,
			},
			setType:    VersioningTypeSimple,
			wantParams: map[string]string{"keep": "5", "cleanoutDays": "7"},
		},
		{
			name: "custom versions path survives",
			before: config.VersioningConfiguration{
				Type:   VersioningTypeTrashcan,
				Params: map[string]string{"cleanoutDays": "0"},
				FSPath: "/elsewhere/versions",
				FSType: config.FilesystemTypeBasic,
			},
			setType:    VersioningTypeTrashcan,
			wantParams: map[string]string{"cleanoutDays": "7"},
			wantFSPath: "/elsewhere/versions",
		},
		{
			name:       "enabling versioning",
			before:     config.VersioningConfiguration{},
			setType:    VersioningTypeTrashcan,
			wantParams: map[string]string{"cleanoutDays": "7"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := newFolderBaseTestFolder(t, filepath.Join(t.TempDir(), "folder"), false)
			if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
				fc.Versioning = tt.before
			}); err != nil {
				t.Fatal(err)
			}

			if err := folder.SetVersioning(tt.setType, 5, 7, 30, 3600); err != nil {
				t.Fatal(err)
			}

			got := folder.folderConfiguration().Versioning
			if got.Type != tt.setType {
				t.Errorf("type is %q, want %q", got.Type, tt.setType)
			}
			if !maps.Equal(got.Params, tt.wantParams) {
				t.Errorf("params are %v, want %v", got.Params, tt.wantParams)
			}
			if got.FSPath != tt.wantFSPath {
				t.Errorf("versions path is %q, want %q", got.FSPath, tt.wantFSPath)
			}
			if got.FSType != config.FilesystemTypeBasic {
				t.Errorf("versions filesystem type is %q, want basic", got.FSType)
			}
			if got.CleanupIntervalS != 3600 {
				t.Errorf("cleanup interval is %d s, want 3600", got.CleanupIntervalS)
			}
		})
	}
}
