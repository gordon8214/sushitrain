// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package desktopui

import (
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SearchEntry contains navigation metadata only, never configuration values or drafts.
type SearchEntry struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Breadcrumb string   `json:"breadcrumb"`
	Category   string   `json:"category"`
	Action     string   `json:"action"`
	Section    string   `json:"section,omitempty"`
	Field      string   `json:"field,omitempty"`
	Anchor     string   `json:"anchor,omitempty"`
	Capability string   `json:"capability"`
	Platforms  []string `json:"platforms,omitempty"`
	Keywords   []string `json:"keywords,omitempty"`
}

func SearchText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFD.String(text))
}

// SearchScore is shared by the GTK results and catalog regression fixtures.
func SearchScore(entry SearchEntry, query string) int {
	query = strings.TrimSpace(SearchText(query))
	if query == "" {
		return -1
	}
	title := SearchText(entry.Title)
	corpus := title + " " + SearchText(entry.Breadcrumb+" "+entry.Field+" "+strings.Join(entry.Keywords, " "))
	for _, word := range strings.Fields(query) {
		if !strings.Contains(corpus, word) {
			return -1
		}
	}
	if title == query {
		return 0
	}
	if strings.HasPrefix(title, query) {
		return 1
	}
	if strings.Contains(title, query) {
		return 2
	}
	return 3
}

func Search(entries []SearchEntry, query string) []SearchEntry {
	type match struct {
		entry SearchEntry
		score int
	}
	matches := []match{}
	for _, entry := range entries {
		if score := SearchScore(entry, query); score >= 0 {
			matches = append(matches, match{entry, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.score != b.score {
			return a.score < b.score
		}
		return a.entry.ID < b.entry.ID
	})
	out := make([]SearchEntry, len(matches))
	for i, m := range matches {
		out[i] = m.entry
	}
	return out
}

func SearchEntries(platform string) []SearchEntry {
	c := Read()
	entries := []SearchEntry{}
	if platform != "ios" {
		for _, category := range c.Settings {
			for _, section := range category.Sections {
				entries = append(entries, SearchEntry{ID: category.ID + "." + section.ID, Title: section.Title,
					Breadcrumb: category.Title, Category: category.ID, Action: section.Action, Capability: section.Capability})
			}
		}
	}
	for _, entry := range c.Search {
		if entry.Section != "" {
			continue
		}
		if len(entry.Platforms) == 0 || strings.Contains(" "+strings.Join(entry.Platforms, " ")+" ", " "+platform+" ") {
			entries = append(entries, entry)
		}
	}
	return entries
}
