// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
// Package desktopui defines the navigation contract shared by the native desktops.
package desktopui

import (
	_ "embed"
	"encoding/json"
	"slices"
)

//go:embed catalog.json
var data []byte

type Section struct {
	ID, Title, LabelKey, Capability, Action string
	Platforms                               []string
}

func (s Section) Available(platform, mode string) bool {
	return Supports(s.Platforms, platform) && Available(s.Capability, mode)
}

type Category struct {
	ID, Title, LabelKey, Symbol, Icon string
	Sections                          []Section
}
type Destination struct {
	ID, Title, LabelKey, Symbol, Icon string
	Sections                          []string
	Capability                        string
	Platforms                         []string
}
type NavigationGroup struct {
	ID, Title, LabelKey string
	Items               []string
}
type NavigationItem struct {
	ID, Title, Symbol, Icon, Route string
}
type NavigationSection struct {
	ID, Title string
	Items     []NavigationItem
}
type Catalog struct {
	Version           int
	DetailDescriptors map[string][]Section
	Destinations      []Destination
	Settings          []Category
	Details           map[string][]string
	Aliases           map[string]string
	Search            []SearchEntry
	NavigationGroups  []NavigationGroup
}

func Supports(platforms []string, platform string) bool {
	return len(platforms) == 0 || slices.Contains(platforms, platform)
}

// Navigation is independent of the current page and engine's running state.
func (c Catalog) Navigation(platform, mode string) []NavigationSection {
	var groups []NavigationSection
	for _, group := range c.NavigationGroups {
		section := NavigationSection{ID: group.ID, Title: group.Title}
		for _, id := range group.Items {
			for _, d := range c.Destinations {
				if d.ID == id && Supports(d.Platforms, platform) && Available(d.Capability, mode) {
					section.Items = append(section.Items, NavigationItem{d.ID, d.Title, d.Symbol, d.Icon, d.ID})
				}
			}
			for _, category := range c.Settings {
				if category.ID == id && slices.ContainsFunc(category.Sections, func(s Section) bool { return s.Available(platform, mode) }) {
					section.Items = append(section.Items, NavigationItem{category.ID, category.Title, category.Symbol, category.Icon, "settings:" + category.ID})
				}
			}
		}
		if len(section.Items) > 0 {
			groups = append(groups, section)
		}
	}
	return groups
}

// NativeRoute keeps existing Linux page identities while display labels evolve.
func (n NavigationItem) NativeRoute() string {
	if n.ID == "files" {
		return "Files"
	}
	if n.Route != n.ID {
		return n.Route
	}
	return n.Title
}

// Read returns an independent copy, so adapters cannot change another window's catalog.
func Read() Catalog {
	var value Catalog
	if err := json.Unmarshal(data, &value); err != nil {
		panic(err)
	}
	return value
}

func MainTitles() []string {
	var titles []string
	for _, item := range Read().Destinations {
		titles = append(titles, item.Title)
	}
	return titles
}

func Available(capability, mode string) bool {
	return capability == "always" || capability == mode
}
