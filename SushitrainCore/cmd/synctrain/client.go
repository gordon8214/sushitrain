// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"t-shaped.nl/sushitrain/v2/adminsock"
)

// background runs fn away from GTK's main loop, then done on it with fn's result
func background[T any](fn func() (T, error), done func(T, error)) {
	go func() {
		result, err := fn()
		coreglib.IdleAdd(func() {
			done(result, err)
		})
	}()
}

func runtimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return dir
	}
	return os.TempDir()
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// filesSocket is synctrain-files's admin socket (as its default)
func filesSocket() string {
	if env := os.Getenv("SYNCTRAIN_FILES_SOCKET"); env != "" {
		return env
	}
	return filepath.Join(runtimeDir(), "synctrain", "files.sock")
}

// callFiles calls a method of synctrain-files
func callFiles(method string, params, result any) error {
	return adminsock.Call(filesSocket(), method, params, result)
}

// running reports whether something serves the admin socket
func running(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Modes, in ~/.config/synctrain/mode: one per machine
const (
	modeSynchronize = "synchronize"
	modeFiles       = "files"
)

func modePath() string {
	return filepath.Join(configDir(), "synctrain", "mode")
}

// readMode is the machine's mode, or "" before it was set up
func readMode() string {
	data, err := os.ReadFile(modePath())
	if err != nil {
		return ""
	}
	switch mode := strings.TrimSpace(string(data)); mode {
	case modeSynchronize, modeFiles:
		return mode
	}
	return ""
}

// writeMode records the machine's mode: the last step of setting it up, or of switching
func writeMode(mode string) error {
	if err := os.MkdirAll(filepath.Dir(modePath()), 0o755); err != nil {
		return err
	}
	tmp := modePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(mode+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, modePath())
}

// systemctl runs systemctl for the user's services
func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// waitForSocket waits until a daemon serves its socket
func waitForSocket(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !running(socket) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the service didn't start (see journalctl --user -e)")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

// humanBytes is a size as GNOME shows sizes
func humanBytes(n int64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(n)/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d bytes", n)
}
