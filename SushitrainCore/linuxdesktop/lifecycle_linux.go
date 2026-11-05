// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux

// Package linuxdesktop owns desktop lifecycle operations shared by GTK and KDE.
package linuxdesktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gatewayadmin"
	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

const (
	modeSynchronize = "synchronize"
	modeFiles       = "files"
)

func callFiles(method string, params, result any) error {
	return adminsock.Call(FilesSocket(), method, params, result)
}
func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

func FilesSocket() string {
	if env := os.Getenv("SYNCTRAIN_FILES_SOCKET"); env != "" {
		return env
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "synctrain", "files.sock")
	}
	state := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(state) {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "synctrain", "files", "files.sock")
}

func running(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func modePath() string {
	return filepath.Join(configDir(), "synctrain", "mode")
}

func ReadMode() string {
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

func WriteMode(mode string) error {
	if err := os.MkdirAll(filepath.Dir(modePath()), 0o755); err != nil {
		return err
	}
	tmp := modePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(mode+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, modePath())
}

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

func waitForService(unit, socket string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for !running(socket) {
		if systemctl("is-failed", "--quiet", unit) == nil {
			return fmt.Errorf("%s stopped (see journalctl --user -u %s)", unit, unit)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't start within %s (see journalctl --user -u %s)", unit, limit, unit)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func stopUnit(unit string) {
	_ = systemctl("disable", "--now", unit)
	_ = systemctl("reset-failed", unit)
}

func environmentDiffers() string {
	manager := map[string]string{}
	if out, err := exec.Command("systemctl", "--user", "show-environment").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if name, value, ok := strings.Cut(line, "="); ok {
				manager[name] = value
			}
		}
	}
	for _, name := range []string{"STHOMEDIR", "SYNCTRAIN_GATEWAY_ADMIN"} {
		if os.Getenv(name) != manager[name] {
			return name
		}
	}
	return ""
}

func Lingering() bool {
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger",
		"--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

func GatewayCommand(args ...string) ([]byte, error) {
	program := "synctrain-gateway"
	if self, err := os.Executable(); err == nil {
		if next := filepath.Join(filepath.Dir(self), program); fileExists(next) {
			program = next
		}
	}
	cmd := exec.Command(program, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "synctrain-gateway: ")
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func DescribeHome() (gatewayadmin.Home, error) {
	var h gatewayadmin.Home
	out, err := GatewayCommand("home", "--json")
	if err != nil {
		return h, err
	}
	return h, json.Unmarshal(out, &h)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
	}
	return err
}

func Inspect() (Check, error) {
	var c Check
	h, err := DescribeHome()
	if err != nil {
		return c, err
	}
	c.Home = h
	switch {
	case h.Problem != "":
		c.Refusal = "The Syncthing in " + h.Dir + " can't be taken over: " + h.Problem + "."
	case environmentDiffers() != "":
		c.Refusal = environmentDiffers() + " is set here but not in your user's service manager, where Synctrain " +
			"runs: set it there too (systemctl --user set-environment), or not at all."
	case systemSyncthing():
		c.Refusal = "Syncthing runs as a system service (syncthing@" + userName() + "): have an administrator " +
			"disable it first."
	case !h.Exists && systemctl("is-active", "--quiet", "syncthing.service") == nil:
		c.Refusal = "syncthing.service runs with a home Synctrain can't find (" + h.Dir + " has no configuration). " +
			"Stop it, or start Synctrain with that home as STHOMEDIR in your user's service manager."
	}
	c.Tray = trayStartsSyncthing()
	return c, nil
}

func systemSyncthing() bool {
	out, _ := exec.Command("systemctl", "is-enabled", "syncthing@"+userName()+".service").Output()
	return strings.TrimSpace(string(out)) == "enabled"
}

func userName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return strconv.Itoa(os.Getuid())
}

func trayStartsSyncthing() bool {
	data, err := os.ReadFile(filepath.Join(configDir(), "syncthingtray.ini"))
	if err != nil {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section == "[startup]" && strings.ReplaceAll(line, " ", "") == "syncthingAutostart=true" {
			return true
		}
	}
	return false
}

type Check struct {
	Home gatewayadmin.Home
	// Why it can't be set up, when it can't
	Refusal string
	Tray    bool
}

type Result struct {
	Backup   string
	Removals []files.Removal
	// Set once the gateway may have opened Syncthing's database, which the stock Syncthing then mustn't be
	// restarted on: the engine's may be newer
	GatewayStarted bool
}

func (r Result) Notes() string {
	var notes []string
	if r.Backup != "" {
		notes = append(notes, "The previous configuration and database are in "+r.Backup+".")
	}
	for _, removal := range r.Removals {
		if removal.Moved > 0 {
			notes = append(notes, "Files that were only on this machine are in "+removal.Leftovers+".")
		}
	}
	return strings.Join(notes, "\n\n")
}

func Synchronize(h gatewayadmin.Home, fromFiles bool) (Result, error) {
	unlock, err := transitionLock(context.Background())
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	return synchronize(h, fromFiles)
}
func synchronize(h gatewayadmin.Home, fromFiles bool) (Result, error) {
	var r Result
	var undos []func()
	undo := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}
	switch {
	case h.Exists && !h.Taken:
		wasActive := systemctl("is-active", "--quiet", "syncthing.service") == nil
		if wasActive {
			if err := systemctl("stop", "syncthing.service"); err != nil {
				return r, err
			}
		}
		if err := systemctl("mask", "syncthing.service"); err != nil {
			return r, err
		}
		undos = append(undos, func() {
			if r.GatewayStarted {
				return
			}
			_ = systemctl("unmask", "syncthing.service")
			if wasActive {
				_ = systemctl("start", "syncthing.service")
			}
		})
		// One that systemd didn't start, such as from Syncthing Tray
		if _, err := GatewayCommand("home", "--stop"); err != nil {
			undo()
			return r, err
		}
		backup := h.Dir + ".before-synctrain-" + time.Now().Format("20060102-150405")
		if _, err := GatewayCommand("home", "--backup", backup); err != nil {
			undo()
			return r, err
		}
		r.Backup = backup
	case h.Locked:
		// Taken before, with something else running on it now (a Syncthing started outside its masked unit)
		if _, err := GatewayCommand("home", "--stop"); err != nil {
			return r, err
		}
	}
	if fromFiles {
		if running(FilesSocket()) {
			if err := callFiles("leave", map[string]time.Duration{"timeout": 2 * time.Minute},
				&r.Removals); err != nil {
				undo()
				return r, err
			}
		}
		if err := systemctl("disable", "--now", "synctrain-files.service"); err != nil {
			undo()
			return r, err
		}
		undos = append(undos, func() { _ = systemctl("enable", "--now", "synctrain-files.service") })
	}
	r.GatewayStarted = true
	err := systemctl("enable", "--now", "synctrain-gateway.service")
	if err == nil {
		// A large database may be migrated first
		err = waitForService("synctrain-gateway.service", h.Socket, 10*time.Minute)
	}
	if err == nil {
		err = WriteMode(modeSynchronize)
	}
	if err != nil {
		stopUnit("synctrain-gateway.service")
		undo()
	}
	return r, err
}

func PreviewCopy(ctx context.Context, location, name string, limit int64) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	base = filepath.Join(base, "synctrain", "desktop-previews")
	if err = os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	releaseQuota, err := previewcache.Reserve(ctx, base, limit)
	if err != nil {
		return "", err
	}
	defer releaseQuota()
	dir, err := os.MkdirTemp(base, "preview-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	dest := filepath.Join(dir, filepath.Base(name))
	var reader io.ReadCloser
	if strings.HasPrefix(location, "http://") {
		u, err := url.Parse(location)
		if err != nil || u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
			return "", errors.New("the preview URL is not a local service")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return "", err
		}
		client := http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != 200 && resp.StatusCode != 204 {
			resp.Body.Close()
			return "", fmt.Errorf("preview service returned %s", resp.Status)
		}
		reader = resp.Body
	} else {
		reader, err = os.Open(location)
		if err != nil {
			return "", err
		}
	}
	defer reader.Close()
	stopRead := context.AfterFunc(ctx, func() { reader.Close() })
	defer stopRead()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	n, copyErr := safecopy.Copy(ctx, out, io.LimitReader(reader, limit+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	if n > limit {
		return "", errors.New("the file exceeds the preview size limit")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	success = true
	return dest, nil
}
