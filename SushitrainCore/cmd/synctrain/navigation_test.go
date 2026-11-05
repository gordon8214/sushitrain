// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
)

// Run under xvfb-run and dbus-run-session. All GTK calls stay on one OS thread;
// service requests use an isolated socket and never touch the user's engine.
func TestNavigation(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	adw.Init()
	socket := filepath.Join(t.TempDir(), "test.sock")
	t.Setenv("SYNCTRAIN_FILES_SOCKET", socket)
	server := adminsock.NewServer()
	defer server.Close()
	server.Register("mcp.settings", func(context.Context, json.RawMessage) (any, error) { return map[string]bool{"Enabled": false}, nil })
	var savedValues atomic.Value
	server.Register("settings.get", func(_ context.Context, raw json.RawMessage) (any, error) {
		var request desktopapi.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if request.Section == "folder/advanced-fixture" {
			return desktopapi.Editor{Token: "fixture", Fields: []desktopapi.Field{
				{Path: "test", Label: "Test setting", Kind: "bool"},
				{Path: "Copiers", Label: "Copiers", Kind: "string", Value: "7", IsAdvanced: true},
			}}, nil
		}
		return desktopapi.Editor{Token: "fixture", Fields: []desktopapi.Field{{Path: "test", Label: "Test setting", Kind: "bool"}}}, nil
	})
	server.Register("settings.close", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	var rejectSave atomic.Bool
	server.Register("settings.save", func(_ context.Context, raw json.RawMessage) (any, error) {
		var request desktopapi.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		savedValues.Store(request.Values)
		if rejectSave.Load() {
			return nil, errors.New("invalid test setting")
		}
		return nil, nil
	})
	for _, method := range []string{"engine.folders", "engine.devices", "engine.pending", "engine.ignored", "engine.pendingFolders", "engine.versions", "history.list"} {
		server.Register(method, func(context.Context, json.RawMessage) (any, error) { return []any{}, nil })
	}
	for _, method := range []string{"engine.browsePage", "files.childrenPage"} {
		server.Register(method, func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"Entries": []any{}, "Next": 0}, nil
		})
	}
	server.Register("files.historyTarget", func(context.Context, json.RawMessage) (any, error) { return map[string]any{}, nil })
	server.Register("files.versions", func(context.Context, json.RawMessage) (any, error) { return map[string]any{}, nil })
	server.Register("engine.action", func(_ context.Context, raw json.RawMessage) (any, error) {
		var request desktopapi.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if request.Action == "ignores" {
			return []string{"*.tmp"}, nil
		}
		return []any{}, nil
	})
	if err := server.Listen(context.Background(), socket, false); err != nil {
		t.Fatal(err)
	}
	app := adw.NewApplication("com.gordonbyrnes.Synctrain.NavigationTest", gio.ApplicationNonUnique)
	if err := app.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := &mainWindow{app: app, win: adw.NewApplicationWindow(&app.Application), toasts: adw.NewToastOverlay()}
	defer w.win.Destroy()
	w.win.SetContent(w.toasts)
	w.win.SetTitle("Synctrain Navigation Test")
	w.win.SetDefaultSize(900, 600)
	w.win.Present()
	d := &desktopUI{w: w, route: "Settings", mode: modeFiles, socket: socket, navigation: adw.NewNavigationView()}
	d.resolveOnce.Do(func() {})
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.close()
	d.navigation.SetAnimateTransitions(false)
	d.navigation.ConnectPopped(func(*adw.NavigationPage) { d.pagePopped() })
	w.toasts.SetChild(d.navigation)
	d.showRoute()
	rootBody := d.body
	for _, mode := range []string{modeFiles, modeSynchronize} {
		d.mode = mode
		for _, open := range []func(){d.mcpSettings, d.previewSettings, d.resourceSettings} {
			open()
			waitNavigationUI(t, func() bool { return d.navigation.VisiblePage().Title() != "Loading…" })
			title := d.navigation.VisiblePage().Title()
			d.refreshView()
			if len(d.pages) != 2 || d.navigation.VisiblePage().Title() != title {
				t.Fatal("refresh changed the current settings destination")
			}
			button := nativeBack(d.detail)
			if button == nil {
				t.Fatalf("%s in %s mode has no native header Back button", d.navigation.VisiblePage().Title(), mode)
			}
			button.Emit("clicked")
			if len(d.pages) != 1 || d.body != rootBody || d.navigation.VisiblePage().Title() != "Settings" {
				t.Fatal("Back did not restore the original settings list")
			}
		}
	}

	// A nested editor must return to its category after saving or discarding.
	d.subpage(func() { d.reset("Advanced Configuration") })
	d.edit("advanced/options", "Options")
	waitNavigationUI(t, func() bool { return d.editor != "" })
	setting := navigationTestSwitch(d.body)
	if setting == nil {
		t.Fatal("editor did not render the setting")
	}
	setting.SetActive(true)
	p := d.pages[len(d.pages)-1]
	if p.page.CanPop() || !p.back.Visible() {
		t.Fatal("unsaved edits allow an unguarded pop or hide guarded Back")
	}
	navigationKey(t, d, uint(gdk.KEY_Left), gdk.AltMask)
	waitNavigationUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog := w.win.VisibleDialog()
	if d.handleBackKey(uint(gdk.KEY_Escape), 0, 0) {
		t.Fatal("page intercepted a modal dialog's Escape key")
	}
	dialog.Emit("response", "cancel")
	dialog.ForceClose()
	if !d.dirty || len(d.pages) != 3 || d.editor != "fixture" {
		t.Fatal("canceling discard lost the editor or its draft")
	}
	p.back.Emit("clicked")
	waitNavigationUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog = w.win.VisibleDialog()
	dialog.Emit("response", "go")
	dialog.ForceClose()
	if d.dirty || d.editor != "" || len(d.pages) != 2 || d.navigation.VisiblePage().Title() != "Advanced Configuration" {
		t.Fatal("discard did not return to the immediate parent and close its editor")
	}
	d.edit("advanced/options", "Options")
	waitNavigationUI(t, func() bool { return d.editor != "" })
	d.setDirty(true)
	rejectSave.Store(true)
	save := navigationButton(d.commands, "Save")
	if focus := w.win.Focus(); focus == nil || !gtk.BaseWidget(focus).IsAncestor(d.detail) {
		t.Fatal("loaded editor lost keyboard focus")
	}
	if save == nil {
		t.Fatal("editor has no Save button")
	}
	save.Emit("clicked")
	if !d.saving || d.pages[len(d.pages)-1].page.CanPop() {
		t.Fatal("an editor can be popped during Save")
	}
	d.goBack()
	if len(d.pages) != 3 {
		t.Fatal("Back left the editor during Save")
	}
	waitNavigationUI(t, func() bool { return !d.saving })
	if len(d.pages) != 3 || !d.dirty || d.editor != "fixture" || !d.banner.Revealed() {
		t.Fatal("failed Save lost the editor, draft, or validation error")
	}
	rejectSave.Store(false)
	save.Emit("clicked")
	waitNavigationUI(t, func() bool { return !d.saving })
	if len(d.pages) != 2 || d.navigation.VisiblePage().Title() != "Advanced Configuration" {
		t.Fatal("Save bypassed the editor's parent")
	}
	d.goBack()
	if len(d.pages) != 1 || d.body != rootBody {
		t.Fatal("nested navigation did not return to the original list")
	}
	loads := 0
	d.subpage(func() {
		loads++
		d.reset("Pending Parent")
		d.load("engine.pendingFolders", nil, func(json.RawMessage) { d.text("Loaded") })
	})
	d.subpage(func() { d.reset("Child") })
	d.goBack()
	waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
	if loads != 2 {
		t.Fatal("returning to an interrupted parent did not reload it")
	}
	d.goBack()
	testFolderAdvanced(t, d, &rejectSave, &savedValues)
	testManagementNavigation(t, d)
	testBrowserNavigation(t, d)

	d.sidebar = gtk.NewListBox()
	for range desktopRoutes {
		d.sidebar.Append(gtk.NewListBoxRow())
	}
	d.root = adw.NewNavigationSplitView()
	d.route = "Overview"
	d.navigate("Settings")
	if d.sidebar.SelectedRow().Index() != len(desktopRoutes)-1 {
		t.Fatal("opening Settings with a shortcut did not select its sidebar row")
	}
}

