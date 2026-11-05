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

// Each settings destination owns a page and header bar. The settings list stays
// in the navigation stack, preserving its scroll position and keyboard focus.
type settingsPage struct {
	page     *adw.NavigationPage
	detail   *adw.ToolbarView
	banner   *adw.Banner
	back     *gtk.Button
	body     *gtk.Box
	commands *gtk.FlowBox
	show     func()
}

func (d *desktopUI) newSettingsPage(title string, show func()) *settingsPage {
	p := &settingsPage{detail: adw.NewToolbarView(), banner: adw.NewBanner(""), show: show}
	p.page = adw.NewNavigationPage(p.detail, title)
	header := adw.NewHeaderBar()
	// HeaderBar derives its title, accessible navigation labels, and back
	// tooltips from NavigationPage.
	p.back = gtk.NewButtonFromIconName("go-previous-symbolic")
	p.back.SetTooltipText("Back")
	p.back.UpdateProperty([]gtk.AccessibleProperty{gtk.AccessiblePropertyLabel}, []coreglib.Value{*coreglib.NewValue("Back")})
	p.back.ConnectClicked(d.backSettings)
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
	keys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		// Clean pages use libadwaita's native shortcuts and gestures. A dirty or
		// busy page cannot be popped by them; route keyboard Back through guard.
		if len(d.pages) < 2 || !d.dirty && !d.saving {
			return false
		}
		backKey := uint(gdk.KEY_Left)
		if d.navigation.Direction() == gtk.TextDirRTL {
			backKey = uint(gdk.KEY_Right)
		}
		modifiers := state & (gdk.ShiftMask | gdk.ControlMask | gdk.AltMask | gdk.SuperMask | gdk.HyperMask | gdk.MetaMask)
		if keyval == uint(gdk.KEY_Escape) && modifiers == 0 || keyval == backKey && modifiers == gdk.AltMask {
			d.backSettings()
			return true
		}
		return false
	})
	p.detail.AddController(keys)
	d.restoreSettingsPage(p)
	return p
}

func (d *desktopUI) restoreSettingsPage(p *settingsPage) {
	d.detail, d.banner = p.detail, p.banner
	d.body, d.commands = p.body, p.commands
}

func (d *desktopUI) pushSettingsPage(show func()) {
	parent := d.pages[len(d.pages)-1]
	parent.body, parent.commands = d.body, d.commands
	d.startView()
	p := d.newSettingsPage("Settings", show)
	d.pages = append(d.pages, p)
	d.reset("Settings")
	d.text("Loading…")
	d.navigation.Push(p.page)
	show()
	d.updateSettingsNavigation()
}

func (d *desktopUI) settingsPopped() {
	if len(d.pages) < 2 {
		return
	}
	d.startView()
	last := len(d.pages) - 1
	d.pages[last] = nil
	d.pages = d.pages[:last]
	p := d.pages[len(d.pages)-1]
	d.restoreSettingsPage(p)
	// Nested categories may have had in-flight requests canceled on entry to
	// the child. Refresh them with a new generation; retain the root list as is.
	if p.show != nil {
		p.show()
	}
	d.updateSettingsNavigation()
}

func (d *desktopUI) backSettings() {
	if len(d.pages) > 1 {
		d.guard(func() { d.navigation.Pop() })
	}
}

// Save and Cancel return to the immediate parent, including advanced sections.
func (d *desktopUI) finishSettings() {
	if len(d.pages) > 1 {
		d.backSettings()
	} else {
		d.showRoute()
	}
}

func (d *desktopUI) refreshView() {
	if len(d.pages) > 1 {
		show := d.pages[len(d.pages)-1].show
		d.startView()
		show()
		d.updateSettingsNavigation()
	} else {
		d.showRoute()
	}
}

func (d *desktopUI) setDirty(dirty bool) {
	d.dirty = dirty
	d.updateSettingsNavigation()
}

func (d *desktopUI) updateSettingsNavigation() {
	if len(d.pages) == 0 {
		return
	}
	p := d.pages[len(d.pages)-1]
	blocked := d.dirty || d.saving
	p.page.SetCanPop(!blocked)
	// can-pop also hides the automatic button. Keep a guarded Back control in
	// the same header position while confirmation is required.
	p.back.SetVisible(len(d.pages) > 1 && blocked)
}
