// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
)

// The text of live progress, without GTK
func TestProgressText(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{formatPercent(0.999), "99%"},
		{formatPercent(1), "100%"},
		{formatPercent(-1), "0%"},
		{formatPercent(math.NaN()), "0%"},
		{formatRemaining(3725), "1:02:05"},
		{formatRemaining(7), "0:00:07"},
		{formatRemaining(-5), "0:00:00"},
		{transferRate(0, -1), "(Transfer speed unknown)"},
		{transferRate(1_200_000, 65), "1.2 MB/s · 0:01:05"},
		{transferRate(1000, -1), "1 kB/s"},
		{transferHeading(true, nil), "Not downloading"},
		{transferHeading(false, nil), "Not uploading"},
		{transferHeading(true, &desktopapi.TransferTotals{FilesTotal: 1}), "Receiving 1 file…"},
		{transferHeading(false, &desktopapi.TransferTotals{FilesTotal: 3}), "Sending 3 files…"},
		{transferSummary(false, nil), "Currently no files are being sent to other devices."},
		{transferSummary(true, &desktopapi.TransferTotals{BytesDone: 3e6, BytesTotal: 1e7, BytesPerSecond: 1e6, SecondsRemaining: 7}),
			"3.0 MB of 10.0 MB · 1.0 MB/s · 0:00:07"},
		{transferSummary(true, &desktopapi.TransferTotals{BytesDone: 3e6, BytesTotal: 1e7, SecondsRemaining: -1}),
			"3.0 MB of 10.0 MB · (Transfer speed unknown)"},
		{fileTransferDetail(desktopapi.FileTransfer{BytesDone: 1e6, BytesTotal: 4e6, BytesPerSecond: 5e5}), "1.0 MB of 4.0 MB · 500 kB/s"},
		{fileTransferDetail(desktopapi.FileTransfer{BytesDone: 1e6, BytesTotal: 4e6}), "1.0 MB of 4.0 MB"},
		{folderState(desktopapi.Folder{State: "syncing"}), "Synchronizing…"},
		{folderState(desktopapi.Folder{State: "idle"}), "idle"},
		{folderRemaining(desktopapi.Folder{Sync: &desktopapi.FolderSync{LocalBytes: 4e6, GlobalBytes: 1e7}}), "6.0 MB remaining"},
		{folderRemaining(desktopapi.Folder{Sync: &desktopapi.FolderSync{LocalBytes: 2e7, GlobalBytes: 1e7}}), "0 bytes remaining"},
		{folderRemaining(desktopapi.Folder{State: "syncing"}), ""},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if secondsRemaining(1, 10, 3) != 3 || secondsRemaining(0, 10, 0) != -1 || secondsRemaining(20, 10, 3) != 0 {
		t.Error("wrong time remaining")
	}
	if title, subtitle := connectionStatus(nil); title != "Not connected" || subtitle != "" {
		t.Errorf("no devices: %q, %q", title, subtitle)
	}
	if title, subtitle := connectionStatus([]desktopapi.Device{{Connected: true}, {}}); title != "Connected" || subtitle != "1 of 2 devices connected" {
		t.Errorf("one of two connected: %q, %q", title, subtitle)
	}
	if title, _ := connectionStatus([]desktopapi.Device{{}}); title != "Not connected" {
		t.Error("a device that isn't connected counts")
	}
	if (*statsProgress)(nil).totals() != nil || (&statsProgress{FilesTotal: 1}).totals() != nil {
		t.Error("an idle direction of engine.stats has totals")
	}
	if totals := (&statsProgress{FilesTotal: 2, BytesTotal: 10, BytesDone: 5, Percentage: 0.5}).totals(); totals == nil ||
		totals.FilesTotal != 2 || totals.BytesDone != 5 || totals.BytesPerSecond != 0 || totals.SecondsRemaining != -1 {
		t.Errorf("totals from engine.stats: %+v", totals)
	}

	// Downloads by folder (even when the engine's order splits one), uploads by device as "Folder: path"
	sections := transferSections(desktopapi.Transfers{Downloads: []desktopapi.FileTransfer{
		{Folder: "docs", FolderLabel: "Documents", Path: "a", BytesTotal: 10, Percentage: 0.5},
		{Folder: "photos", FolderLabel: "Photos", Path: "b", BytesTotal: 10},
		{Folder: "docs", FolderLabel: "Documents", Path: "c", BytesTotal: 10},
	}}, true)
	if len(sections) != 2 || sections[0].title != "Documents" || sections[1].title != "Photos" || len(sections[0].items) != 2 ||
		sections[0].items[0].title != "a" || sections[0].items[1].title != "c" || !sections[0].items[0].bar ||
		sections[0].items[0].fraction != 0.5 {
		t.Errorf("download sections: %+v", sections)
	}
	sections = transferSections(desktopapi.Transfers{Uploads: []desktopapi.FileTransfer{
		{Folder: "docs", FolderLabel: "Documents", Path: "a", Device: "PEER"},
		{Folder: "docs", FolderLabel: "Documents", Path: "a", Device: "OTHER", DeviceName: "Laptop"},
	}}, false)
	if len(sections) != 2 || sections[0].title != "PEER" || sections[1].title != "Laptop" ||
		sections[0].items[0].title != "Documents: a" {
		t.Errorf("upload sections: %+v", sections)
	}

	// Files mode: in the daemon's order; the ID when the path is gone; a bar once blocks are known
	items := filesTransferItems([]files.TransferProgress{
		{ID: "3", Path: "b", State: files.TransferPaused},
		{ID: "1", Path: "z", State: files.TransferDownloading, TotalBlocks: 4, VerifiedBlocks: 1, TotalBytes: 4e6, VerifiedBytes: 1e6},
		{ID: "2", State: files.TransferFailed, Error: "boom"},
	})
	if len(items) != 3 || items[0].title != "b" || items[1].title != "z" || items[2].title != "2" {
		t.Fatalf("files transfers aren't in the daemon's order: %+v", items)
	}
	if !items[1].bar || items[1].fraction != 0.25 || items[1].detail != "Download · downloading · 1.0 MB of 4.0 MB" {
		t.Errorf("running transfer: %+v", items[1])
	}
	if items[2].bar || items[2].problem != "boom" || items[2].detail != "Download · failed" {
		t.Errorf("failed transfer: %+v", items[2])
	}
	many := make([]files.TransferProgress, filesTransferLimit+2)
	for i := range many {
		many[i] = files.TransferProgress{ID: strconv.Itoa(i), State: files.TransferPaused}
	}
	if items := filesTransferItems(many); len(items) != filesTransferLimit+1 || items[filesTransferLimit].title != "2 more transfers" {
		t.Errorf("a long list isn't cut short: %d items", len(items))
	}
}

