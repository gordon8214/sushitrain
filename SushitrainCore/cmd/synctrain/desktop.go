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
	"t-shaped.nl/sushitrain/v2/desktopui"
)

type desktopUI struct {
	settingsSearchBar                                          *gtk.SearchBar
	settingsSearchButton                                       *gtk.ToggleButton
	settingsSearchEntry                                        *gtk.SearchEntry
	settingsSearchStack                                        *gtk.Stack
	settingsSearchResults                                      *gtk.ListBox
	settingsSearchIndex                                        []desktopui.SearchEntry
	settingsSearchError, searchTargetAnchor, searchTargetField string
	settingsSearchLoading                                      bool
	settingsSearchGeneration                                   uint64

	resolveOnce                            sync.Once
	resolveErr                             error
	deviceID                               string
	browserSearch                          bool
	browserRows                            []browserRow
	browserNext                            int
	browserSelected                        string
	browserFilter, browserScope            string
	browserLoadTarget                      int
	filesCacheInvalid                      bool
	parent                                 int64
	preferences                            Preferences
	w                                      *mainWindow
	root                                   *adw.NavigationSplitView
	navigation                             *adw.NavigationView
	pages                                  []*navigationPage
	retainedFiles                          []*navigationPage
	resumeFiles                            func()
	detail                                 *adw.ToolbarView
	body                                   *gtk.Box
	commands                               *gtk.FlowBox
	banner                                 *adw.Banner
	sidebar                                *gtk.ListBox
	sidebarRoutes                          []string
	updatingSidebar                        bool
	sidebarBack, sidebarSettings           *gtk.Button
	folderShortcuts                        *adw.ExpanderRow
	shortcutRows                           []gtk.Widgetter
	shortcutsExpanded                      bool
	shortcutGeneration                     uint64
	shortcutContext                        context.Context
	shortcutCancel                         context.CancelFunc
	route, mode, socket, folder, directory string
	appRoute                               string
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	generation                             uint64
	dirty, saving, closed                  bool
	editor                                 string
	stopPage                               func()
	timer                                  coreglib.SourceHandle
}

