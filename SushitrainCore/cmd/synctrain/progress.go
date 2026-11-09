// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

// Live sync progress: the files the engine receives and sends (engine.transfers), how far syncing folders have come
// (engine.folders), and the downloads of Files mode (transfers). A page that shows it polls while it's in
// view, and updates its rows in place.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
)

// pagePoll fetches what a page shows again, while the page is in view
type pagePoll struct {
	run  func()
	busy bool
}

// pollPage shows what fetch gets on the current page, every two seconds while the page is in view (the window's
// timer calls pollVisible), and returns a function that fetches it at once. A failed request shows its error in the
// page's banner until one succeeds (the page remembers that, so a poll that replaces it hides the banner too); show
// gets the error as well. Showing the page's content again (reset) ends the poll.
func pollPage[T any](d *desktopUI, fetch func(context.Context) (T, error), show func(T, error)) func() {
	if len(d.pages) == 0 {
		return func() {}
	}
	p := d.pages[len(d.pages)-1]
	poll := &pagePoll{}
	poll.run = func() {
		if poll.busy || d.closed || p.poll != poll {
			return
		}
		poll.busy = true
		ctx := d.ctx
		background(func() (T, error) { return fetch(ctx) }, func(value T, err error) {
			poll.busy = false
			if d.closed || p.poll != poll || len(d.pages) == 0 || d.pages[len(d.pages)-1] != p {
				return
			}
			if err != nil && ctx.Err() != nil {
				// Canceled by navigation that has come back here since (Back while it ran): fetch again at once
				if d.ctx.Err() == nil {
					poll.run()
				}
				return
			}
			if err != nil {
				p.pollFailed = true
				d.error(err)
			} else if p.pollFailed {
				p.pollFailed = false
				d.banner.SetRevealed(false)
			}
			show(value, err)
		})
	}
	p.poll = poll
	return poll.run
}

// pollVisible fetches again what the page in view shows, if it polls and is shown: not under a collapsed sidebar,
// and not while the window is minimized or otherwise out of sight
func (d *desktopUI) pollVisible() {
	if d.closed || d.saving || len(d.pages) == 0 || d.detail == nil || !d.detail.Mapped() {
		return
	}
	if d.w != nil && d.w.win != nil && d.w.win.IsSuspended() {
		return
	}
	if poll := d.pages[len(d.pages)-1].poll; poll != nil {
		poll.run()
	}
}

// unknownMethod reports whether a daemon older than the window doesn't have a method
func unknownMethod(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "unknown method ")
}

// liveTransfers is what the engine receives and sends now
type liveTransfers struct {
	desktopapi.Transfers
	// Whether the engine lists the files and measures speeds: an engine older than the window has no
	// engine.transfers, and its totals come from engine.stats
	listed bool
}

// fetchTransfers is what the engine receives and sends now, from engine.transfers, or from engine.stats for an
// engine that doesn't have it
func (d *desktopUI) fetchTransfers(ctx context.Context) (liveTransfers, error) {
	var t liveTransfers
	err := d.request(ctx, "engine.transfers", nil, &t.Transfers)
	if !unknownMethod(err) {
		t.listed = err == nil
		return t, err
	}
	var stats struct{ Download, Upload *statsProgress }
	if err := d.request(ctx, "engine.stats", nil, &stats); err != nil {
		return t, err
	}
	t.Download, t.Upload = stats.Download.totals(), stats.Upload.totals()
	return t, nil
}

// formatPercent is a fraction as a whole percentage, which reaches 100% only once it's done
func formatPercent(fraction float64) string {
	return fmt.Sprintf("%d%%", int(math.Floor(clampFraction(fraction)*100)))
}

func clampFraction(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return min(1, max(0, f))
}

// formatRemaining is a time to go as h:mm:ss
func formatRemaining(seconds int64) string {
	seconds = max(0, seconds)
	return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
}

func formatSpeed(bytesPerSecond int64) string { return humanBytes(bytesPerSecond) + "/s" }

