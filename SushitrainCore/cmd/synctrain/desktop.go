// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
)

type desktopUI struct {
	resolveOnce                            sync.Once
	resolveErr                             error
	deviceID                               string
	browserSearch                          bool
	overviewRefresh                        func()
	browserRows                            []browserRow
	browserNext                            int
	browserSelected                        string
	parent                                 int64
	preferences                            Preferences
	w                                      *mainWindow
	root                                   *adw.NavigationSplitView
	navigation                             *adw.NavigationView
	pages                                  []*navigationPage
	detail                                 *adw.ToolbarView
	body                                   *gtk.Box
	commands                               *gtk.FlowBox
	banner                                 *adw.Banner
	sidebar                                *gtk.ListBox
	route, mode, socket, folder, directory string
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	generation                             uint64
	dirty, saving, closed                  bool
	editor                                 string
	stopPage                               func()
	timer                                  coreglib.SourceHandle
}

var desktopRoutes = []string{"Overview", "Folders", "Devices", "Files", "Remote Files", "History Sources", "Pending Changes", "Settings"}

func newDesktopUI(w *mainWindow) *desktopUI {
	d := &desktopUI{w: w, mode: readMode(), route: "Overview", preferences: readPreferences()}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.root = adw.NewNavigationSplitView()
	breakpoint := adw.NewBreakpoint(adw.NewBreakpointConditionLength(adw.BreakpointConditionMaxWidth, 700, adw.LengthUnitSp))
	breakpoint.AddSetter(d.root, "collapsed", true)
	w.win.AddBreakpoint(breakpoint)
	d.sidebar = gtk.NewListBox()
	d.sidebar.AddCSSClass("navigation-sidebar")
	d.sidebar.SetSelectionMode(gtk.SelectionSingle)
	pages := desktopRoutes
	for _, name := range pages {
		r := gtk.NewListBoxRow()
		l := gtk.NewLabel(name)
		l.SetXAlign(0)
		l.SetMarginTop(12)
		l.SetMarginBottom(12)
		l.SetMarginStart(12)
		r.SetChild(l)
		d.sidebar.Append(r)
	}
	side := adw.NewToolbarView()
	h := adw.NewHeaderBar()
	h.SetTitleWidget(gtk.NewLabel("Synctrain"))
	side.AddTopBar(h)
	side.SetContent(d.sidebar)
	d.root.SetSidebar(adw.NewNavigationPage(side, "Synctrain"))
	d.navigation = adw.NewNavigationView()
	d.navigation.ConnectPopped(func(*adw.NavigationPage) { d.pagePopped() })
	d.root.SetContent(adw.NewNavigationPage(d.navigation, "Synctrain"))
	d.sidebar.ConnectRowActivated(func(r *gtk.ListBoxRow) {
		if r != nil {
			d.root.SetShowContent(true)
		}
	})
	d.sidebar.ConnectRowSelected(func(r *gtk.ListBoxRow) {
		if r == nil {
			return
		}
		next := pages[r.Index()]
		if next == d.route {
			return
		}
		for i, name := range pages {
			if name == d.route {
				d.sidebar.SelectRow(d.sidebar.RowAtIndex(i))
				break
			}
		}
		d.guard(func() { d.navigate(next) })
	})
	d.sidebar.SelectRow(d.sidebar.RowAtIndex(0))
	d.showRoute()
	d.timer = coreglib.TimeoutSecondsAdd(2, func() bool {
		if d.closed {
			return false
		}
		next := readPreferences()
		if next.Preferences != d.preferences.Preferences && !d.saving && !d.dirty {
			d.preferences = next
			_ = configureTray(next.Tray)
			d.refreshView()
		}
		if d.route == "Overview" && !d.saving {
			if d.overviewRefresh != nil {
				d.overviewRefresh()
			}
		}
		return true
	})
	return d
}
func (d *desktopUI) close() {
	d.closeEditor()
	if d.closed {
		return
	}
	d.closed = true
	d.cancel()
	if d.stopPage != nil {
		d.stopPage()
	}
	if d.timer != 0 {
		coreglib.SourceRemove(d.timer)
	}
}
func (d *desktopUI) guard(next func()) {
	if d.saving {
		d.w.toast("Wait for the current operation to finish.")
		return
	}
	if !d.dirty {
		next()
		return
	}
	if d.w.win.VisibleDialog() != nil {
		return
	}
	d.w.confirm("Discard Changes?", "Your edits have not been saved.", "Discard Changes", true, func() { d.setDirty(false); d.closeEditor(); next() })
}
func (d *desktopUI) closeEditor() {
	token := d.editor
	d.editor = ""
	if token != "" {
		go d.request(context.Background(), "settings.close", desktopapi.Request{Token: token}, nil)
	}
}
func (d *desktopUI) navigate(route string) {
	d.closeEditor()
	d.cancel()
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.generation++
	if d.stopPage != nil {
		d.stopPage()
		d.stopPage = nil
	}
	d.route = route
	for i, name := range desktopRoutes {
		if name == route {
			d.sidebar.SelectRow(d.sidebar.RowAtIndex(i))
			break
		}
	}
	d.root.SetShowContent(true)
	d.dirty = false
	d.showRoute()
}
func (d *desktopUI) request(ctx context.Context, method string, p, result any) error {
	if d.mode == modeSynchronize {
		d.resolveOnce.Do(func() { home, err := describeHome(); d.socket = home.Socket; d.resolveErr = err })
		if d.resolveErr != nil {
			return d.resolveErr
		}
	}
	socket := d.socket
	if d.mode == modeFiles {
		socket = filesSocket()
	}

	limit := 2 * time.Minute
	if method == "history.pair" || method == "pair" {
		limit = 11 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return adminsock.CallContext(ctx, socket, method, p, result)
}
func (d *desktopUI) load(method string, p any, done func(json.RawMessage)) {
	loaded := d.trackPageLoad()
	generation := d.generation
	ctx := d.ctx
	background(func() (json.RawMessage, error) {
		var out json.RawMessage
		err := d.request(ctx, method, p, &out)
		return out, err
	}, func(out json.RawMessage, err error) {
		defer loaded()
		if d.closed || generation != d.generation {
			if method == "settings.get" && err == nil {
				var stale desktopapi.Editor
				if json.Unmarshal(out, &stale) == nil && stale.Token != "" {
					go d.request(context.Background(), "settings.close", desktopapi.Request{Token: stale.Token}, nil)
				}
			}
			return
		}
		if err != nil {
			d.error(err)
			return
		}
		d.banner.SetRevealed(false)
		done(out)
		d.focusPage()
	})
}
func (d *desktopUI) run(method string, p any, done func(json.RawMessage)) {
	if d.saving {
		return
	}
	d.saving = true
	d.updateNavigation()
	d.detail.SetSensitive(false)
	background(func() (json.RawMessage, error) {
		var out json.RawMessage
		err := d.request(context.Background(), method, p, &out)
		return out, err
	}, func(out json.RawMessage, err error) {
		d.saving = false
		d.updateNavigation()
		d.detail.SetSensitive(true)
		if d.closed {
			return
		}
		if err != nil {
			d.error(err)
			return
		}

		if done != nil {
			done(out)
		} else {
			d.invalidateParents()
			d.refreshView()
		}
	})
}
func (d *desktopUI) error(err error) { d.banner.SetTitle(sentence(err)); d.banner.SetRevealed(true) }
func (d *desktopUI) reset(title string) {
	if len(d.pages) > 0 {
		d.pages[len(d.pages)-1].page.SetTitle(title)
	}
	d.body = gtk.NewBox(gtk.OrientationVertical, 12)
	d.detail.SetSensitive(!d.saving)
	d.body.SetMarginStart(24)
	d.body.SetMarginEnd(24)
	d.body.SetMarginTop(18)
	d.body.SetMarginBottom(24)
	d.commands = gtk.NewFlowBox()
	d.commands.SetSelectionMode(gtk.SelectionNone)
	d.commands.SetColumnSpacing(8)
	d.commands.SetRowSpacing(8)
	d.commands.SetMinChildrenPerLine(1)
	d.commands.SetMaxChildrenPerLine(5)
	d.body.Append(d.commands)
	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(d.body)
	d.detail.SetContent(scroll)
}
func (d *desktopUI) text(s string) {
	l := gtk.NewLabel(s)
	l.SetXAlign(0)
	l.SetWrap(true)
	l.SetSelectable(true)
	d.body.Append(l)
}
func (d *desktopUI) button(label string, fn func()) { d.commands.Insert(textButton(label, fn), -1) }
func (d *desktopUI) group(title string) *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	d.body.Append(g)
	return g
}
func (d *desktopUI) showRoute() {
	d.startView()
	if d.closed {
		return
	}
	if d.stopPage != nil {
		d.stopPage()
		d.stopPage = nil
	}
	d.pages = nil
	p := d.newNavigationPage(d.route, d.renderRoute)
	d.pages = []*navigationPage{p}
	d.navigation.Replace([]*adw.NavigationPage{p.page})
	d.renderRoute()
}

