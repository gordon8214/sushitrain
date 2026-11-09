// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"encoding/json"
	"testing"
	"time"
)

func at(start time.Time, seconds float64) time.Time {
	return start.Add(time.Duration(seconds * float64(time.Second)))
}

func TestRateMeterMeasuresTheCounter(t *testing.T) {
	var m rateMeter
	start := time.Unix(1_000_000, 0)
	if rate, files := m.observe(at(start, 0), 5000, map[string]int64{"a": 0}); rate != 0 || len(files) != 0 {
		t.Fatalf("first sample: %d, %v", rate, files)
	}
	// Under the minimum span, no speed is known yet
	if rate, files := m.observe(at(start, 0.5), 5050, map[string]int64{"a": 50}); rate != 0 || len(files) != 0 {
		t.Fatalf("after half a second: %d, %v", rate, files)
	}
	rate, files := m.observe(at(start, 2), 5200, map[string]int64{"a": 200})
	if rate != 100 || files["a"] != 100 {
		t.Fatalf("after two seconds: %d, %v", rate, files)
	}
	// a finished and b started: the counter goes on regardless, and b's own speed needs two samples
	rate, files = m.observe(at(start, 4), 5600, map[string]int64{"b": 200})
	if rate != 150 || len(files) != 0 {
		t.Fatalf("after a file finished: %d, %v", rate, files)
	}
	// A file starting over has no speed until it has moved again, and doesn't count backwards
	rate, files = m.observe(at(start, 5), 5700, map[string]int64{"b": 0})
	if rate != 140 || len(files) != 0 {
		t.Fatalf("after a file started over: %d, %v", rate, files)
	}
	if _, files = m.observe(at(start, 6), 5800, map[string]int64{"b": 100}); files["b"] != 100 {
		t.Fatalf("after starting over: %v", files)
	}
}

func TestRateMeterWindow(t *testing.T) {
	var m rateMeter
	start := time.Unix(2_000_000, 0)
	for i := range 13 {
		m.observe(at(start, float64(i)), int64(i)*10, map[string]int64{"a": int64(i) * 10})
	}
	// The window starts at the newest sample at least 10 seconds old (3), so a burst in the last second shows diluted
	rate, files := m.observe(at(start, 13), 1120, map[string]int64{"a": 1120})
	if want := int64((1120 - 30) / 10); rate != want || files["a"] != want {
		t.Fatalf("over the window: %d, %v, want %d", rate, files, want)
	}
	// A gap shorter than the stale limit is measured over the gap
	rate, files = m.observe(at(start, 25), 2320, map[string]int64{"a": 2320})
	if rate != 100 || files["a"] != 100 {
		t.Fatalf("after a 12 second gap: %d, %v", rate, files)
	}
	// A longer one starts over
	rate, files = m.observe(at(start, 60), 900_000, map[string]int64{"a": 900_000})
	if rate != 0 || len(files) != 0 {
		t.Fatalf("after a long gap: %d, %v", rate, files)
	}
	if rate, _ = m.observe(at(start, 62), 902_000, map[string]int64{"a": 902_000}); rate != 1000 {
		t.Fatalf("after the long gap: %d", rate)
	}
}

func TestRateMeterStartsWhenFilesDo(t *testing.T) {
	var m rateMeter
	start := time.Unix(3_000_000, 0)
	// Index traffic while nothing is transferred
	for i := range 10 {
		m.observe(at(start, float64(i)), int64(i), map[string]int64{})
	}
	// The window starts when files appear, so the speed isn't diluted by the idle time before
	m.observe(at(start, 10), 100, map[string]int64{"a": 0})
	if rate, _ := m.observe(at(start, 12), 4100, map[string]int64{"a": 4000}); rate != 2000 {
		t.Fatalf("after files started: %d", rate)
	}
}

