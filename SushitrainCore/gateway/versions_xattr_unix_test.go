// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

//go:build linux || darwin

package gateway

import (
	"github.com/syncthing/syncthing/lib/versioner"
	"golang.org/x/sys/unix"
	"net/http"
	"path/filepath"
	"testing"
)

func TestUnchangedContentsWithXattrsAreArchived(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	e.write("tagged.txt", "contents")
	path := filepath.Join(e.folderPath, "tagged.txt")
	if err := unix.Setxattr(path, "user.synctrain-test", []byte("kept"), 0); err != nil {
		t.Skipf("the folder's filesystem can't set extended attributes: %v", err)
	}
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "tagged.txt")

	// Swapping the same contents in would lose the attributes: the replaced file is archived with them
	upload, digest := ts.upload(token, []byte("contents"))
	var same Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &same)
	if same.State != stateResultCommitted {
		t.Fatalf("replace with the same contents: %+v", same)
	}
	list := ts.versions(token, ts.itemAt(token, "tagged.txt"))
	if len(list.Versions) != 1 {
		t.Fatalf("versions after writing the same contents: %+v", list.Versions)
	}
	archived, err := filepath.Glob(filepath.Join(e.folderPath, versioner.DefaultPath, "tagged~*.txt"))
	if err != nil || len(archived) != 1 {
		t.Fatalf("archived files: %v %v", archived, err)
	}
	buf := make([]byte, 16)
	n, err := unix.Getxattr(archived[0], "user.synctrain-test", buf)
	if err != nil {
		t.Fatalf("archived file's attribute: %v", err)
	}
	if string(buf[:n]) != "kept" {
		t.Fatalf("archived file's attribute holds %q", buf[:n])
	}
}
