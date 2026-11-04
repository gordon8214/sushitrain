// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"io"
	"strings"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"testing"
	"time"
)

type failedCapture struct{ done bool }

func (r *failedCapture) Read(data []byte) (int, error) {
	if r.done {
		return 0, errors.New("capture failed")
	}
	r.done = true
	return copy(data, "partial"), nil
}

func TestSnapshotImportIsAtomic(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "original"})
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/document.txt")
	if _, err := ts.ImportSnapshot(context.Background(), entry, &failedCapture{}, 100, time.Now()); err == nil {
		t.Fatal("failed capture was accepted")
	}
	if got := string(ts.read(ts.mustLookup("docs/document.txt"))); got != "original" {
		t.Fatalf("failed capture changed the file: %s", got)
	}
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("failed capture scheduled an upload: %+v %v", edits, err)
	}
	updated, err := ts.ImportSnapshot(context.Background(), entry, strings.NewReader("complete edit"), 13, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Item != entry.Item || updated.Node == entry.Node {
		t.Fatal("snapshot did not preserve item identity and advance its content node")
	}
	if got := string(ts.read(updated)); got != "complete edit" {
		t.Fatalf("read after importing: %s", got)
	}
	ts.settled()
	ts.gatewayHas("document.txt", "complete edit")
}
func TestSnapshotImportRejectsStaleBase(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "version one"})
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/document.txt")
	g.Engine.Write("document.txt", "version two")
	ts.refresh()
	if _, err := ts.ImportSnapshot(context.Background(), entry, strings.NewReader("old local edit"), 14, time.Now()); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("stale edit was rebased: %v", err)
	}
	if got := string(ts.read(ts.mustLookup("docs/document.txt"))); got != "version two" {
		t.Fatalf("stale capture replaced the newer version: %s", got)
	}
	if _, err := ts.ImportSnapshot(context.Background(), ts.mustLookup("docs/document.txt"), strings.NewReader("short"), 10, time.Now()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short capture: %v", err)
	}
}

func TestSnapshotReceiptReplay(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "original"})
	ts := newTestService(t, g, 0)
	original := ts.mustLookup("docs/document.txt")
	first, err := ts.ImportSnapshot(context.Background(), original, strings.NewReader("edit"), 4, time.Now(), "capture-one")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := ts.ImportSnapshot(context.Background(), original, strings.NewReader("edit"), 4, time.Now(), "capture-one")
	if err != nil {
		t.Fatal(err)
	}
	if first.Item != replay.Item || first.Node != replay.Node {
		t.Fatalf("capture replay allocated another version: %+v %+v", first, replay)
	}
	receipt, ok, err := ts.SnapshotReceipt("capture-one")
	if err != nil || !ok || receipt.Node != first.Node {
		t.Fatalf("receipt: %+v %v %v", receipt, ok, err)
	}
	if err := ts.AcknowledgeSnapshot("capture-one"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ts.SnapshotReceipt("capture-one"); err != nil || ok {
		t.Fatalf("ack: %v %v", ok, err)
	}
	parent := ts.mustLookup("docs")
	if _, err := ts.CreateSnapshot(context.Background(), parent.Item, "failed.txt", false, &failedCapture{}, 100, time.Now(), "bad-capture"); err == nil {
		t.Fatal("failed creation accepted")
	}
	if _, ok, err := ts.Lookup(parent.Item, "failed.txt"); err != nil || ok {
		t.Fatalf("failed creation left an empty file: %v %v", ok, err)
	}
	if _, err := ts.Remove(parent.Item, original.Name, false, original); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("stale delete: %v", err)
	}
	if _, err := ts.Rename(parent.Item, original.Name, parent.Item, "renamed.txt", true, original); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("stale rename: %v", err)
	}
	ts.settled()
	ts.gatewayHas("document.txt", "edit")
}

func TestSnapshotReceiptKeepsCapturedVersion(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "original"})
	ts := newTestService(t, g, 0)
	original := ts.mustLookup("docs/document.txt")
	captured, err := ts.ImportSnapshot(context.Background(), original, strings.NewReader("local capture"), 13, time.Now(), "lost-response")
	if err != nil {
		t.Fatal(err)
	}
	ts.settled()
	g.Engine.Write("document.txt", "newer remote content")
	ts.refresh()
	current := ts.mustLookup("docs/document.txt")
	if current.Node == captured.Node {
		t.Fatal("fixture did not advance remote version")
	}
	ts.restart()
	receipt, ok, err := ts.SnapshotReceipt("lost-response")
	if err != nil || !ok || receipt.Node != captured.Node {
		t.Fatalf("receipt rebound old bytes to a newer version: %+v %v %v", receipt, ok, err)
	}
}

