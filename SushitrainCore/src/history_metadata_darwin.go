//go:build darwin

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"encoding/json"
	"sort"
	"t-shaped.nl/sushitrain/v2/versionsnapshot"
)

func historyMetadataHash(path string) (string, error) {
	attrs, err := versionsnapshot.Attributes(path)
	if err != nil {
		return "", err
	}
	return versionsnapshot.AttributeDigest(attrs)
}
func copyHistoryMetadata(source, destination string) error {
	return versionsnapshot.CopyAttributes(source, destination)
}

// Format zero was written by early metadata-aware builds using JSON hashing.
func historyRecoveryMetadataHash(path string, format int) (string, error) {
	if format == 1 {
		return historyMetadataHash(path)
	}
	attrs, err := versionsnapshot.Attributes(path)
	if err != nil {
		return "", err
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Name < attrs[j].Name })
	data, err := json.Marshal(attrs)
	if err != nil {
		return "", err
	}
	return versionsnapshot.Digest(data), nil
}
