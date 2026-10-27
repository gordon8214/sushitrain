// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
)

type fakeGlobalIndex struct {
	metas     []globalFileMeta
	infos     map[string]protocol.FileInfo
	infoCalls []string
}

func (f *fakeGlobalIndex) allGlobalFiles(folderID string, yield func(globalFileMeta) bool) error {
	for _, meta := range f.metas {
		if !yield(meta) {
			break
		}
	}
	return nil
}

func (f *fakeGlobalIndex) globalFileInfo(folderID string, path string) (protocol.FileInfo, bool, error) {
	f.infoCalls = append(f.infoCalls, path)
	info, ok := f.infos[path]
	return info, ok, nil
}

func (f *fakeGlobalIndex) add(meta globalFileMeta) {
	f.metas = append(f.metas, meta)
	if !meta.deleted {
		if f.infos == nil {
			f.infos = map[string]protocol.FileInfo{}
		}
		f.infos[meta.name] = protocol.FileInfo{
			Name:       meta.name,
			Type:       meta.fileType,
			Size:       meta.size,
			ModifiedS:  meta.modNanos / 1e9,
			ModifiedNs: int32(meta.modNanos % 1e9),
			ModifiedBy: protocol.ShortID(42),
			BlocksHash: []byte(meta.name),
			Sequence:   meta.sequence,
		}
	}
}

type collectingRecordDelegate struct {
	batches [][]fileProviderRecord
	err     error
}

func (c *collectingRecordDelegate) OnRecords(js []byte) error {
	if c.err != nil {
		return c.err
	}
	var batch []fileProviderRecord
	if err := json.Unmarshal(js, &batch); err != nil {
		return err
	}
	c.batches = append(c.batches, batch)
	return nil
}

func (c *collectingRecordDelegate) records() []fileProviderRecord {
	var all []fileProviderRecord
	for _, b := range c.batches {
		all = append(all, b...)
	}
	return all
}

