// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpcontrol

import "context"

type progressKey struct{}
type Progress struct {
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
	Phase     string `json:"phase"`
}

// ReportProgress updates host-owned work without requiring an MCP subscription.
func ReportProgress(ctx context.Context, completed, total int64, phase string) {
	if report, ok := ctx.Value(progressKey{}).(func(Progress)); ok {
		report(Progress{Completed: completed, Total: total, Phase: phase})
	}
}
