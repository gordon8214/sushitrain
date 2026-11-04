// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpcontrol

import (
	"encoding/json"
	"strings"
)

func copyObject(source map[string]any, except string) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		if key != except {
			out[key] = value
		}
	}
	return out
}

// Keep native result shapes and metadata while paginating their collection.
func collection(tool string, data any) ([]any, func([]any) any, int, bool) {
	field := ""
	switch tool {
	case "synctrain_versions_list":
		field = "versions"
	case "synctrain_folder_differences":
		field = "needed"
	case "synctrain_history_versions_list":
		return historyCollection(data)
	}
	if object, ok := data.(map[string]any); ok && field != "" {
		for key, value := range object {
			if !strings.EqualFold(key, field) {
				continue
			}
			if list, ok := value.([]any); ok {
				header := copyObject(object, key)
				encoded, _ := json.Marshal(header)
				return list, func(items []any) any {
					result := copyObject(header, "")
					result[key] = items
					return result
				}, len(encoded), true
			}
		}
	}
	list, ok := data.([]any)
	return list, nil, 0, ok
}

type historyRow struct {
	Group   int
	Header  map[string]any
	Field   string
	Version any
	Empty   bool
}

// A history source may itself contain more than a response's worth of versions.
// Count versions, then regroup just the current page under their source metadata.
func historyCollection(data any) ([]any, func([]any) any, int, bool) {
	sources, ok := data.([]any)
	if !ok {
		return nil, nil, 0, false
	}
	rows := []any{}
	for group, source := range sources {
		object, ok := source.(map[string]any)
		if !ok {
			return sources, nil, 0, true
		}
		field := ""
		var versions []any
		for key, value := range object {
			if strings.EqualFold(key, "versions") {
				field = key
				versions, _ = value.([]any)
			}
		}
		header := copyObject(object, field)
		if len(versions) == 0 {
			rows = append(rows, historyRow{Group: group, Header: header, Field: field, Empty: true})
		}
		for _, version := range versions {
			rows = append(rows, historyRow{Group: group, Header: header, Field: field, Version: version})
		}
	}
	return rows, func(items []any) any {
		result := []any{}
		lastGroup := -1
		var current map[string]any
		for _, item := range items {
			row := item.(historyRow)
			if row.Group != lastGroup {
				current = copyObject(row.Header, "")
				if row.Field != "" {
					current[row.Field] = []any{}
				}
				result = append(result, current)
				lastGroup = row.Group
			}
			if !row.Empty {
				current[row.Field] = append(current[row.Field].([]any), row.Version)
			}
		}
		return result
	}, 0, true
}
