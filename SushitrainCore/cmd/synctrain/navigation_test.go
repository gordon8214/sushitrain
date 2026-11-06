// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/desktopui"
	"t-shaped.nl/sushitrain/v2/files"
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
	server.Register("settings.index", func(context.Context, json.RawMessage) (any, error) {
		return []desktopui.SearchEntry{{ID: "test-search", Title: "Test setting", Breadcrumb: "Folders › Documents", Category: "advanced", Action: "advanced/options", Section: "options", Field: "test", Anchor: "test", Capability: "synchronize"}}, nil
	})
	var savedValues atomic.Value
	server.Register("settings.get", func(_ context.Context, raw json.RawMessage) (any, error) {
		var request desktopapi.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if request.Section == "folder/advanced-fixture" || request.Section == "reset/defaults/folder" || request.Section == "reset/folder/test" {
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
	for _, method := range []string{"files.children", "engine.folders", "engine.devices", "engine.pending", "engine.ignored", "engine.pendingFolders", "engine.versions", "engine.search", "files.search", "history.list"} {
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
	d := &desktopUI{w: w, route: "settings:general", mode: modeFiles, socket: socket, navigation: adw.NewNavigationView()}
	d.resolveOnce.Do(func() {})
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.close()
	d.sidebar = gtk.NewListBox()
	d.sidebarBack, d.sidebarSettings = gtk.NewButton(), gtk.NewButton()
	d.root = adw.NewNavigationSplitView()
	side := adw.NewToolbarView()
	header := adw.NewHeaderBar()
	side.AddTopBar(header)
	searchColumn := gtk.NewBox(gtk.OrientationVertical, 0)
	sidebarScroll := gtk.NewScrolledWindow()
	sidebarScroll.SetChild(d.sidebar)
	d.installSettingsSearch(header, searchColumn, sidebarScroll)
	side.SetContent(searchColumn)
	d.root.SetSidebar(adw.NewNavigationPage(side, "Settings"))
	d.navigation.SetAnimateTransitions(false)
	d.navigation.ConnectPopped(func(*adw.NavigationPage) { d.pagePopped() })
	d.root.SetContent(adw.NewNavigationPage(d.navigation, "Settings"))
	w.toasts.SetChild(d.root)

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
			if len(d.pages) != 1 || d.body != rootBody || d.navigation.VisiblePage().Title() != "General" {
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
	for _, section := range []string{"reset/defaults/folder", "reset/folder/test"} {
		d.edit(section, "Reset Synchronization Settings")
		waitNavigationUI(t, func() bool { return d.editor != "" })
		if d.dirty {
			t.Fatal("preview started with unrelated dirty state")
		}
		var notice bool
		var visit func(gtk.Widgetter)
		visit = func(widget gtk.Widgetter) {
			w := gtk.BaseWidget(widget)
			if label, ok := w.Cast().(*gtk.Label); ok && label.Text() == desktopapi.FolderResetDescription {
				notice = true
			}
			for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
				visit(child)
			}
		}
		visit(d.body)
		if !notice {
			t.Fatal("reset preview does not explain Save/Cancel scope")
		}
		d.goBack()
		waitNavigationUI(t, func() bool { return d.editor == "" })
	}
	testFolderAdvanced(t, d, &rejectSave, &savedValues)
	testManagementNavigation(t, d)
	testBrowserNavigation(t, d)
	testFilesManagementInvalidation(t, d, server)

	d.sidebar = gtk.NewListBox()
	for range desktopRoutes {
		d.sidebar.Append(gtk.NewListBoxRow())
	}
	d.root = adw.NewNavigationSplitView()
	d.sidebarBack = gtk.NewButton()
	d.sidebarSettings = gtk.NewButton()
	for _, mode := range []string{modeFiles, modeSynchronize} {
		d.mode = mode
		for _, category := range desktopui.Read().Settings {
			var sections []desktopui.Section
			for _, section := range category.Sections {
				if section.Available("gnome", mode) {
					sections = append(sections, section)
				}
			}
			if len(sections) == 0 {
				continue
			}
			d.navigate("settings:" + category.ID)
			expected := 18
			if mode == modeFiles {
				expected = 13
			}
			if len(d.sidebarRoutes) != expected {
				t.Fatal("unified navigation missing", d.sidebarRoutes)
			}
			i := 0
			for child := d.body.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
				if group, ok := gtk.BaseWidget(child).Cast().(*adw.PreferencesGroup); ok {
					if i >= len(sections) || group.Title() != sections[i].Title {
						t.Fatalf("%s section order differs", category.ID)
					}
					if sections[i].Action == "mode" {
						current, target := "Synchronize", "Files"
						if mode == modeFiles {
							current, target = "Files", "Synchronize"
						}
						r := navigationWidget[*adw.ActionRow](group)
						if r == nil || r.Subtitle() != "Current mode: "+current {
							t.Fatalf("General does not show the current %s mode", current)
						}
						if navigationButton(group, "Open Switch to "+target+"…") == nil {
							t.Fatalf("General does not offer switching to %s", target)
						}
					}
					i++
				}
			}
			if i != len(sections) {
				t.Fatalf("%s missing sections", category.ID)
			}
		}
	}

	d.route = "Overview"
	d.navigate("Settings")
	if d.route != "settings:general" || d.sidebar.SelectedRow().Index() != 7 || len(d.sidebarRoutes) != 18 {
		t.Fatal("Settings shortcut did not select General in the unified sidebar")
	}

	d.mode = modeSynchronize
	d.settingsSearchButton.SetActive(true)
	waitNavigationUI(t, func() bool { return len(d.settingsSearchIndex) == 1 })
	d.settingsSearchEntry.SetText("documents test")
	d.showSettingsSearch()
	if d.settingsSearchStack.VisibleChildName() != "results" || d.editor != "" {
		t.Fatal("search did not show results independently of editors")
	}
	d.openSettingsSearch(d.settingsSearchIndex[0])
	waitNavigationUI(t, func() bool { return d.editor != "" })
	navigationTestSwitch(d.body).SetActive(true)
	d.settingsSearchEntry.SetText("MCP")
	d.showSettingsSearch()
	if !d.dirty || d.editor != "fixture" || w.win.VisibleDialog() != nil {
		t.Fatal("typing in search discarded or interrupted draft")
	}
	d.guard(func() { d.openSettingsSearch(desktopui.SearchEntry{Title: "MCP", Action: "mcp", Capability: "always"}) })
	waitNavigationUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog = w.win.VisibleDialog()
	dialog.Emit("response", "cancel")
	dialog.ForceClose()
	if !d.dirty || d.editor != "fixture" {
		t.Fatal("canceled search departure lost the draft")
	}
	d.settingsSearchEntry.SetText("")
	d.showSettingsSearch()
	if d.settingsSearchStack.VisibleChildName() != "categories" || !d.dirty {
		t.Fatal("clearing search lost categories or draft")
	}

	// An accepted result replaces the previous editor even within one category.
	second := d.settingsSearchIndex[0]
	second.ID, second.Title, second.Action = "second-search", "Second setting", "advanced/gui"
	d.guard(func() { d.openSettingsSearch(second) })
	waitNavigationUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog = w.win.VisibleDialog()
	dialog.Emit("response", "go")
	dialog.ForceClose()
	waitNavigationUI(t, func() bool { return d.editor != "" && d.navigation.VisiblePage().Title() == "Second setting" })
	if len(d.pages) != 2 {
		t.Fatal("search retained a discarded editor in the Back stack")
	}
	d.goBack()
	if len(d.pages) != 1 || d.editor != "" {
		t.Fatal("Back restored a closed search editor")
	}
	d.openSettingsSearch(d.settingsSearchIndex[0])
	waitNavigationUI(t, func() bool { return d.editor != "" })
	navigationTestSwitch(d.body).SetActive(true)
	navigationButton(d.commands, "Save").Emit("clicked")
	waitNavigationUI(t, func() bool { return !d.saving })
	if len(d.pages) != 1 || d.dirty || d.banner.Revealed() {
		t.Fatal("fresh search editor could not save after Back")
	}

	// The first metadata request resolves its own endpoint without stranding loading.
	helperDir := t.TempDir()
	home, err := json.Marshal(map[string]string{"socket": socket})
	if err != nil {
		t.Fatal(err)
	}
	helper := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(string(home), "'", "'\\''") + "'\n"
	if err := os.WriteFile(filepath.Join(helperDir, "synctrain-gateway"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	unresolved := &desktopUI{mode: modeSynchronize}
	unresolved.loadSettingsSearch()
	waitNavigationUI(t, func() bool { return !unresolved.settingsSearchLoading })
	if unresolved.socket != socket || len(unresolved.settingsSearchIndex) != 1 {
		t.Fatal("first resolved search snapshot was discarded")
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
		// Switching destinations retains the native browser widgets and state too.
		d.navigate("History")
		waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
		d.navigate("Files")
		if d.body != parentBody || filter.Text() != "Example" || d.browserSelected != selected {
			t.Fatal("destination navigation lost browser state")
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
		// A search owns its filter and survives a metadata requery independently.
		d.searchFiles("Different")
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if d.browserFilter != "" || !d.pages[len(d.pages)-1].browser {
			t.Fatal("search inherited its containing folder filter")
		}
		d.navigate("History")
		waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
		d.filesCacheInvalid = true
		d.navigate("Files")
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if d.pages[len(d.pages)-1].page.Title() != "Search Results" || !d.browserSearch {
			t.Fatal("invalidated search did not requery its results")
		}
		d.goBack()
		// The latest containing filter must win over a previous child snapshot.
		filter.SetText("Updated")
		d.navigate("History")
		waitNavigationUI(t, func() bool { return d.pages[0].pending == 0 })
		d.filesCacheInvalid = true // A completed mutation on another destination invalidates metadata.
		d.navigate("Files")
		waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
		if d.body == parentBody || d.browserFilter != "Updated" || d.parent != parent {
			t.Fatal("invalidated browsing did not reload while retaining its location/filter")
		}
	}
}

func testFilesManagementInvalidation(t *testing.T, d *desktopUI, server *adminsock.Server) {
	t.Helper()
	var removed atomic.Bool
	server.Register("status", func(context.Context, json.RawMessage) (any, error) {
		status := files.Status{Mount: "/tmp/desktop-ui-fixture"}
		if !removed.Load() {
			status.Folders = []files.Folder{{ID: 1, DirName: "Mounted Folder", Listed: true}}
		}
		return status, nil
	})
	server.Register("pending.list", func(context.Context, json.RawMessage) (any, error) { return []files.Edit{}, nil })
	server.Register("storage", func(context.Context, json.RawMessage) (any, error) { return files.Storage{}, nil })
	server.Register("folders.remove", func(context.Context, json.RawMessage) (any, error) {
		removed.Store(true)
		return files.Removal{}, nil
	})
	server.Register("files.childrenPage", func(context.Context, json.RawMessage) (any, error) {
		entries := []map[string]any{}
		if !removed.Load() {
			entries = append(entries, map[string]any{"Item": 42, "Name": "Mounted Folder", "Kind": 3})
		}
		return map[string]any{"Entries": entries, "Next": 0}, nil
	})
	d.w.desk = d
	d.mode = modeFiles
	d.openFilesAt("", "", 0)
	waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
	browser := d.body
	if len(d.browserRows) != 1 {
		t.Fatal("missing mounted folder fixture")
	}
	d.navigate("Folders")
	waitNavigationUI(t, func() bool { return navigationButton(d.detail, "Remove from This Device") != nil })
	navigationButton(d.detail, "Remove from This Device").Emit("clicked")
	waitNavigationUI(t, func() bool { return d.w.win.VisibleDialog() != nil })
	dialog := d.w.win.VisibleDialog()
	dialog.Emit("response", "go")
	dialog.ForceClose()
	waitNavigationUI(t, func() bool { return removed.Load() && d.filesCacheInvalid })
	d.navigate("Files")
	waitNavigationUI(t, func() bool { return d.pages[len(d.pages)-1].pending == 0 })
	if d.body == browser || len(d.browserRows) != 0 {
		t.Fatal("removed folder survived in retained browser")
	}
}

func navigationButton(widget gtk.Widgetter, label string) *gtk.Button {
	w := gtk.BaseWidget(widget)
	if menu, ok := w.Cast().(*gtk.MenuButton); ok && menu.Popover() != nil {
		if button := navigationButton(menu.Popover(), label); button != nil {
			return button
		}
	}
	if b, ok := w.Cast().(*gtk.Button); ok && (b.Label() == label || b.TooltipText() == label) {
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