var desktopRoutes = desktopui.MainTitles()

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
	side := adw.NewToolbarView()
	h := adw.NewHeaderBar()
	h.SetTitleWidget(gtk.NewLabel(""))
	side.AddTopBar(h)
	column := gtk.NewBox(gtk.OrientationVertical, 0)
	d.sidebarBack = textButton("", func() { d.guard(func() { d.navigate(d.appDestination()) }) })
	column.Append(d.sidebarBack)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(d.sidebar)
	d.installSettingsSearch(h, column, scroll)
	d.sidebarSettings = textButton("Settings", func() { d.guard(func() { d.navigate("Settings") }) })
	column.Append(d.sidebarSettings)
	side.SetContent(column)
	d.root.SetSidebar(adw.NewNavigationPage(side, "Navigation"))
	d.navigation = adw.NewNavigationView()
	d.navigation.ConnectPopped(func(*adw.NavigationPage) { d.pagePopped() })
	d.root.SetContent(adw.NewNavigationPage(d.navigation, "Synctrain"))
	d.sidebar.ConnectRowActivated(func(r *gtk.ListBoxRow) {
		if r != nil {
			d.root.SetShowContent(true)
		}
	})
	d.sidebar.ConnectRowSelected(func(r *gtk.ListBoxRow) {
		if r == nil || d.updatingSidebar || r.Index() >= len(d.sidebarRoutes) || d.sidebarRoutes[r.Index()] == "" {
			return
		}
		next := d.sidebarRoutes[r.Index()]
		if next == d.route {
			return
		}
		for i, name := range d.sidebarRoutes {
			if name == d.route {
				d.sidebar.SelectRow(d.sidebar.RowAtIndex(i))
				break
			}
		}
		d.guard(func() { d.navigate(next) })
	})
	d.rebuildSidebar()
	d.sidebar.SelectRow(d.sidebar.RowAtIndex(1))
	d.showRoute()
	d.timer = coreglib.TimeoutSecondsAdd(2, func() bool {
		if d.closed {
			return false
		}
		next := readPreferences()
		if d.settingsSearchBar.SearchMode() && !d.saving {
			d.loadSettingsSearch()
		}
		if next.Preferences != d.preferences.Preferences && !d.saving && !d.dirty {
			d.preferences = next
			_ = configureTray(next.Tray)
			d.refreshView()
		}
		// Live progress, on the page in view
		d.pollVisible()
		return true
	})
	return d
}
func (d *desktopUI) close() {
	if d.shortcutCancel != nil {
		d.shortcutCancel()
	}
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
func (d *desktopUI) appDestination() string {
	if d.appRoute != "" {
		return d.appRoute
	}
	return "Overview"
}

func (d *desktopUI) navigate(route string) {
	route = canonicalRoute(route)
	available := false
	for _, group := range desktopui.Read().Navigation("gnome", d.mode) {
		for _, item := range group.Items {
			if item.NativeRoute() == route {
				available = true
			}
		}
	}
	unavailable := !available && d.mode != ""
	if unavailable {
		route = "Overview"
	}
	if route == "Files" && d.route == route && len(d.pages) > 0 {
		d.root.SetShowContent(true)
		return
	}
	if d.route == "Files" && route != "Files" && len(d.pages) > 0 {
		complete := d.editor == "" && !d.dirty && !d.saving
		for _, page := range d.pages {
			complete = complete && page.pending == 0 && !page.editor
		}
		if complete {
			current := d.pages[len(d.pages)-1]
			current.body, current.commands, current.focus = d.body, d.commands, d.w.win.Focus()
			d.retainedFiles, d.resumeFiles = d.pages, d.browserState()
		}
	}
	d.closeEditor()
	d.cancel()
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.generation++
	if d.stopPage != nil {
		d.stopPage()
		d.stopPage = nil
	}
	if strings.HasPrefix(route, "settings:") && !strings.HasPrefix(d.route, "settings:") {
		d.appRoute = d.route
	}
	d.route = route
	if unavailable {
		d.searchTargetAnchor = ""
		d.searchTargetField = ""
	}
	d.rebuildSidebar()
	d.root.SetShowContent(true)
	d.dirty = false
	if route == "Files" && len(d.retainedFiles) > 0 {
		d.pages = append([]*navigationPage(nil), d.retainedFiles...)
		d.resumeFiles()
		d.retainedFiles, d.resumeFiles = nil, nil
		current := d.pages[len(d.pages)-1]
		if d.filesCacheInvalid {
			for len(d.pages) > 1 && !d.pages[len(d.pages)-1].browser {
				d.pages = d.pages[:len(d.pages)-1]
			}
			ancestor := d.pages[len(d.pages)-1]
			if ancestor != current && ancestor.resume != nil {
				ancestor.resume()
			}
			current = ancestor
		}
		d.restoreNavigationPage(current)
		pages := make([]*adw.NavigationPage, len(d.pages))
		for i, page := range d.pages {
			pages[i] = page.page
		}
		d.navigation.Replace(pages)
		if d.filesCacheInvalid {
			d.filesCacheInvalid = false
			d.detail.SetSensitive(false)
			current.show()
		}
		d.updateNavigation()
		d.focusPage()
		d.populateFolderShortcuts()
		return
	}
	d.showRoute()
	if unavailable {
		d.text("The requested page is unavailable in this mode. Overview is shown instead.")
	}
}

// Explicit folder targets supersede a retained browser location.
func (d *desktopUI) openFilesAt(folder, directory string, parent int64) {
	d.retainedFiles, d.resumeFiles = nil, nil
	d.folder, d.directory, d.parent = folder, directory, parent
	if d.route == "Files" {
		d.showRoute()
	} else {
		d.navigate("Files")
	}
}

func (d *desktopUI) requestEndpoint(mode string) (string, error) {
	if mode == modeSynchronize {
		d.resolveOnce.Do(func() { home, err := describeHome(); d.socket = home.Socket; d.resolveErr = err })
		if d.resolveErr != nil {
			return "", d.resolveErr
		}
	}
	if mode == modeFiles {
		return filesSocket(), nil
	}
	return d.socket, nil
}

func (d *desktopUI) request(ctx context.Context, method string, p, result any) error {
	socket, err := d.requestEndpoint(d.mode)
	if err != nil {
		return err
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
	d.runWithFailure(method, p, done, nil)
}
func (d *desktopUI) runWithFailure(method string, p any, done func(json.RawMessage), failed func()) {
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
			if failed != nil {
				failed()
			}
			d.error(err)
			return
		}

		d.filesCacheInvalid = true
		d.invalidateSettingsSearch()
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
		p := d.pages[len(d.pages)-1]
		p.page.SetTitle(title)
		// What the page polled for is replaced, and so is the error of a poll that failed
		p.poll = nil
		if p.pollFailed {
			p.pollFailed = false
			p.banner.SetRevealed(false)
		}
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
	d.populateFolderShortcuts()
}

func (d *desktopUI) renderRoute() {
	d.reset(d.route)
	if strings.HasPrefix(d.route, "settings:") {
		d.settingsCategory(strings.TrimPrefix(d.route, "settings:"))
		return
	}
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
	case "Remote Access":
		if d.mode == modeFiles {
			d.legacyFiles()
		} else {
			widget, stop := newSyncView(d.w)
			d.detail.SetContent(widget)
			d.stopPage = stop
		}
	case "History":
		d.historyPage()
	case "Activity":
		d.activityPage()
	case "Settings":
		d.settings()
	}
}
func (d *desktopUI) legacyFiles() {
	selected := "folders"
	switch d.route {
	case "Remote Access":
		selected = "gateways"
	case "Activity":
		selected = "changes"
	default:
		if strings.HasPrefix(d.route, "settings:") {
			selected = "storage"
		}
	}
	widget, stop := newFilesPage(d.w, selected)
	d.detail.SetContent(widget)
	d.stopPage = stop
}
func (d *desktopUI) action(action, folder, device string) {
	d.run("engine.action", desktopapi.Request{Action: action, Folder: folder, Device: device}, nil)
}
func (d *desktopUI) overview() {
	d.reset("Overview")
	d.serviceButtons()
	groups := map[string]*adw.PreferencesGroup{}
	labels := map[string]string{"status": "Status", "attention": "Needs Attention", "summary": "Synchronization", "storage": "Storage"}
	if d.mode == modeFiles {
		labels["summary"] = "Gateways"
	}
	for _, destination := range desktopui.Read().Destinations {
		if destination.ID == "overview" {
			for _, id := range destination.Sections {
				groups[id] = d.group(labels[id])
			}
		}
	}
	// What the engine receives and sends, or else whether other devices are connected (each opens its list)
	var receiving, sending *transferRow
	var connection, downloads, uploads *adw.ActionRow
	if d.mode == modeSynchronize {
		receiving = newTransferRow(true, func() { d.openTransfers(true) })
		sending = newTransferRow(false, func() { d.openTransfers(false) })
		connection = row("", "")
		for _, r := range []*adw.ActionRow{receiving.row, sending.row, connection} {
			r.SetVisible(false)
			groups["status"].Add(r)
		}
		// The lists, also when nothing goes that way (shown in their place: a list has one way in)
		downloads = choiceRow("Downloads", "", func() { d.openTransfers(true) })
		uploads = choiceRow("Uploads", "", func() { d.openTransfers(false) })
		groups["summary"].Add(downloads)
		groups["summary"].Add(uploads)
	}
	heading := gtk.NewLabel("Connecting to the background service…")
	heading.SetXAlign(0)
	heading.SetWrap(true)
	groups["status"].Add(heading)
	identity := gtk.NewLabel("")
	identity.SetSelectable(true)
	identity.SetWrap(true)
	identity.SetXAlign(0)
	groups["status"].Add(identity)
	mountPath := ""
	openMount := textButton("Open in File Manager", func() {
		if mountPath != "" {
			d.w.openPath(mountPath)
		}
	})
	openMount.SetVisible(d.mode == modeFiles)
	groups["status"].Add(openMount)
	if d.mode == modeSynchronize {
		d.load("engine.folders", nil, func(data json.RawMessage) {
			var folders []desktopapi.Folder
			if err := json.Unmarshal(data, &folders); err != nil {
				d.error(err)
				return
			}
			for _, folder := range folders {
				folder := folder
				groups["summary"].Add(choiceRow(folder.Label, folder.Path, func() { d.w.openPath(folder.Path) }))
			}
		})
	}
	reason := gtk.NewLabel("")
	reason.SetXAlign(0)
	reason.SetWrap(true)
	groups["attention"].Add(reason)
	gatewaySummary := gtk.NewLabel("")
	gatewaySummary.SetXAlign(0)
	gatewaySummary.SetVisible(d.mode == modeFiles)
	groups["summary"].Add(gatewaySummary)
	summary := gtk.NewLabel("")
	summary.SetXAlign(0)
	summary.SetWrap(true)
	groups["storage"].Add(summary)
	if d.mode == modeSynchronize {
		d.button("Pause All", func() { d.action("pauseAll", "", "") })
		d.button("Resume All", func() { d.action("resumeAll", "", "") })
		d.button("Rescan All", func() { d.action("rescanAll", "", "") })
	}

	type snapshot struct {
		Status struct {
			DeviceID, Name, State, Mount string
			Folders                      []struct{ Label, State string }
			RestartRequired              bool
		}
		Stats struct {
			Totals struct {
				Local, LocalNeed *struct {
					Files int
					Bytes int64
				}
			}
		}
		Transfers    liveTransfers
		Devices      []desktopapi.Device
		DevicesKnown bool
		Policy       struct{ Reason string }
		Storage      struct{ Used, Kept, Budget int64 }
	}
	pollPage(d, func(ctx context.Context) (snapshot, error) {
		var state snapshot
		method := "engine.status"
		if d.mode == modeFiles {
			method = "status"
		}
		if err := d.request(ctx, method, nil, &state.Status); err != nil {
			return state, err
		}
		if d.mode == modeFiles {
			if err := d.request(ctx, "storage", nil, &state.Storage); err != nil {
				return state, err
			}
		}
		if d.mode == modeSynchronize {
			if err := d.request(ctx, "engine.stats", nil, &state.Stats); err != nil {
				return state, err
			}
			var err error
			if state.Transfers, err = d.fetchTransfers(ctx); err != nil {
				return state, err
			}
			state.DevicesKnown = d.request(ctx, "engine.devices", nil, &state.Devices) == nil
			_ = d.request(ctx, "policy.get", nil, &state.Policy)
		}
		return state, nil
	}, func(s snapshot, err error) {
		if err != nil {
			heading.SetText("The background service is stopped or unavailable.")
			if d.mode == modeSynchronize {
				// What they said isn't known any more
				receiving.row.SetVisible(false)
				sending.row.SetVisible(false)
				connection.SetVisible(false)
				downloads.SetVisible(true)
				uploads.SetVisible(true)
			}
			return
		}
		if d.mode == modeFiles {
			heading.SetText("Files mode — folders are fetched as you open them")
			identity.SetText(s.Status.Mount)
			mountPath = s.Status.Mount
			openMount.SetSensitive(mountPath != "")
			gatewaySummary.SetText(fmt.Sprintf("%d folders available", len(s.Status.Folders)))
			summary.SetText(fmt.Sprintf("Cached: %s · Kept: %s · Cache budget: %s", humanBytes(s.Storage.Used), humanBytes(s.Storage.Kept), humanBytes(s.Storage.Budget)))
			reason.SetText("Gateway connection and pending-edit problems are shown in Remote Access and Activity.")
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
		download, upload := s.Transfers.Download, s.Transfers.Upload
		receiving.set(download)
		receiving.row.SetVisible(download != nil)
		downloads.SetVisible(download == nil)
		sending.set(upload)
		sending.row.SetVisible(upload != nil)
		uploads.SetVisible(upload == nil)
		title, subtitle := connectionStatus(s.Devices)
		connection.SetTitle(title)
		connection.SetSubtitle(subtitle)
		connection.SetVisible(download == nil && upload == nil && s.DevicesKnown)
		message := s.Policy.Reason
		if s.Status.RestartRequired {
			message += " Saved settings require an engine restart."
		}
		reason.SetText(strings.TrimSpace(message))
	})()
}

// statsProgress is a direction's totals in engine.stats
type statsProgress struct {
	FilesTotal, BytesTotal, BytesDone int64
	Percentage                        float64
}

// totals are statsProgress as engine.transfers has them, without the speed (nil when nothing goes that way)
func (p *statsProgress) totals() *desktopapi.TransferTotals {
	if p == nil || p.BytesTotal == 0 {
		return nil
	}
	return &desktopapi.TransferTotals{FilesTotal: p.FilesTotal, BytesDone: p.BytesDone, BytesTotal: p.BytesTotal,
		Percentage: p.Percentage, SecondsRemaining: -1}
}

// connectionStatus is whether other devices are connected, and how many of them
func connectionStatus(devices []desktopapi.Device) (string, string) {
	connected := 0
	for _, device := range devices {
		if device.Connected {
			connected++
		}
	}
	title := "Not connected"
	if connected > 0 {
		title = "Connected"
	}
	if len(devices) == 0 {
		return title, ""
	}
	return title, fmt.Sprintf("%d of %s connected", connected, plural(len(devices), "device", "devices"))
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