// native identifies a widget: Go wrappers of one widget can differ
func native(widget gtk.Widgetter) uintptr {
	if widget == nil {
		return 0
	}
	return coreglib.InternObject(widget).Native()
}

func findWidget(widget gtk.Widgetter, match func(*gtk.Widget) bool) *gtk.Widget {
	w := gtk.BaseWidget(widget)
	if match(w) {
		return w
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if found := findWidget(child, match); found != nil {
			return found
		}
	}
	return nil
}

// visibleRow is the shown action row with a title
func visibleRow(widget gtk.Widgetter, title string) *adw.ActionRow {
	found := findWidget(widget, func(w *gtk.Widget) bool {
		r, ok := w.Cast().(*adw.ActionRow)
		return ok && r.Title() == title && w.IsVisible()
	})
	if found == nil {
		return nil
	}
	return found.Cast().(*adw.ActionRow)
}

// visibleLabel is the shown label with a text
func visibleLabel(widget gtk.Widgetter, text string) *gtk.Label {
	found := findWidget(widget, func(w *gtk.Widget) bool {
		l, ok := w.Cast().(*gtk.Label)
		return ok && l.Text() == text && w.IsVisible()
	})
	if found == nil {
		return nil
	}
	return found.Cast().(*gtk.Label)
}