// transferRate is how fast something is transferred and how long it has to go (when known), or that its speed
// isn't known yet
func transferRate(bytesPerSecond, secondsRemaining int64) string {
	if bytesPerSecond <= 0 {
		return "(Transfer speed unknown)"
	}
	if secondsRemaining < 0 {
		return formatSpeed(bytesPerSecond)
	}
	return formatSpeed(bytesPerSecond) + " · " + formatRemaining(secondsRemaining)
}

// secondsRemaining is how long the rest of a transfer takes at a speed (-1 when the speed isn't known)
func secondsRemaining(done, total, bytesPerSecond int64) int64 {
	if bytesPerSecond <= 0 {
		return -1
	}
	return (max(0, total-done) + bytesPerSecond - 1) / bytesPerSecond
}

// transferHeading is what a direction does now: "Receiving 3 files…", or "Not downloading" when nothing goes that way
func transferHeading(receiving bool, totals *desktopapi.TransferTotals) string {
	if totals == nil {
		if receiving {
			return "Not downloading"
		}
		return "Not uploading"
	}
	verb := "Sending"
	if receiving {
		verb = "Receiving"
	}
	return verb + " " + plural(int(totals.FilesTotal), "file", "files") + "…"
}

// transferSummary is how far a direction has come, how fast it goes and how long it has to go
func transferSummary(receiving bool, totals *desktopapi.TransferTotals) string {
	if totals == nil {
		if receiving {
			return "Currently no files are being downloaded from other devices."
		}
		return "Currently no files are being sent to other devices."
	}
	return humanBytes(totals.BytesDone) + " of " + humanBytes(totals.BytesTotal) + " · " +
		transferRate(totals.BytesPerSecond, totals.SecondsRemaining)
}

// fileTransferDetail is how far a file has come, and how fast it goes when that's known
func fileTransferDetail(f desktopapi.FileTransfer) string {
	parts := []string{humanBytes(f.BytesDone) + " of " + humanBytes(f.BytesTotal)}
	if f.BytesPerSecond > 0 {
		parts = append(parts, formatSpeed(f.BytesPerSecond))
	}
	return strings.Join(parts, " · ")
}

// progressItem is a row of a progress list
type progressItem struct {
	key, title, detail, problem string
	fraction                    float64
	// Whether the row has a progress bar (fraction is then how far it has come)
	bar bool
}

// progressSection is a titled part of a progress list
type progressSection struct {
	key, title string
	items      []progressItem
}

// transferSections are the files being received, by folder, or the files being sent, by device, in the engine's order
func transferSections(t desktopapi.Transfers, receiving bool) []progressSection {
	list := t.Downloads
	if !receiving {
		list = t.Uploads
	}
	var out []progressSection
	index := map[string]int{}
	for _, f := range list {
		key, title, name := f.Folder, f.FolderLabel, f.Path
		if !receiving {
			key, title, name = f.Device, f.DeviceName, f.FolderLabel+": "+f.Path
		}
		if title == "" {
			title = key
		}
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, progressSection{key: key, title: title})
		}
		out[i].items = append(out[i].items, progressItem{key: f.Folder + "\x00" + f.Path, title: name,
			detail: fileTransferDetail(f), fraction: f.Percentage, bar: true})
	}
	return out
}

// At most this many of Files mode's transfers are listed (it keeps up to 10,000)
const filesTransferLimit = 100

// filesTransferItems are Files mode's downloads, in the daemon's order (the order they started in, which stays)
func filesTransferItems(list []files.TransferProgress) []progressItem {
	var out []progressItem
	for i, t := range list {
		if i == filesTransferLimit {
			more := len(list) - filesTransferLimit
			out = append(out, progressItem{key: "\x00more", title: plural(more, "more transfer", "more transfers")})
			break
		}
		title := t.Path
		if title == "" {
			title = t.ID
		}
		detail := "Download · " + t.State
		item := progressItem{key: t.ID, title: title, problem: t.Error}
		if t.TotalBlocks > 0 {
			detail += " · " + humanBytes(t.VerifiedBytes) + " of " + humanBytes(t.TotalBytes)
			item.bar = true
			item.fraction = float64(t.VerifiedBlocks) / float64(t.TotalBlocks)
		}
		item.detail = detail
		out = append(out, item)
	}
	return out
}

