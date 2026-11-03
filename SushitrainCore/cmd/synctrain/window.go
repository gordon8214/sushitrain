// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"context"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// mainWindow shows the page of the machine's mode, or the choice of one
type mainWindow struct {
	app    *adw.Application
	win    *adw.ApplicationWindow
	toasts *adw.ToastOverlay
	// Stops the current page's refreshing
	stop func()
}

func newMainWindow(app *adw.Application) *mainWindow {
	w := &mainWindow{app: app, win: adw.NewApplicationWindow(&app.Application), toasts: adw.NewToastOverlay()}
	w.win.SetTitle("Synctrain")
	w.win.SetDefaultSize(760, 680)
	w.win.SetContent(w.toasts)
	w.win.ConnectCloseRequest(func() bool {
		if w.stop != nil {
			w.stop()
		}
		return false
	})
	w.show()
	return w
}

// show shows the page of the machine's mode
func (w *mainWindow) show() {
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	switch readMode() {
	case modeFiles:
		page, stop := newFilesView(w)
		w.toasts.SetChild(page)
		w.stop = stop
	case modeSynchronize:
		page, stop := newSyncView(w)
		w.toasts.SetChild(page)
		w.stop = stop
	default:
		w.toasts.SetChild(w.chooser())
	}
}

func (w *mainWindow) parent() *gtk.Window {
	return &w.win.ApplicationWindow.Window
}

func (w *mainWindow) toast(message string) {
	t := adw.NewToast(message)
	t.SetTimeout(5)
	w.toasts.AddToast(t)
}

func (w *mainWindow) failed(err error) {
	if err != nil {
		w.toast(err.Error())
	}
}

// confirm asks before something that can't be undone, then does it
func (w *mainWindow) confirm(heading, body, action string, destructive bool, then func()) {
	d := adw.NewAlertDialog(heading, body)
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("go", action)
	if destructive {
		d.SetResponseAppearance("go", adw.ResponseDestructive)
	} else {
		d.SetResponseAppearance("go", adw.ResponseSuggested)
	}
	d.SetDefaultResponse("cancel")
	d.SetCloseResponse("cancel")
	d.ConnectResponse(func(response string) {
		if response == "go" {
			then()
		}
	})
	d.Present(w.win)
}

// openPath opens a file or directory in the program the desktop opens it with
func (w *mainWindow) openPath(path string) {
	gtk.NewFileLauncher(gio.NewFileForPath(path)).Launch(context.Background(), w.parent(), nil)
}

// chooser lets the person choose the machine's mode, the first time
func (w *mainWindow) chooser() gtk.Widgetter {
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	page := adw.NewStatusPage()
	page.SetIconName("folder-remote-symbolic")
	page.SetTitle("Welcome to Synctrain")
	page.SetDescription("Choose what this machine does. You can switch later.")
	group := adw.NewPreferencesGroup()
	synchronize := choiceRow("Synchronize", "Run Syncthing here: every folder syncs all of its files, and this "+
		"machine can be a gateway for phones and other machines.", w.setUpSynchronize)
	filesMode := choiceRow("Files", "Run no Syncthing: the folders of a gateway on another system appear in "+
		"~/Synctrain, and files are fetched as they are opened.", w.setUpFiles)
	group.Add(synchronize)
	group.Add(filesMode)
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(560)
	clamp.SetChild(group)
	page.SetChild(clamp)
	toolbar.SetContent(page)
	return toolbar
}

func choiceRow(title, subtitle string, activate func()) *adw.ActionRow {
	r := row(title, subtitle)
	r.SetActivatable(true)
	r.AddSuffix(gtk.NewImageFromIconName("go-next-symbolic"))
	r.ConnectActivated(activate)
	return r
}

// setUpFiles starts Files mode: the daemon runs from now on, at every login
func (w *mainWindow) setUpFiles() {
	w.toast("Starting Synctrain Files…")
	background(func() (struct{}, error) {
		if err := systemctl("enable", "--now", "synctrain-files.service"); err != nil {
			return struct{}{}, err
		}
		if err := waitForSocket(filesSocket(), 20*time.Second); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, writeMode(modeFiles)
	}, func(_ struct{}, err error) {
		if err != nil {
			w.toast("Synctrain Files couldn't start: " + err.Error())
			return
		}
		w.show()
	})
}

func row(title, subtitle string) *adw.ActionRow {
	r := adw.NewActionRow()
	r.SetTitle(title)
	if subtitle != "" {
		r.SetSubtitle(subtitle)
	}
	return r
}

func iconButton(icon, tooltip string, clicked func()) *gtk.Button {
	b := gtk.NewButtonFromIconName(icon)
	b.SetTooltipText(tooltip)
	b.SetVAlign(gtk.AlignCenter)
	b.AddCSSClass("flat")
	b.ConnectClicked(clicked)
	return b
}

func textButton(label string, clicked func()) *gtk.Button {
	b := gtk.NewButtonWithLabel(label)
	b.SetVAlign(gtk.AlignCenter)
	b.ConnectClicked(clicked)
	return b
}

// listGroup is a preferences group whose rows are made again when what they show changes
type listGroup struct {
	group *adw.PreferencesGroup
	rows  []gtk.Widgetter
	key   string
}

func newListGroup(title, description string) *listGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	g.SetDescription(description)
	return &listGroup{group: g}
}

// set shows rows, unless key says they show what is shown already
func (l *listGroup) set(key string, rows func() []gtk.Widgetter) {
	if key == l.key && l.rows != nil {
		return
	}
	for _, r := range l.rows {
		l.group.Remove(r)
	}
	l.key, l.rows = key, rows()
	if l.rows == nil {
		l.rows = []gtk.Widgetter{}
	}
	for _, r := range l.rows {
		l.group.Add(r)
	}
}
