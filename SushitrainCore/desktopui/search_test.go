// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package desktopui

import "testing"

func TestSettingsSearchMatching(t *testing.T) {
	entries := []SearchEntry{{ID: "a", Title: "Cache", Breadcrumb: "Files & Storage", Keywords: []string{"downloaded files"}},
		{ID: "b", Title: "Cache budget"}, {ID: "c", Title: "Clear cache"}, {ID: "d", Title: "Café", Keywords: []string{"cache"}}}
	got := Search(entries, "CACHE")
	if len(got) != 4 || got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatal(got)
	}
	for query, id := range map[string]string{"  downloaded  storage ": "a", "CAFE": "d"} {
		if got := Search(entries, query); len(got) != 1 || got[0].ID != id {
			t.Fatal(query, got)
		}
	}
	if len(Search(entries, "")) != 0 || len(Search(entries, "cache absent")) != 0 {
		t.Fatal("empty or unmatched query")
	}
}

func TestSettingsSearchCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range Read().Search {
		if seen[entry.ID] || entry.ID == "" || entry.Title == "" || entry.Action == "" || entry.Category == "" {
			t.Fatalf("invalid descriptor: %+v", entry)
		}
		seen[entry.ID] = true
	}
	for _, entry := range SearchEntries("ios") {
		if entry.Section != "" {
			t.Fatal("mobile advertises engine configuration")
		}
	}
	if matches := Search(SearchEntries("kde"), "public port"); len(matches) != 1 || matches[0].ID != "gateway.port" {
		t.Fatal(matches)
	}
}

func TestApplicableSearchResultsReachVisiblePages(t *testing.T) {
	c := Read()
	for _, platform := range []string{"macOS", "windows", "gnome", "kde", "ios"} {
		for _, mode := range []string{"files", "synchronize"} {
			if platform == "ios" && mode != "files" {
				continue
			}
			visible := map[string]bool{}
			for _, group := range c.Navigation(platform, mode) {
				for _, item := range group.Items {
					visible[item.ID] = true
				}
			}
			for _, entry := range SearchEntries(platform) {
				if Available(entry.Capability, mode) && !visible[entry.Category] {
					t.Errorf("%s/%s search %s targets hidden %s", platform, mode, entry.ID, entry.Category)
				}
			}
		}
	}
}