func TestRateMeterKeepsItsHistoryWhenTheClockGoesBack(t *testing.T) {
	var m rateMeter
	start := time.Unix(4_000_000, 0)
	m.observe(at(start, 0), 0, map[string]int64{"a": 0})
	m.observe(at(start, 2), 2000, map[string]int64{"a": 2000})
	if rate, files := m.observe(at(start, 1), 3000, map[string]int64{"a": 3000}); rate != 1500 || files["a"] != 1500 {
		t.Fatalf("after the clock went back: %d, %v", rate, files)
	}
	// A counter that went back (a new engine) starts over
	if rate, files := m.observe(at(start, 4), 10, map[string]int64{"a": 3100}); rate != 0 || files["a"] != 0 {
		t.Fatalf("after the counter went back: %d, %v", rate, files)
	}
}

func TestMeasureTotals(t *testing.T) {
	var m rateMeter
	key := func(f FileTransfer) string { return f.Path }
	start := time.Unix(5_000_000, 0)
	var idle rateMeter
	if totals := measure([]FileTransfer{}, &idle, start, 0, key); totals != nil {
		t.Fatalf("totals of nothing: %+v", totals)
	}
	files := []FileTransfer{{Path: "a", BytesDone: 0, BytesTotal: 400}, {Path: "b", BytesDone: 100, BytesTotal: 600}}
	totals := measure(files, &m, at(start, 1), 0, key)
	if totals.FilesTotal != 2 || totals.BytesDone != 100 || totals.BytesTotal != 1000 || totals.Percentage != 0.1 ||
		totals.BytesPerSecond != 0 || totals.SecondsRemaining != -1 {
		t.Fatalf("first totals: %+v", totals)
	}
	if files[1].Percentage != 1.0/6 || files[0].Percentage != 0 {
		t.Fatalf("percentages: %+v", files)
	}
	files = []FileTransfer{{Path: "a", BytesDone: 100, BytesTotal: 400}, {Path: "b", BytesDone: 300, BytesTotal: 600}}
	totals = measure(files, &m, at(start, 3), 300, key)
	// 300 bytes in 2 seconds, 600 to go
	if totals.BytesPerSecond != 150 || totals.SecondsRemaining != 4 || files[0].BytesPerSecond != 50 ||
		files[1].BytesPerSecond != 100 {
		t.Fatalf("second totals: %+v, files %+v", totals, files)
	}
	// More done than the total (sizes that changed) doesn't make a negative time remaining
	files = []FileTransfer{{Path: "b", BytesDone: 700, BytesTotal: 600}}
	if totals = measure(files, &m, at(start, 4), 700, key); totals.SecondsRemaining != 0 {
		t.Fatalf("time remaining when done exceeds the total: %+v", totals)
	}
	// Zero sizes don't make percentages that JSON can't carry
	files = []FileTransfer{{Path: "c", BytesTotal: 0}}
	totals = measure(files, &m, at(start, 5), 800, key)
	if _, err := json.Marshal(Transfers{Download: totals, Downloads: files}); err != nil {
		t.Fatal(err)
	}
}

func TestSortTransfers(t *testing.T) {
	downloads := []FileTransfer{{Folder: "2", FolderLabel: "Photos", Path: "b"}, {Folder: "1", FolderLabel: "Docs", Path: "z"},
		{Folder: "2", FolderLabel: "Photos", Path: "a"}}
	uploads := []FileTransfer{{Device: "X", DeviceName: "Phone", Folder: "1", FolderLabel: "Docs", Path: "b"},
		{Device: "Y", DeviceName: "Laptop", Folder: "1", FolderLabel: "Docs", Path: "c"},
		{Device: "X", DeviceName: "Phone", Folder: "1", FolderLabel: "Docs", Path: "a"}}
	sortTransfers(downloads, uploads)
	if downloads[0].Path != "z" || downloads[1].Path != "a" || downloads[2].Path != "b" {
		t.Fatalf("downloads: %+v", downloads)
	}
	if uploads[0].DeviceName != "Laptop" || uploads[1].Path != "a" || uploads[2].Path != "b" {
		t.Fatalf("uploads: %+v", uploads)
	}
}