// newProgressBar is a progress bar that shows its percentage
func newProgressBar(width int, label string) *gtk.ProgressBar {
	b := gtk.NewProgressBar()
	b.SetShowText(true)
	b.SetText("0%")
	b.SetSizeRequest(width, -1)
	b.SetVAlign(gtk.AlignCenter)
	setAccessibleLabel(b, label)
	return b
}

func setProgress(b *gtk.ProgressBar, fraction float64) {
	b.SetFraction(clampFraction(fraction))
	b.SetText(formatPercent(fraction))
}

func setAccessibleLabel(widget gtk.Widgetter, label string) {
	gtk.BaseWidget(widget).UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel},
		[]coreglib.Value{*coreglib.NewValue(label)})
}

// transferRow sums up what is received or sent, and opens the list of its files
type transferRow struct {
	row       *adw.ActionRow
	bar       *gtk.ProgressBar
	receiving bool
	// Downloads or Uploads
	title string
}

func newTransferRow(receiving bool, open func()) *transferRow {
	title := "Downloads"
	if !receiving {
		title = "Uploads"
	}
	t := &transferRow{row: row("", ""), bar: newProgressBar(160, title), receiving: receiving, title: title}
	t.row.AddSuffix(t.bar)
	button := iconButton("go-next-symbolic", "Open "+title, open)
	t.row.AddSuffix(button)
	t.row.SetActivatable(true)
	t.row.SetActivatableWidget(button)
	t.set(nil)
	return t
}

// unknown shows the row without what goes that way, which isn't known now
func (t *transferRow) unknown() {
	t.row.SetTitle(t.title)
	t.row.SetSubtitle("")
	t.bar.SetVisible(false)
}

func (t *transferRow) set(totals *desktopapi.TransferTotals) {
	t.row.SetTitle(transferHeading(t.receiving, totals))
	t.row.SetSubtitle(transferSummary(t.receiving, totals))
	t.bar.SetVisible(totals != nil)
	if totals != nil {
		setProgress(t.bar, totals.Percentage)
	}
}

// progressRow is a file's row in a progress list: its name, how far it has come, and a problem in red
type progressRow struct {
	row                    *gtk.ListBoxRow
	title, detail, problem *gtk.Label
	bar                    *gtk.ProgressBar
	// The row's accessible description: its detail and problem
	description string
}

func newProgressRow() *progressRow {
	r := &progressRow{row: gtk.NewListBoxRow(), title: gtk.NewLabel(""), detail: gtk.NewLabel(""),
		problem: gtk.NewLabel(""), bar: gtk.NewProgressBar()}
	r.row.SetActivatable(false)
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	box.SetMarginTop(10)
	box.SetMarginBottom(10)
	box.SetMarginStart(12)
	box.SetMarginEnd(12)
	// Names come from other devices and gateways: labels never take them as markup
	for _, l := range []*gtk.Label{r.title, r.detail, r.problem} {
		l.SetXAlign(0)
		l.SetWrap(true)
		l.SetWrapMode(pango.WrapWordChar)
		box.Append(l)
	}
	r.detail.AddCSSClass("dim-label")
	r.detail.AddCSSClass("caption")
	r.problem.AddCSSClass("error")
	r.problem.AddCSSClass("caption")
	box.InsertChildAfter(r.bar, r.detail)
	r.row.SetChild(box)
	return r
}

func (r *progressRow) set(item progressItem) {
	if r.title.Text() != item.title {
		r.title.SetText(item.title)
		setAccessibleLabel(r.row, item.title)
		setAccessibleLabel(r.bar, item.title)
	}
	if description := strings.Join(nonEmpty(item.detail, item.problem), ". "); description != r.description {
		r.description = description
		gtk.BaseWidget(r.row).UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyDescription},
			[]coreglib.Value{*coreglib.NewValue(description)})
	}
	r.detail.SetText(item.detail)
	r.detail.SetVisible(item.detail != "")
	r.bar.SetVisible(item.bar)
	if item.bar {
		r.bar.SetFraction(clampFraction(item.fraction))
	}
	r.problem.SetText(item.problem)
	r.problem.SetVisible(item.problem != "")
}

