// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func defaultEndpoint(ctx context.Context) (string, error) {
	if p := os.Getenv("SYNCTRAIN_MCP_SOCKET"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Synctrain", "MCP", "control.sock"), nil
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	mode, _ := os.ReadFile(filepath.Join(config, "synctrain", "mode"))
	if strings.TrimSpace(string(mode)) == "files" {
		if p := os.Getenv("SYNCTRAIN_FILES_SOCKET"); p != "" {
			return p, nil
		}
		if p := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(p) {
			return filepath.Join(p, "synctrain", "files.sock"), nil
		}
		state := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(state) {
			state = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(state, "synctrain", "files", "files.sock"), nil
	}
	if p := os.Getenv("SYNCTRAIN_GATEWAY_ADMIN"); p != "" {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 'home' describes the existing setup; it never starts an engine.
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	command := filepath.Join(filepath.Dir(executable), "synctrain-gateway")
	b, err := exec.CommandContext(ctx, command, "home", "--json").Output()
	if err != nil {
		return "", err
	}
	var info struct{ Socket string }
	if err = json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	return info.Socket, nil
}