func waitNavigationUI(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("GTK operation did not finish within three seconds")
		}
		glib.MainContextDefault().Iteration(false)
		runtime.Gosched()
	}
}

func nativeBack(widget gtk.Widgetter) *gtk.Button {
	w := gtk.BaseWidget(widget)
	if b, ok := w.Cast().(*gtk.Button); ok && b.Visible() && b.IconName() == "go-previous-symbolic" {
		return b
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if button := nativeBack(child); button != nil {
			return button
		}
	}
	return nil
}

func navigationTestSwitch(widget gtk.Widgetter) *adw.SwitchRow {
	w := gtk.BaseWidget(widget)
	if r, ok := w.Cast().(*adw.SwitchRow); ok && r.Title() == "Test setting" {
		return r
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if r := navigationTestSwitch(child); r != nil {
			return r
		}
	}
	return nil
}

func testManagementNavigation(t *testing.T, d *desktopUI) {
	t.Helper()
	for _, section := range []struct {
		route string
		open  func()
	}{
		{"Folders", func() { d.edit("basic/folder/test", "Folder Settings") }},
		{"Folders", func() { d.share(desktopapi.Folder{ID: "test", Label: "Documents"}) }},
		{"Folders", func() { d.ignores("test") }},
		{"Folders", func() { d.failedItems("test") }},
		{"Folders", func() { d.deletedFiles("test") }},
		{"Folders", func() { d.addFolder("offered", "device", false) }},
		{"Devices", func() { d.edit("device/test", "Device Settings") }},
		{"Devices", func() { d.offeredFolders("test") }},
	} {
		d.mode, d.route = modeSynchronize, section.route
		d.showRoute()
		waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
		parentBody := d.body
		section.open()
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if len(d.pages) != 2 || nativeBack(d.detail) == nil {
			t.Fatalf("%s has no standard header Back", d.navigation.VisiblePage().Title())
		}
		if navigationButton(d.body, "Back") != nil || navigationButton(d.body, "Cancel") != nil {
			t.Fatal("inline page still has competing navigation buttons")
		}
		nativeBack(d.detail).Emit("clicked")
		if len(d.pages) != 1 || d.body != parentBody {
			t.Fatal("management Back did not retain the parent view")
		}
	}
	d.edit("folder/test", "Folder Settings")
	waitNavigationUI(t, func() bool { return d.editor != "" })
	d.setDirty(true)
	navigationButton(d.commands, "Save").Emit("clicked")
	waitNavigationUI(t, func() bool { return !d.saving && len(d.pages) == 1 && d.pages[0].pending == 0 })
	if d.navigation.VisiblePage().Title() != "Devices" || d.editor != "" {
		t.Fatal("saved editor did not restore its sidebar location")
	}
	// Text editors and Add Folder protect drafts like settings do.
	d.route = "Folders"
	d.showRoute()
	waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
	for _, open := range []func(){func() { d.ignores("test") }, func() { d.addFolder("test", "", false) }} {
		open()
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if editor := navigationWidget[*gtk.TextView](d.body); editor != nil {
			editor.Buffer().SetText("changed")
		} else {
			entry := navigationWidget[*adw.EntryRow](d.body)
			if entry == nil {
				t.Fatal("Add Folder has no entry")
			}
			entry.SetText("changed")
		}
		p := d.pages[len(d.pages)-1]
		if !d.dirty || p.page.CanPop() || !p.back.Visible() || p.back.TooltipText() != "Back to Folders" {
			t.Fatal("draft does not have guarded, labeled header navigation")
		}
		d.navigation.SetDirection(gtk.TextDirRTL)
		navigationKey(t, d, uint(gdk.KEY_Right), gdk.AltMask)
		waitNavigationUI(t, func() bool { return d.w.win.VisibleDialog() != nil })
		dialog := d.w.win.VisibleDialog()
		dialog.Emit("response", "cancel")
		dialog.ForceClose()
		if !d.dirty || len(d.pages) != 2 {
			t.Fatal("canceled Back lost draft")
		}
		d.navigation.SetDirection(gtk.TextDirLTR)
		navigationKey(t, d, uint(gdk.KEY_Escape), 0)
		waitNavigationUI(t, func() bool { return d.w.win.VisibleDialog() != nil })
		dialog = d.w.win.VisibleDialog()
		dialog.Emit("response", "go")
		dialog.ForceClose()
		if d.dirty || len(d.pages) != 1 {
			t.Fatal("discard did not leave editor")
		}
	}
}

