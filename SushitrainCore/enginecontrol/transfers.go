// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	core "t-shaped.nl/sushitrain/v2/src"
)

type FileTransfer = desktopapi.FileTransfer
type TransferTotals = desktopapi.TransferTotals
type Transfers = desktopapi.Transfers

const (
	// Speeds are measured over this long
	rateWindow = 10 * time.Second
	// A speed needs samples at least this far apart
	rateMinimumSpan = time.Second
	// After this long without a sample, a meter starts over: a speed averaged over the gap isn't the current one
	rateStaleAfter = 15 * time.Second
	// Callers within this long of each other get the same sample, so many windows cost no more than one
	transfersReuse = 500 * time.Millisecond
)

// transferSampler takes the samples of the engine's transfers, one at a time, and keeps the last for a moment
type transferSampler struct {
	mu                       sync.Mutex
	last                     *Transfers
	lastAt                   time.Time
	downloadRate, uploadRate rateMeter
}

// transfers lists the files the engine receives and sends, with their totals and speeds. Samples are taken one at a
// time and shared for a moment, so callers that ask at once all see the same speeds.
func (c *Control) transfers() Transfers {
	s := &c.transferSampler
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.last != nil && now.Sub(s.lastAt) < transfersReuse && !now.Before(s.lastAt) {
		return *s.last
	}
	// What every connection received and sent, which is what Syncthing's own GUI measures its rates by
	in, out := protocol.TotalInOut()
	t := collectTransfers(c.Client, &s.downloadRate, &s.uploadRate, now, in, out)
	s.last, s.lastAt = &t, now
	return t
}

func collectTransfers(cl *core.Client, downloadRate, uploadRate *rateMeter, now time.Time, in, out int64) Transfers {
	t := Transfers{Downloads: []FileTransfer{}, Uploads: []FileTransfer{}}
	labels := map[string]string{}
	label := func(folderID string) string {
		if l, ok := labels[folderID]; ok {
			return l
		}
		l := folderID
		if f := cl.FolderWithID(folderID); f != nil && f.Label() != "" {
			l = f.Label()
		}
		labels[folderID] = l
		return l
	}

	for _, folderID := range Strings(cl.DownloadingFolders()) {
		for _, path := range Strings(cl.DownloadingPathsForFolder(folderID)) {
			if p := cl.GetDownloadProgressForFile(path, folderID); p != nil && p.BytesTotal > 0 {
				t.Downloads = append(t.Downloads, FileTransfer{Folder: folderID, FolderLabel: label(folderID), Path: path,
					BytesDone: p.BytesDone, BytesTotal: p.BytesTotal})
			}
		}
	}
	for _, deviceID := range Strings(cl.UploadingToPeers()) {
		name := deviceID
		if peer := cl.PeerWithID(deviceID); peer != nil {
			name = peer.Name()
			if name == "" {
				name = peer.ShortDeviceID()
			}
		}
		for _, folderID := range Strings(cl.UploadingFoldersForPeer(deviceID)) {
			for _, path := range Strings(cl.UploadingFilesForPeerAndFolder(deviceID, folderID)) {
				if p := cl.UploadProgressForPeerFolderPath(deviceID, folderID, path); p != nil && p.BytesTotal > 0 {
					t.Uploads = append(t.Uploads, FileTransfer{Folder: folderID, FolderLabel: label(folderID), Path: path,
						Device: deviceID, DeviceName: name, BytesDone: p.BytesDone, BytesTotal: p.BytesTotal})
				}
			}
		}
	}

	sortTransfers(t.Downloads, t.Uploads)
	t.Download = measure(t.Downloads, downloadRate, now, in, downloadKey)
	t.Upload = measure(t.Uploads, uploadRate, now, out, uploadKey)
	return t
}

func downloadKey(t FileTransfer) string { return t.Folder + "\x00" + t.Path }
func uploadKey(t FileTransfer) string   { return t.Device + "\x00" + t.Folder + "\x00" + t.Path }

// sortTransfers puts downloads in order of folder and path, and uploads in order of device, folder and path
func sortTransfers(downloads, uploads []FileTransfer) {
	slices.SortFunc(downloads, func(a, b FileTransfer) int {
		return cmp.Or(cmp.Compare(a.FolderLabel, b.FolderLabel), cmp.Compare(a.Folder, b.Folder), cmp.Compare(a.Path, b.Path))
	})
	slices.SortFunc(uploads, func(a, b FileTransfer) int {
		return cmp.Or(cmp.Compare(a.DeviceName, b.DeviceName), cmp.Compare(a.Device, b.Device),
			cmp.Compare(a.FolderLabel, b.FolderLabel), cmp.Compare(a.Folder, b.Folder), cmp.Compare(a.Path, b.Path))
	})
}