// labelTexts are the texts of the shown labels, in order
func labelTexts(widget gtk.Widgetter) []string {
	var texts []string
	findWidget(widget, func(w *gtk.Widget) bool {
		if l, ok := w.Cast().(*gtk.Label); ok && w.IsVisible() {
			texts = append(texts, l.Text())
		}
		return false
	})
	return texts
}

func indexOf(list []string, value string) int {
	for i, v := range list {
		if v == value {
			return i
		}
	}
	return -1
}

// rowBar is the progress bar of the progress row a label is in
func rowBar(label *gtk.Label) *gtk.ProgressBar {
	return navigationWidget[*gtk.ProgressBar](gtk.BaseWidget(label).Parent())
}

// waitProgressUI is waitNavigationUI for what a page's own two-second timer brings
func waitProgressUI(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("GTK update did not arrive within eight seconds")
		}
		glib.MainContextDefault().Iteration(false)
		runtime.Gosched()
	}
}

// Rows and sections of progress lists keep their widgets while their items stay
func testProgressLists(t *testing.T) {
	item := func(key string, fraction float64) progressItem {
		return progressItem{key: key, title: key, detail: "detail", fraction: fraction, bar: true}
	}
	order := func(rows *progressRows, keys ...string) {
		t.Helper()
		for i, key := range keys {
			if at := rows.list.RowAtIndex(i); at == nil || native(at) != native(rows.rows[key].row) {
				t.Fatalf("row %d isn't %s", i, key)
			}
		}
		if rows.list.RowAtIndex(len(keys)) != nil {
			t.Fatal("the list has rows of items that are gone")
		}
	}
	rows := newProgressRows("Nothing")
	rows.set([]progressItem{item("a", 0.1), item("c", 0.2)})
	a, c := rows.rows["a"], rows.rows["c"]
	rows.set([]progressItem{item("a", 0.5), item("b", 0), item("c", 0.3), item("a", 0.9)})
	if rows.rows["a"] != a || rows.rows["c"] != c {
		t.Fatal("rows of items that stayed were made again")
	}
	order(rows, "a", "b", "c")
	if a.bar.Fraction() != 0.5 || c.bar.Fraction() != 0.3 {
		t.Fatal("rows weren't updated in place")
	}
	// Only a row out of place moves: the others keep their parent (and so the focus)
	b := rows.rows["b"]
	moves := 0
	for _, r := range []*progressRow{a, b} {
		r.row.NotifyProperty("parent", func() { moves++ })
	}
	rows.set([]progressItem{item("c", 0.4), item("a", 0.6), item("b", 0)})
	order(rows, "c", "a", "b")
	if moves != 0 || rows.rows["a"] != a || rows.rows["b"] != b || rows.rows["c"] != c {
		t.Fatalf("a reordered list moved rows that were in order (%d moves), or made rows again", moves)
	}
	rows.set([]progressItem{item("c", 0.4), item("a", 0.6)})
	if rows.rows["b"] != nil || rows.rows["a"] != a || rows.rows["c"] != c {
		t.Fatal("a shorter list lost its rows' widgets or kept a row that is gone")
	}
	order(rows, "c", "a")
	if a.description != "detail" {
		t.Fatalf("a row's accessible description is %q", a.description)
	}
	rows.set([]progressItem{{key: "c", title: "c", problem: "failed"}})
	if c.bar.Visible() || !c.problem.Visible() || c.problem.Text() != "failed" || c.detail.Visible() || c.description != "failed" {
		t.Fatal("a row without progress shows a bar, or hides its problem")
	}
	rows.set(nil)
	order(rows)

	sections := newProgressSections("Not downloading", "Nothing here")
	if sections.empty.Visible() {
		t.Fatal("the empty state shows before anything is known")
	}
	sections.set(nil)
	if !sections.empty.Visible() {
		t.Fatal("an empty list doesn't say so")
	}
	section := func(key string, items ...progressItem) progressSection {
		return progressSection{key: key, title: strings.ToUpper(key), items: items}
	}
	sections.set([]progressSection{section("x", item("1", 0)), section("z", item("2", 0))})
	x := sections.sections["x"]
	sections.set([]progressSection{section("x", item("1", 0.5)), section("y", item("3", 0)), section("z", item("2", 0))})
	if sections.sections["x"] != x || x.rows.rows["1"].bar.Fraction() != 0.5 || sections.empty.Visible() {
		t.Fatal("a section that stayed was made again")
	}
	texts := labelTexts(sections.box)
	if ix, iy, iz := indexOf(texts, "X"), indexOf(texts, "Y"), indexOf(texts, "Z"); ix < 0 || !(ix < iy && iy < iz) {
		t.Fatalf("sections out of order: %v", texts)
	}
	sections.set([]progressSection{section("z", item("2", 0)), section("x", item("1", 0))})
	if texts := labelTexts(sections.box); indexOf(texts, "Y") >= 0 || indexOf(texts, "Z") > indexOf(texts, "X") {
		t.Fatalf("sections weren't removed or reordered: %v", texts)
	}
}

