// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopui"
)

func (d *desktopUI) installSettingsSearch(header *adw.HeaderBar, column *gtk.Box, scroll *gtk.ScrolledWindow) {
	d.settingsSearchBar = gtk.NewSearchBar()
	d.settingsSearchEntry = gtk.NewSearchEntry()
	d.settingsSearchEntry.SetPlaceholderText("Search Settings")
	d.settingsSearchBar.SetChild(d.settingsSearchEntry)
	d.settingsSearchBar.ConnectEntry(d.settingsSearchEntry)
	d.settingsSearchBar.SetKeyCaptureWidget(column)
	d.settingsSearchButton = gtk.NewToggleButton()
	d.settingsSearchButton.SetIconName("edit-find-symbolic")
	d.settingsSearchButton.SetTooltipText("Search Settings (Ctrl+F)")
	header.PackStart(d.settingsSearchButton)
	d.settingsSearchButton.ConnectToggled(func() {
		d.settingsSearchBar.SetSearchMode(d.settingsSearchButton.Active())
		if d.settingsSearchButton.Active() {
			d.settingsSearchEntry.GrabFocus()
			d.loadSettingsSearch()
		}
	})
	d.settingsSearchBar.NotifyProperty("search-mode-enabled", func() { d.settingsSearchButton.SetActive(d.settingsSearchBar.SearchMode()); d.showSettingsSearch() })
	column.Append(d.settingsSearchBar)
	d.settingsSearchStack = gtk.NewStack()
	d.settingsSearchStack.AddNamed(scroll, "categories")
	d.settingsSearchResults = gtk.NewListBox()
	d.settingsSearchResults.AddCSSClass("navigation-sidebar")
	resultsScroll := gtk.NewScrolledWindow()
	resultsScroll.SetVExpand(true)
	resultsScroll.SetChild(d.settingsSearchResults)
	d.settingsSearchStack.AddNamed(resultsScroll, "results")
	d.settingsSearchStack.SetVExpand(true)
	column.Append(d.settingsSearchStack)
	d.settingsSearchEntry.ConnectSearchChanged(d.showSettingsSearch)
	d.settingsSearchEntry.ConnectActivate(func() {
		matches := desktopui.Search(append(desktopui.SearchEntries("gnome"), d.settingsSearchIndex...), d.settingsSearchEntry.Text())
		if len(matches) > 0 {
			d.guard(func() { d.openSettingsSearch(matches[0]) })
		}
	})
	d.settingsSearchEntry.ConnectStopSearch(func() { d.settingsSearchEntry.SetText(""); d.settingsSearchBar.SetSearchMode(false) })
	action := gio.NewSimpleAction("settings-search", nil)
	action.ConnectActivate(func(_ *glib.Variant) {
		if strings.HasPrefix(d.route, "settings:") {
			d.settingsSearchButton.SetActive(!d.settingsSearchButton.Active())
		}
	})
	d.w.win.AddAction(action)
	d.w.app.SetAccelsForAction("win.settings-search", []string{"<Control>f"})
}

func (d *desktopUI) showSettingsSearch() {
	if d.closed || d.settingsSearchStack == nil {
		return
	}
	query := d.settingsSearchEntry.Text()
	if !strings.HasPrefix(d.route, "settings:") || !d.settingsSearchBar.SearchMode() || strings.TrimSpace(query) == "" {
		d.settingsSearchStack.SetVisibleChildName("categories")
		return
	}
	for child := d.settingsSearchResults.FirstChild(); child != nil; child = d.settingsSearchResults.FirstChild() {
		d.settingsSearchResults.Remove(child)
	}
	entries := append(desktopui.SearchEntries("gnome"), d.settingsSearchIndex...)
	matches := desktopui.Search(entries, query)
	if d.settingsSearchError != "" {
		d.settingsSearchResults.Append(row("Some settings could not be loaded", d.settingsSearchError))
	}
	if len(matches) == 0 {
		d.settingsSearchResults.Append(row("No Results", "Try another word or phrase."))
	}
	for _, entry := range matches {
		e := entry
		r := choiceRow(e.Title, e.Breadcrumb, func() { d.guard(func() { d.openSettingsSearch(e) }) })
		d.settingsSearchResults.Append(r)
	}
	d.settingsSearchStack.SetVisibleChildName("results")
}

