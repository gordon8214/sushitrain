// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePackageTypes counts "icon" and "xcodeproj" as packages, like a Mac, and records what it was asked
type fakePackageTypes struct {
	mutex    sync.Mutex
	asked    []string
	packages map[string]bool
}

func newFakePackageTypes() *fakePackageTypes {
	return &fakePackageTypes{packages: map[string]bool{"icon": true, "xcodeproj": true}}
}

func (f *fakePackageTypes) IsPackageExtension(ext string) bool {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.asked = append(f.asked, ext)
	return f.packages[ext]
}

func (f *fakePackageTypes) set(ext string, isPackage bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.packages[ext] = isPackage
}

func (f *fakePackageTypes) questions() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.asked)
}

func TestPackageCacheAsksOncePerExtension(t *testing.T) {
	types := newFakePackageTypes()
	cache := newPackageCache(types)
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"AppIcon.icon", true},
		{"Loud.ICON", true},
		{"Demo.xcodeproj", true},
		{"a b.icon", true},
		{"Photos", false},
		{"Notes.txt", false},
		// As macOS takes extensions: none after a leading dot alone, none with a space
		{".icon", false},
		{"..icon", false},
		{".swiftpm", false},
		{"Name.icon ", false},
		{"Name.my icon", false},
		{"Trailing.", false},
		// Not asked: no type has a numeric or very long extension
		{"v1.2", false},
		{"Backup.20261110", false},
		{"x." + strings.Repeat("a", packageExtensionMaxLength+1), false},
	} {
		if got := cache.isPackage(tc.name); got != tc.want {
			t.Errorf("isPackage(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Lower case, without the dot, and once per extension
	if want := []string{"icon", "xcodeproj", "txt"}; !slices.Equal(types.asked, want) {
		t.Fatalf("asked %v, want %v", types.asked, want)
	}

	// A newly installed app's type counts once the answer expires, and each answer expires on its own
	now = now.Add(packageAnswerLifetime / 2)
	types.set("bbprojectd", true)
	cache.isPackage("Site.bbprojectd")
	types.set("bbprojectd", false)
	if !cache.isPackage("Site.bbprojectd") {
		t.Fatal("the answer was not kept")
	}
	now = now.Add(packageAnswerLifetime / 2)
	asked := types.questions()
	cache.isPackage("Other.icon") // asked a lifetime ago
	if !cache.isPackage("Site.bbprojectd") || types.questions() != asked+1 {
		t.Fatalf("asked %v", types.asked)
	}
	now = now.Add(packageAnswerLifetime / 2)
	if cache.isPackage("Site.bbprojectd") {
		t.Fatal("the answer was kept past its lifetime")
	}

	var none *packageCache
	if newPackageCache(nil) != nil || none.isPackage("AppIcon.icon") {
		t.Fatal("a host without package types marks packages")
	}
}

// Names with many different extensions keep a bounded number of answers
func TestPackageCacheIsBounded(t *testing.T) {
	cache := newPackageCache(newFakePackageTypes())
	for i := range 3 * packageAnswersKept {
		cache.isPackage(fmt.Sprintf("build.x%d", i))
		if len(cache.answers) > packageAnswersKept {
			t.Fatalf("%d answers kept", len(cache.answers))
		}
	}
}

// Directories with a package extension are marked in snapshots, change lists and items; files with one are not
func TestPackagesAreMarked(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	for _, name := range []string{"AppIcon.icon/icon.json", "Demo.xcodeproj/project.pbxproj", "Photos/a.jpg", "b.icon",
		".icon/x.json"} {
		e.write(name, "contents of "+name)
	}
	types := newFakePackageTypes()
	ts := newTestService(t, e, "", func(cfg *Config) { cfg.Packages = types })
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})

	var page SnapshotPage
	ts.json("GET", "/v1/folders/docs/snapshot", token, nil, http.StatusOK, &page)
	marked := map[string]bool{}
	for _, item := range page.Items {
		marked[item.Path] = item.Package
	}
	want := map[string]bool{"": false, "AppIcon.icon": true, "AppIcon.icon/icon.json": false, "Demo.xcodeproj": true,
		"Demo.xcodeproj/project.pbxproj": false, "Photos": false, "Photos/a.jpg": false, "b.icon": false, ".icon": false}
	for p, w := range want {
		if got, ok := marked[p]; !ok || got != w {
			t.Errorf("snapshot: %q package %v (listed %v), want %v", p, got, ok, w)
		}
	}
	if item := ts.itemAt(token, "AppIcon.icon"); !item.Package || item.Kind != "directory" {
		t.Fatalf("item %+v", item)
	}

	e.write("Second.icon/icon.json", "{}")
	ts.sync()
	var changes ChangesPage
	ts.json("GET", "/v1/folders/docs/changes?cursor="+page.Cursor, token, nil, http.StatusOK, &changes)
	found := false
	for _, item := range changes.Changes {
		if item.Path == "Second.icon" {
			found = item.Package
		}
	}
	if !found {
		t.Fatalf("changes %+v", changes.Changes)
	}
}

// The results of operations carry the mark, and tombstones don't
func TestOperationResultsMarkPackages(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("Photos/a.jpg", "jpeg")
	ts := newTestService(t, e, "", func(cfg *Config) { cfg.Packages = newFakePackageTypes() })
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(token, "")

	created := ts.operate(token, createDirectoryRequest(root, "Made.icon"))
	wantCommitted(t, created)
	if created.Item == nil || !created.Item.Package {
		t.Fatalf("created %+v", created.Item)
	}
	moved := ts.operate(token, moveRequest(ts.itemAt(token, "Photos"), root, "Photos.xcodeproj"))
	wantCommitted(t, moved)
	if moved.Item == nil || !moved.Item.Package {
		t.Fatalf("moved %+v", moved.Item)
	}

	var page SnapshotPage
	ts.json("GET", "/v1/folders/docs/snapshot", token, nil, http.StatusOK, &page)
	wantCommitted(t, ts.operate(token, deleteRequest(ts.itemAt(token, "Made.icon"))))
	ts.sync()
	var changes ChangesPage
	ts.json("GET", "/v1/folders/docs/changes?cursor="+page.Cursor, token, nil, http.StatusOK, &changes)
	deleted := false
	for _, item := range changes.Changes {
		if item.Path == "Made.icon" && item.Deleted {
			deleted = true
			if item.Package {
				t.Fatalf("tombstone %+v", item)
			}
		}
	}
	if !deleted {
		t.Fatalf("changes %+v", changes.Changes)
	}
}

// A host without package types (Linux) marks none
func TestPackagesNeedTheHostsTypes(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("AppIcon.icon/icon.json", "{}")
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	if item := ts.itemAt(token, "AppIcon.icon"); item.Package {
		t.Fatalf("item %+v", item)
	}
}
