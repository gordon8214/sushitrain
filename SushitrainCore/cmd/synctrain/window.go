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
	"unicode"
	"unicode/utf8"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// mountName is where Files mode shows folders, as text that doesn't wrap after the slash (a word joiner follows it)
const mountName = "~/\u2060Synctrain"

// mainWindow shows the page of the machine's mode, or the choice of one
type mainWindow struct {
	app    *adw.Application
	win    *adw.ApplicationWindow
	toasts *adw.ToastOverlay
	// Stops the current page's refreshing
	stop       func()
	switchMode *gio.SimpleAction
	// Set while the machine's mode is set up or changed, which nothing else may start meanwhile
	changing bool
	// Set once the window is closed; a mode change still running then reports with a notification
	closed bool
}

func newMainWindow(app *adw.Application) *mainWindow {
	w := &mainWindow{app: app, win: adw.NewApplicationWindow(&app.Application), toasts: adw.NewToastOverlay()}
	w.win.SetTitle("Synctrain")
	w.win.SetDefaultSize(760, 680)
	w.win.SetContent(w.toasts)
	w.win.ConnectCloseRequest(func() bool {
		if w.stop != nil {
			w.stop()
			w.stop = nil
		}
		w.closed = true
		return false
	})
	w.switchMode = gio.NewSimpleAction("switch-mode", nil)
	w.switchMode.ConnectActivate(func(*glib.Variant) {
		switch readMode() {
		case modeFiles:
			w.askSynchronize(true)
		case modeSynchronize:
			w.leaveSynchronize()
		}
	})
	w.win.AddAction(w.switchMode)
	about := gio.NewSimpleAction("about", nil)
	about.ConnectActivate(func(*glib.Variant) { w.about() })
	w.win.AddAction(about)
	w.show()
	return w
}

// menuButton is the header bar's main menu: switching to the other mode, and About
func (w *mainWindow) menuButton(switchLabel string) *gtk.MenuButton {
	menu := gio.NewMenu()
	menu.Append(switchLabel, "win.switch-mode")
	menu.Append("About Synctrain", "win.about")
	b := gtk.NewMenuButton()
	b.SetIconName("open-menu-symbolic")
	b.SetTooltipText("Main Menu")
	b.SetMenuModel(menu)
	return b
}

func (w *mainWindow) about() {
	d := adw.NewAboutDialog()
	d.SetApplicationName("Synctrain")
	d.SetApplicationIcon(appID)
	if version != "" {
		d.SetVersion(version)
	}
	d.SetComments("Syncthing on this machine, or the folders of a gateway on another system, fetched as they are " +
		"opened")
	d.SetWebsite("https://github.com/gordon8214/sushitrain")
	d.SetLicenseType(gtk.LicenseMPL20)
	d.Present(w.win)
}

// sentence is an error as a sentence of its own
func sentence(err error) string {
	msg := err.Error()
	if msg == "" {
		return msg
	}
	r, size := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[size:]
}

// inform shows something to read and acknowledge, which a toast would cut short
func (w *mainWindow) inform(heading, body string) {
	d := adw.NewAlertDialog(heading, body)
	d.AddResponse("ok", "OK")
	d.SetDefaultResponse("ok")
	d.SetCloseResponse("ok")
	d.Present(w.win)
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
	// Names in messages come from gateways, clients and other devices: never markup
	t.SetUseMarkup(false)
	t.SetTimeout(5)
	w.toasts.AddToast(t)
}

// report tells the outcome of a mode change: in a dialog, or in a notification once the window is closed
func (w *mainWindow) report(heading, body string) {
	if !w.closed {
		w.inform(heading, body)
		return
	}
	n := gio.NewNotification(heading)
	n.SetBody(body)
	w.app.SendNotification("mode", n)
}

// beginChange shows that the machine's mode is being set up or changed, until endChange. The page is replaced, so
// nothing on it can start another change or a daemon meanwhile, and closing the window doesn't end the program before
// the change is done. It reports false when a change runs already.
func (w *mainWindow) beginChange(what string) bool {
	if w.changing {
		return false
	}
	w.changing = true
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.switchMode.SetEnabled(false)
	w.app.Hold()
	page := adw.NewStatusPage()
	page.SetTitle(what)
	page.SetDescription("This can take a few minutes. It goes on if the window is closed.")
	spinner := adw.NewSpinner()
	spinner.SetSizeRequest(48, 48)
	spinner.SetHAlign(gtk.AlignCenter)
	page.SetChild(spinner)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	toolbar.SetContent(page)
	w.toasts.SetChild(toolbar)
	return true
}

func (w *mainWindow) endChange() {
	w.changing = false
	w.switchMode.SetEnabled(true)
	if !w.closed {
		w.show()
	}
	w.app.Release()
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
		"machine can be a gateway for phones and other machines.", func() { w.askSynchronize(false) })
	filesMode := choiceRow("Files", "Run no Syncthing: the folders of a gateway on another system appear in "+
		mountName+", and files are fetched as they are opened.", w.setUpFiles)
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

// setUpFiles starts Files mode: the daemon runs from now on, at every login. A failure leaves it off.
func (w *mainWindow) setUpFiles() {
	if !w.beginChange("Starting Synctrain Files") {
		return
	}
	background(func() (struct{}, error) {
		err := systemctl("enable", "--now", "synctrain-files.service")
		if err == nil {
			err = waitForService("synctrain-files.service", filesSocket(), time.Minute)
		}
		if err == nil {
			err = writeMode(modeFiles)
		}
		if err != nil {
			stopUnit("synctrain-files.service")
		}
		return struct{}{}, err
	}, func(_ struct{}, err error) {
		if err != nil {
			w.report("Synctrain Files Couldn't Start", sentence(err))
		}
		w.endChange()
	})
}

// editing reports whether the person is typing in a widget, which a refresh mustn't overwrite
func editing(widget gtk.Widgetter) bool {
	return gtk.BaseWidget(widget).StateFlags()&gtk.StateFlagFocusWithin != 0
}

// debouncer runs only the last of calls made in quick succession, as when stepping through numbers
type debouncer struct {
	source coreglib.SourceHandle
}

func (d *debouncer) call(fn func()) {
	if d.source != 0 {
		coreglib.SourceRemove(d.source)
	}
	d.source = coreglib.TimeoutAdd(800, func() bool {
		d.source = 0
		fn()
		return false
	})
}

// row is a list row; its text is never markup, as names in it come from gateways, clients and other devices
func row(title, subtitle string) *adw.ActionRow {
	r := adw.NewActionRow()
	r.SetUseMarkup(false)
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