// The pages with live progress poll while they're in view, and update their rows in place
func testLiveProgress(t *testing.T, d *desktopUI, server *adminsock.Server) {
	// Fetches again what the page in view shows, once a fetch underway has finished, and waits for the answer
	pollNow := func() {
		t.Helper()
		busy := func() bool { poll := d.pages[len(d.pages)-1].poll; return poll != nil && poll.busy }
		waitNavigationUI(t, func() bool { return !busy() })
		d.pollVisible()
		waitNavigationUI(t, func() bool { return !busy() })
	}
	idle := desktopapi.Transfers{Downloads: []desktopapi.FileTransfer{}, Uploads: []desktopapi.FileTransfer{}}
	var transfers, failure atomic.Value
	transfers.Store(idle)
	// engine.transfers fails with this while it isn't empty
	failure.Store("")
	server.Register("engine.transfers", func(context.Context, json.RawMessage) (any, error) {
		if message := failure.Load().(string); message != "" {
			return nil, errors.New(message)
		}
		return transfers.Load(), nil
	})
	documents := desktopapi.Folder{ID: "docs", Label: "Documents", Path: "/tmp/docs", Type: "sendreceive", State: "idle"}
	var folders atomic.Value
	folders.Store([]desktopapi.Folder{documents})
	server.Register("engine.folders", func(context.Context, json.RawMessage) (any, error) { return folders.Load(), nil })
	server.Register("engine.status", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"deviceID": "DEVICE", "name": "Test", "state": "running"}, nil
	})
	var stats atomic.Value
	stats.Store(map[string]any{})
	server.Register("engine.stats", func(context.Context, json.RawMessage) (any, error) { return stats.Load(), nil })
	server.Register("engine.devices", func(context.Context, json.RawMessage) (any, error) {
		return []desktopapi.Device{{ID: "PEER", Name: "Phone", Connected: true}, {ID: "OTHER", Name: "Laptop"}}, nil
	})
	// What engine.inspect says about each file (not here, by default), or its error
	var entries sync.Map
	server.Register("engine.inspect", func(_ context.Context, raw json.RawMessage) (any, error) {
		var request desktopapi.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		entry, _ := entries.Load(request.Path)
		if err, ok := entry.(error); ok {
			return nil, err
		}
		e, _ := entry.(desktopapi.Entry)
		e.Path = request.Path
		return map[string]any{"entry": e}, nil
	})
	receiving := desktopapi.Transfers{
		Download: &desktopapi.TransferTotals{FilesTotal: 2, BytesDone: 3e6, BytesTotal: 1e7, Percentage: 0.3,
			BytesPerSecond: 1e6, SecondsRemaining: 7},
		Downloads: []desktopapi.FileTransfer{
			{Folder: "docs", FolderLabel: "Documents", Path: "a.txt", BytesDone: 1e6, BytesTotal: 4e6, Percentage: 0.25, BytesPerSecond: 5e5},
			{Folder: "photos", FolderLabel: "Photos", Path: "b.jpg", BytesDone: 2e6, BytesTotal: 6e6, Percentage: 1.0 / 3},
		},
		Uploads: []desktopapi.FileTransfer{},
	}

	// Activity's transfer rows follow the engine, keeping their widgets
	d.mode, d.route = modeSynchronize, "Activity"
	d.showRoute()
	waitNavigationUI(t, func() bool {
		return visibleRow(d.body, "Not downloading") != nil && visibleRow(d.body, "Not uploading") != nil
	})
	downloads := visibleRow(d.body, "Not downloading")
	transfers.Store(receiving)
	pollNow()
	waitNavigationUI(t, func() bool { return visibleRow(d.body, "Receiving 2 files…") != nil })
	if r := visibleRow(d.body, "Receiving 2 files…"); native(r) != native(downloads) || r.Subtitle() != "3.0 MB of 10.0 MB · 1.0 MB/s · 0:00:07" {
		t.Fatalf("the Downloads row wasn't updated in place: %q", r.Subtitle())
	}

	// Its list groups the files by folder, and follows them too
	navigationButton(d.body, "Open Downloads").Emit("clicked")
	waitNavigationUI(t, func() bool { return len(d.pages) == 2 && visibleLabel(d.body, "a.txt") != nil })
	if d.navigation.VisiblePage().Title() != "Downloads" || visibleLabel(d.body, "Documents") == nil || visibleLabel(d.body, "Photos") == nil {
		t.Fatal("the downloads aren't listed by folder")
	}
	a := visibleLabel(d.body, "a.txt")
	next := receiving
	next.Downloads = []desktopapi.FileTransfer{
		{Folder: "docs", FolderLabel: "Documents", Path: "a.txt", BytesDone: 3e6, BytesTotal: 4e6, Percentage: 0.75},
		{Folder: "docs", FolderLabel: "Documents", Path: "c.txt", BytesTotal: 1e6},
		receiving.Downloads[1],
	}
	transfers.Store(next)
	pollNow()
	waitNavigationUI(t, func() bool { return visibleLabel(d.body, "c.txt") != nil })
	if native(visibleLabel(d.body, "a.txt")) != native(a) || rowBar(a).Fraction() != 0.75 {
		t.Fatal("a.txt's row was made again, or not updated")
	}
	texts := labelTexts(d.body)
	if !(indexOf(texts, "Documents") < indexOf(texts, "a.txt") && indexOf(texts, "a.txt") < indexOf(texts, "c.txt") &&
		indexOf(texts, "c.txt") < indexOf(texts, "Photos")) {
		t.Fatalf("a new download isn't in its place: %v", texts)
	}
	transfers.Store(idle)
	pollNow()
	waitNavigationUI(t, func() bool {
		empty := navigationWidget[*adw.StatusPage](d.body)
		return empty != nil && empty.IsVisible() && empty.Title() == "Not downloading" && visibleLabel(d.body, "a.txt") == nil
	})

	// Back on Activity, its rows catch up at once
	d.goBack()
	waitNavigationUI(t, func() bool { return len(d.pages) == 1 && visibleRow(d.body, "Not downloading") != nil })

	// A failing poll: the banner says why and the rows show nothing stale, until a poll succeeds again
	transfers.Store(receiving)
	pollNow()
	waitNavigationUI(t, func() bool { return visibleRow(d.body, "Receiving 2 files…") != nil })
	failure.Store("the engine stopped")
	pollNow()
	visibleBar := func() bool {
		return findWidget(d.body, func(w *gtk.Widget) bool { _, ok := w.Cast().(*gtk.ProgressBar); return ok && w.IsVisible() }) != nil
	}
	if !d.banner.Revealed() || visibleRow(d.body, "Downloads") == nil || visibleRow(d.body, "Receiving 2 files…") != nil || visibleBar() {
		t.Fatal("a failed poll left its rows as they were, or shows no error")
	}
	failure.Store("")
	pollNow()
	if d.banner.Revealed() || visibleRow(d.body, "Receiving 2 files…") == nil {
		t.Fatal("the banner outlived the failure")
	}
	// Its banner also goes when the page is shown again, by a poll that hasn't failed
	failure.Store("the engine stopped")
	pollNow()
	failure.Store("")
	d.refreshView()
	if d.banner.Revealed() {
		t.Fatal("a failed poll's banner outlived its page")
	}

	// An engine older than the window, without engine.transfers: totals from engine.stats, no speed, no banner
	failure.Store(`unknown method "engine.transfers"`)
	stats.Store(map[string]any{"download": map[string]any{"FilesTotal": 2, "BytesTotal": 1e7, "BytesDone": 3e6, "Percentage": 0.3}})
	pollNow()
	waitNavigationUI(t, func() bool { return visibleRow(d.body, "Receiving 2 files…") != nil })
	if r := visibleRow(d.body, "Receiving 2 files…"); r.Subtitle() != "3.0 MB of 10.0 MB · (Transfer speed unknown)" || d.banner.Revealed() {
		t.Fatalf("an older engine's totals: %q", r.Subtitle())
	}
	navigationButton(d.body, "Open Downloads").Emit("clicked")
	waitNavigationUI(t, func() bool {
		empty := navigationWidget[*adw.StatusPage](d.body)
		return len(d.pages) == 2 && empty != nil && empty.IsVisible() && empty.Title() == "Files Not Listed"
	})
	if d.banner.Revealed() {
		t.Fatal("an older engine's list shows an error")
	}
	d.goBack()
	failure.Store("")
	stats.Store(map[string]any{})
	transfers.Store(idle)
	pollNow()

	// Uploads are listed by device, each as "Folder: path"
	sending := idle
	sending.Upload = &desktopapi.TransferTotals{FilesTotal: 1, BytesDone: 1, BytesTotal: 2, Percentage: 0.5, SecondsRemaining: -1}
	sending.Uploads = []desktopapi.FileTransfer{{Folder: "docs", FolderLabel: "Documents", Path: "a.txt", Device: "PEER",
		DeviceName: "Phone", BytesDone: 1, BytesTotal: 2, Percentage: 0.5}}
	transfers.Store(sending)
	navigationButton(d.body, "Open Uploads").Emit("clicked")
	waitNavigationUI(t, func() bool {
		return visibleLabel(d.body, "Documents: a.txt") != nil && visibleLabel(d.body, "Phone") != nil
	})
	d.goBack()
	waitNavigationUI(t, func() bool { return visibleRow(d.body, "Sending 1 file…") != nil })

	// Overview: what is received, or else whether other devices are connected
	transfers.Store(receiving)
	d.route = "Overview"
	d.showRoute()
	waitNavigationUI(t, func() bool {
		return visibleRow(d.body, "Receiving 2 files…") != nil && visibleRow(d.body, "Documents") != nil
	})
	// The receiving row is the way to the downloads; the uploads have their link
	if visibleRow(d.body, "Connected") != nil || visibleRow(d.body, "Not uploading") != nil ||
		visibleRow(d.body, "Downloads") != nil || visibleRow(d.body, "Uploads") == nil {
		t.Fatal("the overview shows idle rows next to a transfer, or two ways to one list")
	}
	transfers.Store(idle)
	pollNow()
	waitNavigationUI(t, func() bool { return visibleRow(d.body, "Connected") != nil })
	if visibleRow(d.body, "Connected").Subtitle() != "1 of 2 devices connected" || visibleRow(d.body, "Not downloading") != nil ||
		visibleRow(d.body, "Downloads") == nil || visibleRow(d.body, "Uploads") == nil {
		t.Fatal("the overview's connection status is wrong")
	}
	// A failing poll hides what it can't say any more
	transfers.Store(receiving)
	pollNow()
	failure.Store("the engine stopped")
	pollNow()
	if visibleRow(d.body, "Receiving 2 files…") != nil || visibleRow(d.body, "Connected") != nil || visibleRow(d.body, "Downloads") == nil {
		t.Fatal("the overview kept rows its failed poll can't vouch for")
	}
	failure.Store("")
	transfers.Store(idle)
	pollNow()

	// Folders: a syncing folder shows how far it has come, in its row
	d.route = "Folders"
	d.showRoute()
	waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 && navigationWidget[*adw.ExpanderRow](d.body) != nil })
	folder := navigationWidget[*adw.ExpanderRow](d.body)
	if visibleLabel(d.body, "6.0 MB remaining") != nil || navigationButton(d.body, "Pause") == nil {
		t.Fatal("an idle folder shows progress, or can't be paused")
	}
	syncing := documents
	syncing.State, syncing.Paused = "syncing", true
	syncing.Sync = &desktopapi.FolderSync{LocalBytes: 4e6, GlobalBytes: 1e7, NeedBytes: 6e6, NeedFiles: 3, Fraction: 0.4}
	folders.Store([]desktopapi.Folder{syncing})
	pollNow()
	waitNavigationUI(t, func() bool { return visibleLabel(d.body, "6.0 MB remaining") != nil })
	if native(navigationWidget[*adw.ExpanderRow](d.body)) != native(folder) || !strings.Contains(folder.Subtitle(), "Synchronizing…") ||
		navigationButton(d.body, "Resume") == nil {
		t.Fatal("the folder's row wasn't updated in place")
	}
	if bar := navigationWidget[*gtk.ProgressBar](d.body); bar == nil || bar.Fraction() != 0.4 || bar.Text() != "40%" {
		t.Fatal("the folder's progress bar is wrong")
	}
	music := documents
	music.ID, music.Label = "music", "Music"
	folders.Store([]desktopapi.Folder{syncing, music})
	pollNow()
	waitNavigationUI(t, func() bool {
		return findWidget(d.body, func(w *gtk.Widget) bool {
			r, ok := w.Cast().(*adw.ExpanderRow)
			return ok && r.Title() == "Music"
		}) != nil
	})

	// A file's information: how it is received, then nothing once it's here; one that isn't here waits
	d.route, d.folder = "Files", "docs"
	d.showRoute()
	waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
	transfers.Store(receiving)
	information := func(r browserRow) {
		t.Helper()
		if len(d.pages) == 3 {
			d.goBack()
			d.goBack()
		}
		d.fileActions(r)
		button := navigationButton(d.commands, "Information")
		if button == nil {
			t.Fatal("file actions have no Information")
		}
		button.Emit("clicked")
		waitNavigationUI(t, func() bool { return len(d.pages) == 3 && d.navigation.VisiblePage().Title() == "Information" })
		pollNow()
	}
	information(browserRow{name: "a.txt", path: "a.txt", detail: "4.0 MB"})
	if r := visibleRow(d.body, "Downloading file…"); r == nil || r.Subtitle() != "1.0 MB of 4.0 MB · 500 kB/s · 0:00:06" {
		t.Fatal("the file's download isn't shown")
	}
	entries.Store("a.txt", desktopapi.Entry{Local: true})
	transfers.Store(idle)
	pollNow()
	if visibleRow(d.body, "Downloading file…") != nil || visibleRow(d.body, "Waiting to synchronize…") != nil {
		t.Fatal("a downloaded file still shows a status")
	}
	// One that arrives without its download being seen
	information(browserRow{name: "b.txt", path: "b.txt", detail: "1 kB"})
	if visibleRow(d.body, "Waiting to synchronize…") == nil {
		t.Fatal("a file that isn't here doesn't wait")
	}
	entries.Store("b.txt", desktopapi.Entry{Local: true})
	pollNow()
	if visibleRow(d.body, "Waiting to synchronize…") != nil {
		t.Fatal("a file that arrived still waits")
	}
	// One that the ignore patterns leave out, known from browsing (when engine.inspect can't tell) or from engine.inspect
	entries.Store("d.txt", errors.New("file no longer exists"))
	information(browserRow{name: "d.txt", path: "d.txt", ignored: true})
	if visibleRow(d.body, "Waiting to synchronize…") != nil {
		t.Fatal("a file browsing found ignored waits to synchronize")
	}
	entries.Store("e.txt", desktopapi.Entry{Ignored: true})
	information(browserRow{name: "e.txt", path: "e.txt"})
	if visibleRow(d.body, "Waiting to synchronize…") != nil || visibleRow(d.body, "Downloading file…") != nil {
		t.Fatal("an ignored file waits to synchronize")
	}
	information(browserRow{name: "c.txt", path: "c.txt", native: "/tmp/docs/c.txt"})
	if visibleRow(d.body, "Waiting to synchronize…") != nil || visibleRow(d.body, "Downloading file…") != nil {
		t.Fatal("a file that is here waits to synchronize")
	}
	// An engine without engine.transfers: still no banner
	failure.Store(`unknown method "engine.transfers"`)
	information(browserRow{name: "f.txt", path: "f.txt"})
	if visibleRow(d.body, "Waiting to synchronize…") == nil || d.banner.Revealed() {
		t.Fatal("an older engine's file information shows an error, or no status")
	}
	failure.Store("")
	d.goBack()
	d.goBack()

	// Files mode: Activity lists the downloads with their blocks and errors, live
	var filesTransfers atomic.Value
	filesTransfers.Store([]files.TransferProgress{})
	server.Register("transfers", func(context.Context, json.RawMessage) (any, error) {
		return filesTransfers.Load(), nil
	})
	server.Register("status", func(context.Context, json.RawMessage) (any, error) {
		return files.Status{Mount: "/tmp/desktop-ui-fixture"}, nil
	})
	server.Register("pending.list", func(context.Context, json.RawMessage) (any, error) { return []files.Edit{}, nil })
	server.Register("storage", func(context.Context, json.RawMessage) (any, error) { return files.Storage{}, nil })
	d.mode, d.route = modeFiles, "Activity"
	d.showRoute()
	if visibleLabel(d.detail, "Transfers") != nil {
		t.Fatal("Files mode's transfers show before the daemon said what they are")
	}
	waitNavigationUI(t, func() bool { return visibleLabel(d.detail, "No recent transfers") != nil })
	running := files.TransferProgress{ID: "t1", Item: 7, Path: "Docs/a.txt", TotalBytes: 4e6, VerifiedBytes: 1e6, VerifiedBlocks: 1,
		TotalBlocks: 4, State: files.TransferDownloading}
	filesTransfers.Store([]files.TransferProgress{running, {ID: "t2", State: files.TransferFailed, Error: "gateway out of reach"}})
	waitProgressUI(t, func() bool { return visibleLabel(d.detail, "Docs/a.txt") != nil })
	path := visibleLabel(d.detail, "Docs/a.txt")
	problem := visibleLabel(d.detail, "gateway out of reach")
	if visibleLabel(d.detail, "t2") == nil || problem == nil || !problem.HasCSSClass("error") ||
		visibleLabel(d.detail, "Download · downloading · 1.0 MB of 4.0 MB") == nil || rowBar(path).Fraction() != 0.25 {
		t.Fatal("Files mode's transfers aren't shown")
	}
	running.VerifiedBlocks, running.VerifiedBytes = 3, 3e6
	filesTransfers.Store([]files.TransferProgress{running})
	waitProgressUI(t, func() bool { return visibleLabel(d.detail, "t2") == nil })
	if native(visibleLabel(d.detail, "Docs/a.txt")) != native(path) || rowBar(path).Fraction() != 0.75 {
		t.Fatal("a Files-mode transfer's row wasn't updated in place")
	}
	// A daemon older than the window: no transfers, and no error about them
	server.Register("transfers", func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New(`unknown method "transfers"`)
	})
	waitProgressUI(t, func() bool { return visibleLabel(d.detail, "Transfers") == nil })
	if visibleLabel(d.detail, `Unknown method "transfers"`) != nil {
		t.Fatal("Files mode shows an older daemon's error")
	}
	d.route = "Overview"
	d.showRoute()
}