func (d *desktopUI) renderRoute() {
	d.reset(d.route)
	switch d.route {
	case "Overview":
		d.overview()
	case "Folders":
		if d.mode == modeFiles {
			d.legacyFiles()
		} else {
			d.folders()
		}
	case "Devices":
		if d.mode == modeSynchronize {
			d.devices()
		} else {
			d.text("Devices are managed by each gateway.")
		}
	case "Files":
		d.browse()
	case "Remote Files":
		if d.mode == modeFiles {
			d.legacyFiles()
		} else {
			widget, stop := newSyncView(d.w)
			d.detail.SetContent(widget)
			d.stopPage = stop
		}
	case "History Sources":
		d.historySources()
	case "Pending Changes":
		if d.mode == modeFiles {
			d.legacyFiles()
		} else {
			d.text("Pending changes are available in Files mode.")
		}
	case "Settings":
		d.settings()
	}
}
func (d *desktopUI) legacyFiles() {
	widget, stop := newFilesView(d.w)
	d.detail.SetContent(widget)
	d.stopPage = stop
}
func (d *desktopUI) action(action, folder, device string) {
	d.run("engine.action", desktopapi.Request{Action: action, Folder: folder, Device: device}, nil)
}
func (d *desktopUI) overview() {
	d.reset("Overview")
	d.serviceButtons()
	heading := gtk.NewLabel("Connecting to the background service…")
	heading.SetXAlign(0)
	heading.SetWrap(true)
	d.body.Append(heading)
	identity := gtk.NewLabel("")
	identity.SetSelectable(true)
	identity.SetWrap(true)
	identity.SetXAlign(0)
	d.body.Append(identity)
	summary := gtk.NewLabel("")
	summary.SetXAlign(0)
	d.body.Append(summary)
	download := gtk.NewProgressBar()
	download.SetShowText(true)
	download.SetVisible(d.mode == modeSynchronize)
	d.body.Append(download)
	upload := gtk.NewProgressBar()
	upload.SetShowText(true)
	upload.SetVisible(d.mode == modeSynchronize)
	d.body.Append(upload)
	reason := gtk.NewLabel("")
	reason.SetXAlign(0)
	d.body.Append(reason)
	if d.mode == modeSynchronize {
		d.button("Pause All", func() { d.action("pauseAll", "", "") })
		d.button("Resume All", func() { d.action("resumeAll", "", "") })
		d.button("Rescan All", func() { d.action("rescanAll", "", "") })
	}
	type snapshot struct {
		Status struct {
			DeviceID, Name, State, Mount string
			RestartRequired              bool
		}
		Stats struct {
			Download, Upload *struct {
				BytesTotal, BytesDone int64
				Percentage            float64
			}
			Totals struct {
				Local, LocalNeed *struct {
					Files int
					Bytes int64
				}
			}
		}
		Policy struct{ Reason string }
	}
	updating := false
	generation := d.generation
	ctx := d.ctx
	d.overviewRefresh = func() {
		if updating {
			return
		}
		updating = true
		background(func() (snapshot, error) {
			var state snapshot
			method := "engine.status"
			if d.mode == modeFiles {
				method = "status"
			}
			if err := d.request(ctx, method, nil, &state.Status); err != nil {
				return state, err
			}
			if d.mode == modeSynchronize {
				if err := d.request(ctx, "engine.stats", nil, &state.Stats); err != nil {
					return state, err
				}
				_ = d.request(ctx, "policy.get", nil, &state.Policy)
			}
			return state, nil
		}, func(s snapshot, err error) {
			updating = false
			if d.closed || generation != d.generation {
				return
			}
			if err != nil {
				heading.SetText("The background service is stopped or unavailable.")
				d.error(err)
				return
			}
			d.banner.SetRevealed(false)
			if d.mode == modeFiles {
				heading.SetText("Files mode — folders are fetched as you open them")
				identity.SetText(s.Status.Mount)
				return
			}
			d.deviceID = s.Status.DeviceID
			heading.SetText(s.Status.Name + " · " + s.Status.State)
			identity.SetText("Device ID: " + s.Status.DeviceID)
			local, needed := s.Stats.Totals.Local, s.Stats.Totals.LocalNeed
			if local != nil {
				pending := 0
				if needed != nil {
					pending = needed.Files
				}
				summary.SetText(fmt.Sprintf("%d local files · %s · %d files remaining", local.Files, humanBytes(local.Bytes), pending))
			}
			for _, progress := range []struct {
				bar   *gtk.ProgressBar
				name  string
				value *struct {
					BytesTotal, BytesDone int64
					Percentage            float64
				}
			}{{download, "Downloading", s.Stats.Download}, {upload, "Uploading", s.Stats.Upload}} {
				if progress.value == nil || progress.value.BytesTotal == 0 {
					progress.bar.SetFraction(0)
					progress.bar.SetText(progress.name + ": idle")
				} else {
					progress.bar.SetFraction(max(0, min(1, progress.value.Percentage)))
					progress.bar.SetText(fmt.Sprintf("%s: %s of %s", progress.name, humanBytes(progress.value.BytesDone), humanBytes(progress.value.BytesTotal)))
				}
			}
			message := s.Policy.Reason
			if s.Status.RestartRequired {
				message += " Saved settings require an engine restart."
			}
			reason.SetText(strings.TrimSpace(message))
		})
	}
	d.overviewRefresh()
}