func TestNativeDirectoryCreateAndRenameReplay(t *testing.T) {
	g := writableGateway(t, map[string]string{})
	ts := newTestService(t, g, 0)
	root := ts.mustLookup("docs")
	first, err := ts.CreateOnce(root.Item, "new directory", true, "native-directory-one")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ts.CreateOnce(root.Item, "new directory", true, "native-directory-one")
	if err != nil || first.Item != again.Item || first.Node != again.Node {
		t.Fatalf("directory replay: %+v %+v %v", first, again, err)
	}
	if _, err := ts.Rename(root.Item, first.Name, root.Item, "renamed directory", true, first); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Rename(root.Item, first.Name, root.Item, "renamed directory", true, first); err != nil {
		t.Fatal("lost rename response", err)
	}
	if _, err := ts.Remove(root.Item, "renamed directory", true, first); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Remove(root.Item, "renamed directory", true, first); err != nil {
		t.Fatal("lost delete response", err)
	}
	if err := ts.AcknowledgeCreate("native-directory-one"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMoveReplayAfterSourceParentRemoved(t *testing.T) {
	g := writableGateway(t, map[string]string{"source/file.txt": "retained", "target/other.txt": "anchor"})
	ts := newTestService(t, g, 0)
	root := ts.mustLookup("docs")
	source := ts.mustLookup("docs/source")
	target := ts.mustLookup("docs/target")
	entry := ts.mustLookup("docs/source/file.txt")
	if _, err := ts.Rename(source.Item, entry.Name, target.Item, entry.Name, true, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Remove(root.Item, source.Name, true, source); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Rename(source.Item, entry.Name, target.Item, entry.Name, true, entry); err != nil {
		t.Fatal("replaying the committed move after its old parent disappeared:", err)
	}
	if actual := ts.mustLookup("docs/target/file.txt"); actual.Item != entry.Item {
		t.Fatalf("replay changed the moved item: %+v", actual)
	}
}

func TestUnchangedRecoveryCaptureKeepsTheIndexedVersion(t *testing.T) {
	g := writableGateway(t, map[string]string{"document.txt": "unchanged content"})
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/document.txt")
	// Verification is also allowed when the captured version is read-only.
	entry.Writable = false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	matched, err := ts.RecordUnchangedSnapshot(ctx, entry, strings.NewReader("unchanged content"), 17, "recovered")
	if err != nil || !matched {
		t.Fatalf("unchanged capture: %v %v", matched, err)
	}
	receipt, ok, err := ts.SnapshotReceipt("recovered")
	if err != nil || !ok || receipt.Node != entry.Node {
		t.Fatalf("recovery receipt: %+v %v %v", receipt, ok, err)
	}
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("unchanged recovery queued an upload: %+v %v", edits, err)
	}
	canceled, cancelRead := context.WithCancel(ctx)
	cancelRead()
	if _, err := ts.RecordUnchangedSnapshot(canceled, entry, strings.NewReader("unchanged content"), 17, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached verification ignored cancellation: %v", err)
	}
	matched, err = ts.RecordUnchangedSnapshot(ctx, entry, strings.NewReader("different content"), 17, "changed")
	if err != nil || matched {
		t.Fatalf("changed capture accepted: %v %v", matched, err)
	}
	if _, ok, err := ts.SnapshotReceipt("changed"); err != nil || ok {
		t.Fatalf("changed capture receipt exists: %v %v", ok, err)
	}
}

type pausedHistoryCopy struct {
	entered, resume chan struct{}
	reader          io.Reader
	first           bool
}

func (r *pausedHistoryCopy) Read(data []byte) (int, error) {
	if !r.first {
		r.first = true
		close(r.entered)
		<-r.resume
	}
	return r.reader.Read(data)
}
func TestHistoryRestoreDoesNotReplaceEditDuringCopy(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "simple", Params: map[string]string{"keep": "5"}}
	})
	e.Write("report.txt", "original")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/report.txt")
	capture, err := ts.CaptureHistoryRestore(t.Context(), entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	g.Stop()
	source := &pausedHistoryCopy{entered: make(chan struct{}), resume: make(chan struct{}), reader: strings.NewReader("archived")}
	done := make(chan error, 1)
	go func() {
		_, err := ts.importSnapshot(t.Context(), entry, 0, "", false, source, 8, time.Now(), "", &capture)
		done <- err
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("history copy never began")
	}
	ts.write(entry, "intervening edit", true)
	close(source.resume)
	select {
	case err = <-done:
		if !errors.Is(err, ErrPendingChange) {
			t.Fatal("restore accepted an intervening edit", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore deadlocked")
	}
	if bytes := string(ts.read(ts.mustLookup("docs/report.txt"))); bytes != "intervening edit" {
		t.Fatal("restore lost local edit", bytes)
	}
}
