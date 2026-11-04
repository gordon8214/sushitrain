//go:build !darwin

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import "errors"

func historyMetadataHash(_ string) (string, error) { return "", nil }
func copyHistoryMetadata(_, _ string) error        { return errors.New("complete Mac restores require macOS") }

func historyRecoveryMetadataHash(path string, _ int) (string, error) {
	return historyMetadataHash(path)
}