func newFeedTestFolder(t *testing.T, ignoreLines string) *Folder {
	t.Helper()
	root := filepath.Join(t.TempDir(), "test")
	writeFolderWithMarker(t, root)
	if ignoreLines != "" {
		if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(ignoreLines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return newFolderBaseTestFolder(t, root, false)
}

func paths(records []fileProviderRecord) []string {
	var result []string
	for _, r := range records {
		result = append(result, r.Path)
	}
	return result
}

func TestGlobalChangesCursorAndPrivateFiles(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &fakeGlobalIndex{}
	idx.add(globalFileMeta{name: ".stfolder", sequence: 1, fileType: protocol.FileInfoTypeDirectory})
	idx.add(globalFileMeta{name: "a.txt", sequence: 2, size: 1})
	idx.add(globalFileMeta{name: "b.txt", sequence: 5, size: 3, modNanos: 1_700_000_000_123_456_789})
	idx.add(globalFileMeta{name: "dir", sequence: 4, fileType: protocol.FileInfoTypeDirectory})
	idx.add(globalFileMeta{name: "dir/.syncthing.x.tmp", sequence: 6})
	idx.add(globalFileMeta{name: "dir/~syncthing~y", sequence: 7})
	idx.add(globalFileMeta{name: "dir/nul\x00name", sequence: 8})
	idx.add(globalFileMeta{name: "gone.txt", sequence: 3, deleted: true})

	delegate := &collectingRecordDelegate{}
	result, err := folder.globalChanges(idx, 2, delegate)
	if err != nil {
		t.Fatal(err)
	}
	records := delegate.records()
	if got := paths(records); !slices.Equal(got, []string{"b.txt", "dir", "gone.txt"}) {
		t.Fatalf("unexpected records %v", got)
	}
	if result.HighestSequence != 8 || result.SequenceReset || result.Count != 3 {
		t.Fatalf("unexpected result %+v", result)
	}
	if slices.Contains(idx.infoCalls, "gone.txt") {
		t.Fatal("requested full information for a deletion")
	}

	b := records[0]
	if !b.Full || b.Size != 3 || b.ModifiedNanos != 1_700_000_000_123_456_789 || b.BlocksHash == "" || b.ModifiedBy == "" || b.Type != "file" || !b.Selected {
		t.Fatalf("unexpected file record %+v", b)
	}
	if records[1].Type != "directory" {
		t.Fatalf("unexpected directory record %+v", records[1])
	}
	if !records[2].Deleted || records[2].Full {
		t.Fatalf("unexpected deletion record %+v", records[2])
	}

	// A full pass skips the same entries
	all := &collectingRecordDelegate{}
	if _, err := folder.globalChanges(idx, 0, all); err != nil {
		t.Fatal(err)
	}
	if got := paths(all.records()); !slices.Equal(got, []string{"a.txt", "b.txt", "dir", "gone.txt"}) {
		t.Fatalf("unexpected records in a full pass %q", got)
	}
}

func TestGlobalChangesPrefersFullInformation(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &fakeGlobalIndex{}
	idx.add(globalFileMeta{name: "a.txt", sequence: 1, size: 1})
	info := idx.infos["a.txt"]
	info.Size = 10
	idx.infos["a.txt"] = info
	idx.add(globalFileMeta{name: "vanished.txt", sequence: 2, size: 1})
	delete(idx.infos, "vanished.txt")

	delegate := &collectingRecordDelegate{}
	if _, err := folder.globalChanges(idx, 0, delegate); err != nil {
		t.Fatal(err)
	}
	records := delegate.records()
	if records[0].Size != 10 {
		t.Fatalf("metadata size used instead of full information: %+v", records[0])
	}
	if !records[1].Deleted {
		t.Fatalf("entry without full information not reported as deleted: %+v", records[1])
	}
}

func TestGlobalChangesSequenceReset(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &fakeGlobalIndex{}
	idx.add(globalFileMeta{name: "a.txt", sequence: 5})
	delegate := &collectingRecordDelegate{}
	result, err := folder.globalChanges(idx, 10, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SequenceReset || result.Count != 0 || len(delegate.batches) != 0 {
		t.Fatalf("reset not detected: %+v", result)
	}

	// An empty folder at cursor zero is not a reset
	result, err = folder.globalChanges(&fakeGlobalIndex{}, 0, delegate)
	if err != nil || result.SequenceReset {
		t.Fatalf("empty folder reported as reset: %+v, %v", result, err)
	}
}

func TestGlobalChangesBatchingAndErrors(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &fakeGlobalIndex{}
	for i := range 1200 {
		idx.add(globalFileMeta{name: fmt.Sprintf("f%04d", i), sequence: int64(i + 1)})
	}
	delegate := &collectingRecordDelegate{}
	result, err := folder.globalChanges(idx, 0, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if len(delegate.batches) != 3 || len(delegate.batches[2]) != 200 || result.Count != 1200 {
		t.Fatalf("unexpected batching: %d batches, result %+v", len(delegate.batches), result)
	}

	failing := &collectingRecordDelegate{err: errors.New("disk full")}
	if _, err := folder.globalChanges(idx, 0, failing); err == nil {
		t.Fatal("delegate error was swallowed")
	}
	if _, err := folder.globalSnapshot(idx, failing); err == nil {
		t.Fatal("delegate error was swallowed by snapshot")
	}
}

func TestSelectionClassifier(t *testing.T) {
	selective, err := newFeedTestFolder(t, "(?d)*.tmp\n(?d)node_modules\n!/keep\n*\n").selectionClassifier()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		selected, hidden bool
	}{
		{"keep", true, false},
		{"keep/child.txt", true, false},
		{"other.txt", false, false},
		{"x.tmp", false, true},
		{"dir/x.tmp", false, true},
		{"node_modules/a.js", false, true},
		{"src/node_modules/a.js", false, true},
	} {
		if selected, hidden := selective.classify(tc.name); selected != tc.selected || hidden != tc.hidden {
			t.Errorf("selective %q: got selected=%v hidden=%v", tc.name, selected, hidden)
		}
	}

	regular, err := newFeedTestFolder(t, "*.log\n").selectionClassifier()
	if err != nil {
		t.Fatal(err)
	}
	if selected, hidden := regular.classify("a.log"); selected || !hidden {
		t.Errorf("ignored file in regular folder: selected=%v hidden=%v", selected, hidden)
	}
	if selected, hidden := regular.classify("a.txt"); !selected || hidden {
		t.Errorf("regular file: selected=%v hidden=%v", selected, hidden)
	}
}

func TestGlobalSnapshotAndRecords(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &fakeGlobalIndex{}
	idx.add(globalFileMeta{name: ".stignore", sequence: 1})
	idx.add(globalFileMeta{name: "a.txt", sequence: 2, size: 4})
	idx.add(globalFileMeta{name: "gone.txt", sequence: 3, deleted: true})
	idx.add(globalFileMeta{name: "invalid.txt", sequence: 4, localFlags: protocol.FlagLocalRemoteInvalid})
	idx.add(globalFileMeta{name: "nul\x00name", sequence: 5})

	delegate := &collectingRecordDelegate{}
	result, err := folder.globalSnapshot(idx, delegate)
	if err != nil {
		t.Fatal(err)
	}
	records := delegate.records()
	if got := paths(records); !slices.Equal(got, []string{"a.txt", "gone.txt", "invalid.txt"}) || result.HighestSequence != 5 {
		t.Fatalf("unexpected snapshot %v, %+v", got, result)
	}
	if records[0].Full || records[0].BlocksHash != "" || records[0].Sequence != 2 {
		t.Fatalf("snapshot record should be partial: %+v", records[0])
	}
	if !records[1].Deleted || !records[2].Hidden {
		t.Fatalf("unexpected deletion or invalid record: %+v, %+v", records[1], records[2])
	}
	if len(idx.infoCalls) != 0 {
		t.Fatal("snapshot requested full information")
	}

	js, err := folder.globalRecordsJSON(idx, []byte(`["a.txt","missing.txt",".stignore","nul\u0000name"]`))
	if err != nil {
		t.Fatal(err)
	}
	var full []fileProviderRecord
	if err := json.Unmarshal(js, &full); err != nil {
		t.Fatal(err)
	}
	if len(full) != 4 || !full[0].Full || full[0].BlocksHash == "" || !full[1].Deleted || !full[2].Deleted || !full[3].Deleted {
		t.Fatalf("unexpected records %+v", full)
	}
}

// The delegate must not be called while the iterator (a database cursor) is open
type cursorTrackingGlobalIndex struct {
	fakeGlobalIndex
	open bool
}

func (c *cursorTrackingGlobalIndex) allGlobalFiles(folderID string, yield func(globalFileMeta) bool) error {
	c.open = true
	defer func() { c.open = false }()
	return c.fakeGlobalIndex.allGlobalFiles(folderID, yield)
}

type cursorCheckingDelegate struct {
	collectingRecordDelegate
	idx             *cursorTrackingGlobalIndex
	calledWhileOpen bool
}

func (c *cursorCheckingDelegate) OnRecords(js []byte) error {
	c.calledWhileOpen = c.calledWhileOpen || c.idx.open
	return c.collectingRecordDelegate.OnRecords(js)
}

func TestGlobalSnapshotClosesCursorBeforeDelivering(t *testing.T) {
	folder := newFeedTestFolder(t, "")
	idx := &cursorTrackingGlobalIndex{}
	for i := range 1200 {
		idx.add(globalFileMeta{name: fmt.Sprintf("f%04d", i), sequence: int64(i + 1)})
	}
	delegate := &cursorCheckingDelegate{idx: idx}
	result, err := folder.globalSnapshot(idx, delegate)
	if err != nil {
		t.Fatal(err)
	}
	if delegate.calledWhileOpen || len(delegate.batches) != 3 || result.Count != 1200 {
		t.Fatalf("records delivered while the cursor was open: %v, %d batches", delegate.calledWhileOpen, len(delegate.batches))
	}
}

func TestFeedFailsWithoutIgnores(t *testing.T) {
	// A dangling include makes the ignore patterns fail to load
	folder := newFeedTestFolder(t, "#include missing.txt\n")
	idx := &fakeGlobalIndex{}
	idx.add(globalFileMeta{name: "a.txt", sequence: 1})

	delegate := &collectingRecordDelegate{}
	if _, err := folder.globalChanges(idx, 0, delegate); err == nil {
		t.Fatal("changes reported without ignore patterns")
	}
	if _, err := folder.globalSnapshot(idx, delegate); err == nil {
		t.Fatal("snapshot reported without ignore patterns")
	}
	if _, err := folder.globalRecordsJSON(idx, []byte(`["a.txt"]`)); err == nil {
		t.Fatal("records reported without ignore patterns")
	}
	if len(delegate.batches) != 0 {
		t.Fatalf("records delivered without ignore patterns: %+v", delegate.records())
	}
}
