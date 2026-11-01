// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
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

	opts := clt.cfg().Options()
	if opts.MaxSendKbps != 500 {
		t.Errorf("unchanged send limit became %d KiB/s, want 500", opts.MaxSendKbps)
	}
	if opts.MaxRecvKbps != 1221 {
		t.Errorf("receive limit is %d KiB/s, want 1221", opts.MaxRecvKbps)
	}
}