func (d *desktopUI) loadSettingsSearch() {
	if d.mode != modeSynchronize || d.settingsSearchLoading {
		return
	}
	d.settingsSearchLoading = true
	mode, generation := d.mode, d.settingsSearchGeneration
	type snapshot struct {
		endpoint string
		entries  []desktopui.SearchEntry
	}
	background(func() (snapshot, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		endpoint, err := d.requestEndpoint(mode)
		if err != nil {
			return snapshot{}, err
		}
		result := snapshot{endpoint: endpoint}
		err = adminsock.CallContext(ctx, endpoint, "settings.index", nil, &result.entries)
		return result, err
	}, func(result snapshot, err error) {
		if generation != d.settingsSearchGeneration {
			return
		}
		d.settingsSearchLoading = false
		if d.closed || mode != d.mode || result.endpoint != "" && result.endpoint != d.socket {
			return
		}
		entries := result.entries
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message == d.settingsSearchError && (err != nil || reflect.DeepEqual(entries, d.settingsSearchIndex)) {
			return
		}
		if err != nil {
			d.settingsSearchError = err.Error()
		} else {
			d.settingsSearchIndex = entries
			d.settingsSearchError = ""
		}
		d.showSettingsSearch()
	})
}

func (d *desktopUI) invalidateSettingsSearch() {
	d.settingsSearchGeneration++
	d.settingsSearchLoading = false
	d.settingsSearchIndex = nil
	if strings.HasPrefix(d.route, "settings:") {
		d.loadSettingsSearch()
	}
}

func (d *desktopUI) openSettingsSearch(entry desktopui.SearchEntry) {
	d.searchTargetAnchor = entry.Anchor
	if d.searchTargetAnchor == "" {
		d.searchTargetAnchor = entry.Action
	}
	d.searchTargetField = entry.Field
	// A result replaces the previous destination; retained editor widgets must
	// never outlive the save token closed by the departure guard.
	route := d.route
	if entry.Category != "" {
		route = "settings:" + entry.Category
	}
	d.navigate(route)
	if !desktopui.Available(entry.Capability, d.mode) {
		d.subpage(func() {
			d.reset(entry.Title)
			d.text("Available in " + entry.Capability + " mode. Change the operating mode in General.")
		})
		return
	}
	if entry.Section != "" {
		d.edit(entry.Action, entry.Title)
		return
	}
	if entry.Action == "mode" || entry.Action == "configuration" {
		d.navigate("settings:" + entry.Category)
		return
	}
	switch entry.Action {
	case "remoteService":
		d.subpage(func() {
			d.reset("Remote File Service")
			widget, stop := newSyncView(d.w)
			d.detail.SetContent(widget)
			d.stopPage = stop
		})
	case "gateways":
		d.subpage(func() {
			d.reset("Gateways")
			widget, stop := newFilesPage(d.w, "gateways")
			d.detail.SetContent(widget)
			d.stopPage = stop
		})
	case "historySources":
		d.subpage(func() { d.reset("History Sources"); d.historySources() })
	case "app":
		if entry.Anchor == "Tray" {
			d.subpage(func() { d.reset("Desktop Integration"); d.desktopSettings() })
		} else {
			d.previewSettings()
		}
	default:
		d.openCatalogSetting(entry.Action, entry.Title)
	}
}

func (d *desktopUI) revealSetting(anchor string, widget gtk.Widgetter) {
	if !d.matchesSettingsAnchor(anchor) {
		return
	}
	generation := d.generation
	coreglib.IdleAdd(func() {
		if d.closed || d.generation != generation {
			return
		}
		w := gtk.BaseWidget(widget)
		if !w.GrabFocus() {
			w.ChildFocus(gtk.DirTabForward)
		}
	})
}

func (d *desktopUI) matchesSettingsAnchor(anchor string) bool {
	return anchor != "" && (strings.EqualFold(d.searchTargetAnchor, anchor) || strings.EqualFold(d.searchTargetField, anchor))
}