// measure fills in the files' percentages and speeds, and returns their totals (nil for none). The speed of all of them
// comes from counter, the bytes that went that way on every connection.
func measure(files []FileTransfer, meter *rateMeter, now time.Time, counter int64, key func(FileTransfer) string) *TransferTotals {
	sample := make(map[string]int64, len(files))
	for _, f := range files {
		sample[key(f)] = f.BytesDone
	}
	rate, fileRates := meter.observe(now, counter, sample)
	if len(files) == 0 {
		return nil
	}
	totals := &TransferTotals{FilesTotal: int64(len(files)), BytesPerSecond: rate, SecondsRemaining: -1}
	for i := range files {
		f := &files[i]
		f.Percentage = fraction(f.BytesDone, f.BytesTotal)
		f.BytesPerSecond = fileRates[key(*f)]
		totals.BytesDone += f.BytesDone
		totals.BytesTotal += f.BytesTotal
	}
	totals.Percentage = fraction(totals.BytesDone, totals.BytesTotal)
	if rate > 0 {
		left := max(0, totals.BytesTotal-totals.BytesDone)
		totals.SecondsRemaining = (left + rate - 1) / rate
	}
	return totals
}

func fraction(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(1, max(0, float64(done)/float64(total)))
}

type ratePoint struct {
	at    time.Time
	value int64
}

// rateMeter measures a direction's speed from a counter of the bytes that went that way, and each file's speed from
// its bytes done. Samples must come one at a time.
type rateMeter struct {
	last    time.Time
	counter []ratePoint
	files   map[string][]ratePoint
	rate    int64
	// Whether the last sample had no files, so the counter only moved for other traffic
	idle bool
}

func (m *rateMeter) observe(now time.Time, counter int64, sample map[string]int64) (int64, map[string]int64) {
	if !m.last.IsZero() && now.Before(m.last) {
		// The clock went back: the newest point stays the newest
		now = m.last
	}
	if m.last.IsZero() || now.Sub(m.last) > rateStaleAfter ||
		(len(m.counter) > 0 && counter < m.counter[len(m.counter)-1].value) {
		m.counter, m.files, m.rate = nil, map[string][]ratePoint{}, 0
	} else if m.idle && len(sample) > 0 {
		// Files started moving: the time before doesn't count, or the speed would take the window to catch up
		m.counter, m.rate = nil, 0
	}
	m.idle = len(sample) == 0
	m.last = now
	m.counter = trimPoints(append(m.counter, ratePoint{now, counter}), now)
	if r, ok := speed(m.counter); ok {
		m.rate = r
	}

	fileRates := make(map[string]int64, len(sample))
	for key := range m.files {
		if _, ok := sample[key]; !ok {
			delete(m.files, key)
		}
	}
	for key, done := range sample {
		points := m.files[key]
		if len(points) > 0 && done < points[len(points)-1].value {
			// Started over: its earlier progress says nothing about now
			points = points[:0]
		}
		points = trimPoints(append(points, ratePoint{now, done}), now)
		m.files[key] = points
		if r, ok := speed(points); ok {
			fileRates[key] = r
		}
	}
	return m.rate, fileRates
}

// trimPoints keeps the points in the window, and the newest one before it as where the window starts
func trimPoints(points []ratePoint, now time.Time) []ratePoint {
	first := 0
	for first+1 < len(points) && now.Sub(points[first+1].at) >= rateWindow {
		first++
	}
	if first == 0 {
		return points
	}
	n := copy(points, points[first:])
	return points[:n]
}

func speed(points []ratePoint) (int64, bool) {
	if len(points) < 2 {
		return 0, false
	}
	oldest, newest := points[0], points[len(points)-1]
	span := newest.at.Sub(oldest.at)
	if span < rateMinimumSpan {
		return 0, false
	}
	return max(0, int64(float64(newest.value-oldest.value)/span.Seconds())), true
}

// FolderSyncProgress is how far a syncing folder has come, and nil for one in any other state or whose sizes can't be
// read
func FolderSyncProgress(f *core.Folder, state string) *desktopapi.FolderSync {
	if state != "syncing" {
		return nil
	}
	stats, err := f.Statistics()
	if err != nil || stats == nil || stats.Global == nil || stats.Local == nil {
		return nil
	}
	out := &desktopapi.FolderSync{LocalBytes: stats.Local.Bytes, GlobalBytes: stats.Global.Bytes}
	if stats.LocalNeed != nil {
		out.NeedBytes, out.NeedFiles = stats.LocalNeed.Bytes, stats.LocalNeed.Files
	}
	out.Fraction = fraction(out.LocalBytes, out.GlobalBytes)
	return out
}