// progressRows is a boxed list of progress rows. Rows of items that stay are updated in place, keeping their widgets
// (and the focus), and only rows of new items are made. Only rows out of place move.
type progressRows struct {
	list *gtk.ListBox
	rows map[string]*progressRow
}

func newProgressRows(placeholder string) *progressRows {
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	list.SetSelectionMode(gtk.SelectionNone)
	if placeholder != "" {
		label := gtk.NewLabel(placeholder)
		label.SetWrap(true)
		label.SetMarginTop(12)
		label.SetMarginBottom(12)
		label.SetMarginStart(12)
		label.SetMarginEnd(12)
		label.AddCSSClass("dim-label")
		list.SetPlaceholder(label)
	}
	return &progressRows{list: list, rows: map[string]*progressRow{}}
}

// set shows items, in their order
func (p *progressRows) set(items []progressItem) {
	wanted := make(map[string]bool, len(items))
	unique := items[:0:0]
	for _, item := range items {
		if !wanted[item.key] {
			wanted[item.key] = true
			unique = append(unique, item)
		}
	}
	for key, r := range p.rows {
		if !wanted[key] {
			p.list.Remove(r.row)
			delete(p.rows, key)
		}
	}
	// Before item i, the list has the rows of the items before it in order: item i's row goes to i unless it's there
	for i, item := range unique {
		r, ok := p.rows[item.key]
		if !ok {
			r = newProgressRow()
			p.rows[item.key] = r
		}
		if at := r.row.Index(); at != i {
			if at >= 0 {
				p.list.Remove(r.row)
			}
			p.list.Insert(r.row, i)
		}
		r.set(item)
	}
}

// progressSections is a list of progress rows in titled sections, with a status page of its own when it's empty.
// Like the rows, sections that stay keep their widgets.
type progressSections struct {
	box      *gtk.Box
	empty    *adw.StatusPage
	sections map[string]*sectionWidgets
}

type sectionWidgets struct {
	box     *gtk.Box
	heading *gtk.Label
	rows    *progressRows
}

func newProgressSections(emptyTitle, emptyDescription string) *progressSections {
	s := &progressSections{box: gtk.NewBox(gtk.OrientationVertical, 18), empty: adw.NewStatusPage(),
		sections: map[string]*sectionWidgets{}}
	s.empty.SetIconName("media-playback-pause-symbolic")
	s.empty.SetTitle(emptyTitle)
	s.empty.SetDescription(emptyDescription)
	s.empty.AddCSSClass("compact")
	// Nothing is known before the first answer
	s.empty.SetVisible(false)
	return s
}

func (s *progressSections) set(sections []progressSection) {
	want := make(map[string]bool, len(sections))
	for _, section := range sections {
		want[section.key] = true
	}
	for key, w := range s.sections {
		if !want[key] {
			s.box.Remove(w.box)
			delete(s.sections, key)
		}
	}
	var previous gtk.Widgetter
	for _, section := range sections {
		w, ok := s.sections[section.key]
		if !ok {
			w = &sectionWidgets{box: gtk.NewBox(gtk.OrientationVertical, 6), heading: gtk.NewLabel(""),
				rows: newProgressRows("")}
			w.heading.SetXAlign(0)
			w.heading.SetWrap(true)
			w.heading.SetWrapMode(pango.WrapWordChar)
			w.heading.AddCSSClass("heading")
			w.box.Append(w.heading)
			w.box.Append(w.rows.list)
			s.sections[section.key] = w
			if previous == nil {
				s.box.InsertChildAfter(w.box, nil)
			} else {
				s.box.InsertChildAfter(w.box, previous)
			}
		} else if previous == nil {
			s.box.ReorderChildAfter(w.box, nil)
		} else {
			s.box.ReorderChildAfter(w.box, previous)
		}
		if w.heading.Text() != section.title {
			w.heading.SetText(section.title)
		}
		w.rows.set(section.items)
		previous = w.box
	}
	s.empty.SetVisible(len(sections) == 0)
}

// openTransfers opens the list of the files being received or sent
func (d *desktopUI) openTransfers(receiving bool) {
	d.subpage(func() { d.transfersPage(receiving) })
}

