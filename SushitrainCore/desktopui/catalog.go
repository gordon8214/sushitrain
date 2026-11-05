// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
// Package desktopui defines the navigation contract shared by the native desktops.
package desktopui

import (
	_ "embed"
	"encoding/json"
)

//go:embed catalog.json
var data []byte

type Section struct {
	ID, Title, LabelKey, Capability, Action string
}
type Category struct {
	ID, Title, LabelKey, Symbol, Icon string
	Sections                          []Section
}
type Destination struct {
	ID, Title, LabelKey, Symbol, Icon string
	Sections                          []string
}
type Catalog struct {
	Version           int
	DetailDescriptors map[string][]Section
	Destinations      []Destination
	Settings          []Category
	Details           map[string][]string
	Aliases           map[string]string
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
