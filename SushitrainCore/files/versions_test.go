// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

func TestVersions(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "trashcan", Params: map[string]string{"cleanoutDays": "0"}}
	})
	e.Write("doc.txt", "first")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	ts.write(ts.mustLookup("docs/doc.txt"), "second", true)
	ts.settled()
	ctx := context.Background()
	doc := ts.mustLookup("docs/doc.txt")

	versions, err := ts.Versions(ctx, doc.Item)
	if err != nil {
		t.Fatal(err)
	}
	if versions.Versioning != "trashcan" || len(versions.Versions) != 1 || versions.Versions[0].Size != 5 {
		t.Fatalf("versions %+v", versions)
	}
	v := versions.Versions[0]
	preview, err := ts.VersionToFile(ctx, doc.Item, v.ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(preview); string(data) != "first" || filepath.Base(preview) != "doc.txt" {
		t.Fatalf("the preview %s has %q", preview, data)
	}

	name, err := ts.CopyVersion(ctx, doc.Item, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "doc (") || !strings.HasSuffix(name, ").txt") {
		t.Fatalf("the copy is named %q", name)
	}
	ts.gatewayHas(name, "first")

	if err := ts.RestoreVersion(ctx, doc.Item, v.ID); err != nil {
		t.Fatal(err)
	}
	ts.gatewayHas("doc.txt", "first")
	if got := ts.readPath("docs/doc.txt"); got != "first" {
		t.Fatalf("after restoring, the file reads %q", got)
	}

	// Not over a change that isn't on the gateway yet
	g.Stop()
	ts.write(ts.mustLookup("docs/doc.txt"), "unsent", true)
	time.Sleep(5 * testSettleDelay)
	if err := ts.RestoreVersion(ctx, doc.Item, v.ID); !errors.Is(err, ErrPendingChange) {
		t.Fatalf("restoring over an unsent change: %v", err)
	}
	g.Start()
	ts.settled()
}

func TestVersionCopyName(t *testing.T) {
	when := time.Date(2026, 10, 20, 15, 56, 0, 0, time.Local)
	if got := versionCopyName("report.odt", when, nil); got != "report (2026-10-20 15.56).odt" {
		t.Fatalf("got %q", got)
	}
	taken := map[string]bool{"report (2026-10-20 15.56).odt": true}
	if got := versionCopyName("report.odt", when, taken); got != "report (2026-10-20 15.56) 2.odt" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 200) + ".txt"
	if got := versionCopyName(long, when, nil); len(got) > 255 {
		t.Fatalf("a long name became %d bytes", len(got))
	}
}
