// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package gateway

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/versioner"
	"io"
	"net/http"
	"net/url"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
	"testing"
	"time"
)

func TestVersionSnapshotReadOnlyAndMissingMetadata(t *testing.T) {
	e := newTestEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "staggered", Params: map[string]string{"preservePlatformData": "true"}}
	})
	e.write("file.txt", "history")
	cfg, _ := e.cfg.Folder("docs")
	v, err := versioner.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pv, ok := versioner.WithPlatformData(v)
	if !ok {
		t.Fatal("no platform history")
	}
	if err = pv.ArchiveWithPlatformData("file.txt", protocol.PlatformData{Darwin: &protocol.XattrData{Xattrs: []protocol.Xattr{{Name: "com.apple.ResourceFork", Value: []byte("fork")}}}}, false); err != nil {
		t.Fatal(err)
	}
	e.write("file.txt", "current")
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	var list PathVersionList
	query := "?path=" + url.QueryEscape("file.txt")
	ts.json("GET", "/v1/folders/docs/versions"+query, token, nil, http.StatusOK, &list)
	if len(list.Versions) != 1 || list.Versions[0].MetadataStatus != "complete" || list.Versions[0].SnapshotETag == "" {
		t.Fatalf("listing %+v", list)
	}
	route := "/v1/folders/docs/versions/" + list.Versions[0].ID + "/snapshot" + query
	resp, _ := ts.do("GET", route, token, nil)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("missing validator: %d", resp.StatusCode)
	}
	response, payload := ts.do("GET", route, token, nil, "If-Match", list.Versions[0].SnapshotETag)
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != list.Versions[0].SnapshotETag {
		t.Fatalf("snapshot: %d %s", response.StatusCode, payload)
	}
	tr := tar.NewReader(bytes.NewReader(payload))
	header, err := tr.Next()
	if err != nil || header.Name != "manifest.json" {
		t.Fatal("missing snapshot manifest")
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	var manifest versionsnapshot.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Attributes) != 1 || manifest.Attributes[0].Name != "com.apple.ResourceFork" || manifest.Attributes[0].SHA256 != versionsnapshot.Digest([]byte("fork")) {
		t.Fatalf("lost metadata: %+v", manifest)
	}
	// Old archives remain visible but never acquire a complete-metadata claim.
	e.archiveVersion("old.txt", "legacy", time.Now().Add(-time.Hour), time.Now().Add(-2*time.Hour))
	ts.json("GET", "/v1/folders/docs/versions?path=old.txt", token, nil, http.StatusOK, &list)
	if len(list.Versions) != 1 || list.Versions[0].MetadataStatus == "complete" {
		t.Fatalf("legacy listing %+v", list)
	}
}