func testBrowserNavigation(t *testing.T, d *desktopUI) {
	t.Helper()
	for _, mode := range []string{modeFiles, modeSynchronize} {
		d.mode, d.route, d.folder = mode, "Files", "test"
		d.showRoute()
		waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
		r := browserRow{name: "Example.txt", path: "Example.txt", item: 42}
		d.browserRows = []browserRow{r}
		d.showBrowser(d.browserRows)
		filter := navigationWidget[*gtk.SearchEntry](d.body)
		filter.SetText("Example")
		parentBody, selected := d.body, d.browserSelected
		d.fileActions(r)
		actions := d.body
		navigationButton(d.commands, "Previous Versions").Emit("clicked")
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if len(d.pages) != 3 {
			t.Fatal("history did not push onto file actions")
		}
		d.goBack()
		if d.body != actions || len(d.pages) != 2 {
			t.Fatal("history skipped file actions")
		}
		d.goBack()
		if d.body != parentBody || filter.Text() != "Example" || d.browserSelected != selected || len(d.browserRows) != 1 {
			t.Fatal("Back lost browser filters, selection, or data")
		}
		// Directories use native Back and restore their containing location.
		directory := browserRow{name: "Subfolder", path: "Subfolder", item: 9, directory: true}
		parent := d.parent
		d.openBrowserRow(directory)
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if len(d.pages) != 2 || nativeBack(d.detail) == nil {
			t.Fatal("directory has no Back")
		}
		d.refreshView()
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		d.goBack()
		if d.body != parentBody || d.parent != parent || d.directory != "" {
			t.Fatal("directory Back lost its parent location")
		}
	}
}

