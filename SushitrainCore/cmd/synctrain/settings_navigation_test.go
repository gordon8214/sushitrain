// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
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
func TestSettingsNavigation(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	adw.Init()
	socket := filepath.Join(t.TempDir(), "test.sock")
	t.Setenv("SYNCTRAIN_FILES_SOCKET", socket)
	server := adminsock.NewServer()
	defer server.Close()
	server.Register("mcp.settings", func(context.Context, json.RawMessage) (any, error) { return map[string]bool{"Enabled": false}, nil })
	server.Register("settings.get", func(context.Context, json.RawMessage) (any, error) {
		return desktopapi.Editor{Token: "fixture", Fields: []desktopapi.Field{{Path: "test", Label: "Test setting", Kind: "bool"}}}, nil
	})
	server.Register("settings.close", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	server.Register("settings.save", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
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
	w.win.SetDefaultSize(900, 600)
	w.win.Present()
	d := &desktopUI{w: w, route: "Settings", mode: modeFiles, socket: socket, navigation: adw.NewNavigationView()}
	d.resolveOnce.Do(func() {})
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.close()
	d.navigation.SetAnimateTransitions(false)
	d.navigation.ConnectPopped(func(*adw.NavigationPage) { d.settingsPopped() })
	w.toasts.SetChild(d.navigation)
	d.showRoute()
	rootBody := d.body
	for _, mode := range []string{modeFiles, modeSynchronize} {
		d.mode = mode
		for _, open := range []func(){d.mcpSettings, d.previewSettings, d.resourceSettings} {
			open()
			waitSettingsUI(t, func() bool { return d.navigation.VisiblePage().Title() != "Settings" })
			title := d.navigation.VisiblePage().Title()
			d.refreshView()
			if len(d.pages) != 2 || d.navigation.VisiblePage().Title() != title {
				t.Fatal("refresh changed the current settings destination")
			}
			button := nativeSettingsBack(d.detail)
			if button == nil {
				t.Fatalf("%s in %s mode has no native header Back button", d.navigation.VisiblePage().Title(), mode)
			}
			button.Emit("clicked")
			if len(d.pages) != 1 || d.body != rootBody || d.navigation.VisiblePage().Title() != "Settings" {
				t.Fatal("Back did not restore the original settings list")
			}
		}
	}

	// A nested editor must return to its category after Save or Cancel.
	d.subpage(func() { d.reset("Advanced Configuration") })
	d.edit("advanced/options", "Options")
	waitSettingsUI(t, func() bool { return d.editor != "" })
	setting := settingsTestSwitch(d.body)
	if setting == nil {
		t.Fatal("editor did not render the setting")
	}
	setting.SetActive(true)
	p := d.pages[len(d.pages)-1]
	if p.page.CanPop() || !p.back.Visible() {
		t.Fatal("unsaved edits allow an unguarded pop or hide guarded Back")
	}
	p.back.Emit("clicked")
	waitSettingsUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog := w.win.VisibleDialog()
	dialog.Emit("response", "cancel")
	dialog.ForceClose()
	if !d.dirty || len(d.pages) != 3 || d.editor != "fixture" {
		t.Fatal("canceling discard lost the editor or its draft")
	}
	p.back.Emit("clicked")
	waitSettingsUI(t, func() bool { return w.win.VisibleDialog() != nil })
	dialog = w.win.VisibleDialog()
	dialog.Emit("response", "go")
	dialog.ForceClose()
	if d.dirty || d.editor != "" || len(d.pages) != 2 || d.navigation.VisiblePage().Title() != "Advanced Configuration" {
		t.Fatal("discard did not return to the immediate parent and close its editor")
	}
	d.edit("advanced/options", "Options")
	waitSettingsUI(t, func() bool { return d.editor != "" })
	d.setDirty(true)
	save := settingsSaveButton(d.commands)
	if save == nil {
		t.Fatal("editor has no Save button")
	}
	save.Emit("clicked")
	if !d.saving || d.pages[len(d.pages)-1].page.CanPop() {
		t.Fatal("an editor can be popped during Save")
	}
	waitSettingsUI(t, func() bool { return !d.saving })
	if len(d.pages) != 2 || d.navigation.VisiblePage().Title() != "Advanced Configuration" {
		t.Fatal("Save bypassed the editor's parent")
	}
	d.backSettings()
	if len(d.pages) != 1 || d.body != rootBody {
		t.Fatal("nested navigation did not return to the original list")
	}
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

func waitSettingsUI(t *testing.T, ready func() bool) {
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

func nativeSettingsBack(widget gtk.Widgetter) *gtk.Button {
	w := gtk.BaseWidget(widget)
	if b, ok := w.Cast().(*gtk.Button); ok && b.Visible() && b.IconName() == "go-previous-symbolic" {
		return b
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if button := nativeSettingsBack(child); button != nil {
			return button
		}
	}
	return nil
}

func settingsSaveButton(widget gtk.Widgetter) *gtk.Button {
	w := gtk.BaseWidget(widget)
	if b, ok := w.Cast().(*gtk.Button); ok && b.Label() == "Save" {
		return b
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if button := settingsSaveButton(child); button != nil {
			return button
		}
	}
	return nil
}

func settingsTestSwitch(widget gtk.Widgetter) *adw.SwitchRow {
	w := gtk.BaseWidget(widget)
	if r, ok := w.Cast().(*adw.SwitchRow); ok && r.Title() == "Test setting" {
		return r
	}
	for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if r := settingsTestSwitch(child); r != nil {
			return r
		}
	}
	return nil
}
