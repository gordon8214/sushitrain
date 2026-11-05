// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// Each destination owns a page and header bar. Parent views stay
// in the navigation stack, preserving their scroll position and keyboard focus.
type navigationPage struct {
	page     *adw.NavigationPage
	detail   *adw.ToolbarView
	banner   *adw.Banner
	back     *gtk.Button
	body     *gtk.Box
	commands *gtk.FlowBox
	show     func()
	resume   func()
	focus    gtk.Widgetter
	pending  int
	reload   bool
}

func (d *desktopUI) newNavigationPage(title string, show func()) *navigationPage {
	p := &navigationPage{detail: adw.NewToolbarView(), banner: adw.NewBanner(""), show: show}
	p.page = adw.NewNavigationPage(p.detail, title)
	p.page.ConnectShown(func() {
		if len(d.pages) > 0 && d.pages[len(d.pages)-1] == p {
			d.focusPage()
		}
	})
	header := adw.NewHeaderBar()
	// HeaderBar derives its title, accessible navigation labels, and back
	// tooltips from NavigationPage.
	p.back = gtk.NewButtonFromIconName("go-previous-symbolic")
	p.back.SetTooltipText("Back")
	p.back.UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel}, []coreglib.Value{*coreglib.NewValue("Back")})
	p.back.ConnectClicked(d.goBack)
	p.back.SetVisible(false)
	header.PackStart(p.back)
	switchLabel := "Switch to Files…"
	if d.mode == modeFiles {
		switchLabel = "Switch to Synchronize…"
	}
	header.PackEnd(d.w.menuButton(switchLabel))
	p.detail.AddTopBar(header)
	p.detail.AddTopBar(p.banner)
	keys := gtk.NewEventControllerKey()
	keys.SetPropagationPhase(gtk.PhaseCapture)
	keys.ConnectKeyPressed(d.handleBackKey)
	p.detail.AddController(keys)
	mouse := gtk.NewGestureClick()
	mouse.SetButton(8)
	mouse.SetPropagationPhase(gtk.PhaseCapture)
	mouse.ConnectPressed(func(_ int, _, _ float64) {
		if (d.dirty || d.saving) && d.w.win.VisibleDialog() == nil {
			mouse.SetState(gtk.EventSequenceClaimed)
			d.goBack()
		}
	})
	p.detail.AddController(mouse)
	d.restoreNavigationPage(p)
	return p
}

func (d *desktopUI) restoreNavigationPage(p *navigationPage) {
	d.detail, d.banner = p.detail, p.banner
	d.body, d.commands = p.body, p.commands
}

func (d *desktopUI) pushPage(show func()) {
	parent := d.pages[len(d.pages)-1]
	parent.body, parent.commands = d.body, d.commands
	parent.focus = d.w.win.Focus()
	parent.reload = parent.reload || parent.pending > 0
	parent.resume = d.browserState()
	d.startView()
	p := d.newNavigationPage("Loading…", show)
	d.pages = append(d.pages, p)
	d.reset("Loading…")
	d.text("Loading…")
	d.navigation.Push(p.page)
	show()
	d.updateNavigation()
}

func (d *desktopUI) pagePopped() {
	if len(d.pages) < 2 {
		return
	}
	d.startView()
	last := len(d.pages) - 1
	d.pages[last] = nil
	d.pages = d.pages[:last]
	p := d.pages[len(d.pages)-1]
	d.restoreNavigationPage(p)
	if p.resume != nil {
		p.resume()
	}
	// Retain the original widgets, scroll position, filters, and selection.
	// Reload only if entering the child interrupted a request or changed data.
	if p.reload && p.show != nil {
		p.reload = false
		p.show()
	} else if p.focus != nil {
		d.w.win.SetFocus(p.focus)
	}
	d.updateNavigation()
}

func (d *desktopUI) goBack() {
	if len(d.pages) > 1 {
		d.guard(func() { d.navigation.Pop() })
	}
}

// Completed edits return to the immediate parent, including nested sections.
func (d *desktopUI) finishPage() {
	d.invalidateParents()
	if len(d.pages) > 1 {
		d.goBack()
	} else {
		d.showRoute()
	}
}

func (d *desktopUI) refreshView() {
	if len(d.pages) == 0 {
		return
	}
	p := d.pages[len(d.pages)-1]
	d.startView()
	p.reload = false
	p.show()
	d.updateNavigation()
}

func (d *desktopUI) setDirty(dirty bool) {
	d.dirty = dirty
	d.updateNavigation()
}

func (d *desktopUI) updateNavigation() {
	if len(d.pages) == 0 {
		return
	}
	p := d.pages[len(d.pages)-1]
	blocked := d.dirty || d.saving
	p.page.SetCanPop(!blocked)
	if d.root != nil && d.root.Content() != nil {
		d.root.Content().SetCanPop(!blocked)
	}
	if len(d.pages) > 1 {
		parent := d.pages[len(d.pages)-2].page.Title()
		p.back.SetTooltipText("Back to " + parent)
		p.back.UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel}, []coreglib.Value{*coreglib.NewValue("Back to " + parent)})
	}
	// can-pop also hides the automatic button. Keep a guarded Back control in
	// the same header position while confirmation is required.
	p.back.SetVisible(len(d.pages) > 1 && blocked)
}

// Track requests on their originating page, even after it is no longer visible.
func (d *desktopUI) trackPageLoad() func() {
	if len(d.pages) == 0 {
		return func() {}
	}
	p := d.pages[len(d.pages)-1]
	p.pending++
	return func() { p.pending-- }
}

// Browser navigation state belongs to the retained page just like its widgets.
func (d *desktopUI) browserState() func() {
	folder, directory, parent := d.folder, d.directory, d.parent
	rows, next, selected, search := d.browserRows, d.browserNext, d.browserSelected, d.browserSearch
	return func() {
		d.folder, d.directory, d.parent = folder, directory, parent
		d.browserRows, d.browserNext, d.browserSelected, d.browserSearch = rows, next, selected, search
	}
}

func (d *desktopUI) invalidateParents() {
	for i, p := range d.pages {
		if i < len(d.pages)-1 && (i != 0 || d.route != "Settings") {
			p.reload = true
		}
	}
}

func (d *desktopUI) handleBackKey(keyval, _ uint, state gdk.ModifierType) bool {
	// Clean pages use libadwaita's native shortcuts and gestures. Dirty pages
	// route keyboard Back through the same confirmation as the header arrow.
	if len(d.pages) < 2 || !d.dirty && !d.saving || d.w.win.VisibleDialog() != nil {
		return false
	}
	backKey := uint(gdk.KEY_Left)
	if d.navigation.Direction() == gtk.TextDirRTL {
		backKey = uint(gdk.KEY_Right)
	}
	modifiers := state & (gdk.ShiftMask | gdk.ControlMask | gdk.AltMask | gdk.SuperMask | gdk.HyperMask | gdk.MetaMask)
	if keyval == uint(gdk.KEY_Escape) && modifiers == 0 || keyval == backKey && modifiers == gdk.AltMask {
		d.goBack()
		return true
	}
	return false
}

// Replacing a loading placeholder can remove the focused widget. Keep focus
// inside the visible page so its native navigation shortcuts remain available.
func (d *desktopUI) focusPage() {
	focus := d.w.win.Focus()
	if focus != nil && gtk.BaseWidget(focus).IsAncestor(d.detail) {
		return
	}
	if d.body != nil && d.body.ChildFocus(gtk.DirTabForward) {
		return
	}
	d.detail.ChildFocus(gtk.DirTabForward)
}