func navigationButton(widget gtk.Widgetter, label string) *gtk.Button {
	w := gtk.BaseWidget(widget)
	if b, ok := w.Cast().(*gtk.Button); ok && b.Label() == label {
		return b
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if b := navigationButton(child, label); b != nil {
			return b
		}
	}
	return nil
}

func navigationWidget[T interface {
	gtk.Widgetter
	comparable
}](widget gtk.Widgetter) T {
	w := gtk.BaseWidget(widget)
	if v, ok := w.Cast().(T); ok {
		return v
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		v := navigationWidget[T](child)
		var empty T
		if v != empty {
			return v
		}
	}
	var empty T
	return empty
}

func navigationKey(t *testing.T, d *desktopUI, key uint, modifiers gdk.ModifierType) {
	t.Helper()
	if !d.handleBackKey(key, 0, modifiers) {
		t.Fatal("guarded Back key was not handled")
	}
}

func testFolderAdvanced(t *testing.T, d *desktopUI, rejectSave *atomic.Bool, savedValues *atomic.Value) {
	t.Helper()
	open := func() (*adw.ExpanderRow, *adw.EntryRow) {
		d.edit("folder/advanced-fixture", "Folder settings")
		waitNavigationUI(t, func() bool { return d.editor != "" })
		var advanced *adw.ExpanderRow
		var input *adw.EntryRow
		var visit func(gtk.Widgetter)
		visit = func(widget gtk.Widgetter) {
			w := gtk.BaseWidget(widget)
			if row, ok := w.Cast().(*adw.ExpanderRow); ok && row.Title() == "Advanced" {
				advanced = row
			}
			if row, ok := w.Cast().(*adw.EntryRow); ok && row.Title() == "Copiers" {
				input = row
			}
			for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
				visit(child)
			}
		}
		visit(d.body)
		if advanced == nil || input == nil {
			t.Fatal("missing advanced controls")
		}
		if advanced.Expanded() {
			t.Fatal("advanced settings start expanded")
		}
		return advanced, input
	}
	advanced, _ := open()
	advanced.SetExpanded(true)
	advanced.SetExpanded(false)
	if d.dirty {
		t.Fatal("expansion dirtied the draft")
	}
	d.goBack()
	waitNavigationUI(t, func() bool { return d.editor == "" })
	advanced, input := open()
	advanced.SetExpanded(true)
	input.SetText("invalid")
	advanced.SetExpanded(false)
	advanced.SetExpanded(true)
	if input.Text() != "invalid" || !d.dirty {
		t.Fatal("collapse lost the pending edit")
	}
	advanced.SetExpanded(false)
	rejectSave.Store(true)
	save := navigationButton(d.commands, "Save")
	if save == nil {
		t.Fatal("missing Save")
	}
	save.Emit("clicked")
	waitNavigationUI(t, func() bool { return !d.saving })
	if !advanced.Expanded() || input.Text() != "invalid" {
		t.Fatal("failed save did not reveal and preserve advanced edits")
	}
	input.SetText("9")
	advanced.SetExpanded(false)
	rejectSave.Store(false)
	save.Emit("clicked")
	waitNavigationUI(t, func() bool { return !d.saving })
	values := savedValues.Load().(map[string]string)
	if values["Copiers"] != "9" || values["test"] != "false" {
		t.Fatalf("saved values: %v", values)
	}
}