// transfersPage lists the files being received, by folder, or being sent, by device, as they progress
func (d *desktopUI) transfersPage(receiving bool) {
	title, emptyTitle, emptyDescription := "Downloads", "Not downloading",
		"Currently no files are being downloaded from other devices."
	if !receiving {
		title, emptyTitle, emptyDescription = "Uploads", "Not uploading",
			"Currently no files are being sent to other devices."
	}
	d.reset(title)
	list := newProgressSections(emptyTitle, emptyDescription)
	d.body.Append(list.box)
	d.body.Append(list.empty)
	pollPage(d, d.fetchTransfers, func(t liveTransfers, err error) {
		switch {
		case err != nil:
		case !t.listed:
			// An engine older than the window
			list.set(nil)
			list.empty.SetTitle("Files Not Listed")
			list.empty.SetDescription("Restart the background service to list the files it transfers.")
		default:
			list.empty.SetTitle(emptyTitle)
			list.empty.SetDescription(emptyDescription)
			list.set(transferSections(t.Transfers, receiving))
		}
	})()
}

// transferStatus shows what the engine receives and sends, live: in Activity, and in each list
func (d *desktopUI) transferStatus(g *adw.PreferencesGroup) {
	download := newTransferRow(true, func() { d.openTransfers(true) })
	upload := newTransferRow(false, func() { d.openTransfers(false) })
	g.Add(download.row)
	g.Add(upload.row)
	pollPage(d, d.fetchTransfers, func(t liveTransfers, err error) {
		if err != nil {
			// The banner says why: nothing here may look current
			download.unknown()
			upload.unknown()
			return
		}
		download.set(t.Download)
		upload.set(t.Upload)
	})()
}

// fileDownload is how a file of a synchronized folder comes here
type fileDownload struct {
	// Being received (nil when it isn't)
	file *desktopapi.FileTransfer
	// What engine.inspect says about one that isn't being received and wasn't here (inspected is false when it
	// couldn't be asked)
	inspected, local, ignored bool
}

// fileProgress shows, in a file's information, how it is received, or that it waits to be (nothing when it's here,
// or when the folder's ignore patterns leave it out)
func (d *desktopUI) fileProgress(r browserRow, folder string) {
	g := d.group("")
	status := row("", "")
	bar := newProgressBar(160, "Download progress")
	status.AddSuffix(bar)
	g.Add(status)
	g.SetVisible(false)
	var local atomic.Bool
	// Browsing tells whether a file is here, and a search gives the local path only of one that is
	local.Store(r.writable || r.native != "")
	ignored := r.ignored
	pollPage(d, func(ctx context.Context) (fileDownload, error) {
		t, err := d.fetchTransfers(ctx)
		if err != nil {
			return fileDownload{}, err
		}
		for i := range t.Downloads {
			if t.Downloads[i].Folder == folder && t.Downloads[i].Path == r.path {
				return fileDownload{file: &t.Downloads[i]}, nil
			}
		}
		if local.Load() {
			return fileDownload{}, nil
		}
		// Not being received and not here before: has it arrived since, or is it left out?
		var info struct{ Entry desktopapi.Entry }
		if d.request(ctx, "engine.inspect", desktopapi.Request{Folder: folder, Path: r.path}, &info) != nil {
			return fileDownload{}, nil
		}
		return fileDownload{inspected: true, local: info.Entry.Local, ignored: info.Entry.Ignored}, nil
	}, func(p fileDownload, err error) {
		if err != nil {
			g.SetVisible(false)
			return
		}
		if p.inspected {
			local.Store(p.local)
			ignored = p.ignored
		}
		switch {
		case p.file != nil:
			f := *p.file
			status.SetTitle("Downloading file…")
			status.SetSubtitle(humanBytes(f.BytesDone) + " of " + humanBytes(f.BytesTotal) + " · " +
				transferRate(f.BytesPerSecond, secondsRemaining(f.BytesDone, f.BytesTotal, f.BytesPerSecond)))
			setProgress(bar, f.Percentage)
			bar.SetVisible(true)
			g.SetVisible(true)
		case !local.Load() && !ignored:
			status.SetTitle("Waiting to synchronize…")
			status.SetSubtitle("")
			bar.SetVisible(false)
			g.SetVisible(true)
		default:
			g.SetVisible(false)
		}
	})()
}