func (d *desktopUI) serviceButtons() {
	unit := "synctrain-gateway.service"
	if d.mode == modeFiles {
		unit = "synctrain-files.service"
	}
	g := d.group("Background Service")
	r := row("Runs independently of this window", "Closing the window keeps the service running.")
	r.AddSuffix(textButton("Restart", func() { d.serviceCommand("restart", unit) }))
	r.AddSuffix(textButton("Stop", func() {
		d.w.confirm("Stop Synctrain?", "Files that have not reached their gateway stay saved here.", "Stop", false, func() { d.serviceCommand("stop", unit) })
	}))
	r.AddSuffix(textButton("Start", func() { d.serviceCommand("start", unit) }))
	g.Add(r)
}
func (d *desktopUI) serviceCommand(command, unit string) {
	if d.saving {
		return
	}
	d.saving = true
	d.detail.SetSensitive(false)
	background(func() (struct{}, error) { return struct{}{}, systemctl(command, unit) }, func(_ struct{}, err error) {
		d.saving = false
		d.detail.SetSensitive(true)
		if d.closed {
			return
		}
		if err != nil {
			d.error(err)
		} else {
			d.w.show()
		}
	})
}
func pretty(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
func (d *desktopUI) input(title, caption, initial string, done func(string)) {
	dialog := adw.NewAlertDialog(title, caption)
	entry := gtk.NewEntry()
	entry.SetText(initial)
	entry.SetActivatesDefault(true)
	dialog.SetExtraChild(entry)
	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("save", "Continue")
	dialog.SetResponseAppearance("save", adw.ResponseSuggested)
	dialog.SetDefaultResponse("save")
	dialog.SetCloseResponse("cancel")
	dialog.ConnectResponse(func(response string) {
		if response == "save" {
			done(entry.Text())
		}
	})
	dialog.Present(d.w.win)
}

func (d *desktopUI) startView() {
	d.overviewRefresh = nil
	d.closeEditor()
	d.cancel()
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.generation++
	d.dirty = false
	if d.stopPage != nil {
		d.stopPage()
		d.stopPage = nil
	}
}
func (d *desktopUI) subpage(show func()) {
	d.guard(func() { d.pushPage(show) })
}
func (d *desktopUI) passwordInput(title, caption string, done func(string)) {
	dialog := adw.NewAlertDialog(title, caption)
	entry := adw.NewPasswordEntryRow()
	entry.SetTitle("Encryption password")
	dialog.SetExtraChild(entry)
	dialog.AddResponse("cancel", "Cancel")
	dialog.AddResponse("save", "Continue")
	dialog.SetDefaultResponse("save")
	dialog.SetCloseResponse("cancel")
	dialog.ConnectResponse(func(response string) {
		if response == "save" {
			done(entry.Text())
		}
	})
	dialog.Present(d.w.win)
}

func (d *desktopUI) fetch(method string, p any, done func(json.RawMessage, error)) {
	loaded := d.trackPageLoad()
	generation := d.generation
	ctx := d.ctx
	background(func() (json.RawMessage, error) {
		var result json.RawMessage
		err := d.request(ctx, method, p, &result)
		return result, err
	}, func(b json.RawMessage, err error) {
		defer loaded()
		if d.closed || generation != d.generation {
			return
		}
		done(b, err)
	})
}
