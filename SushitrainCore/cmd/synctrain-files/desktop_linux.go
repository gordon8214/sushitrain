// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"t-shaped.nl/sushitrain/v2/files"
)

// credentialStore is where credentials are kept: the desktop's keyring when there is one (the credentials in the
// file are moved into it), the file otherwise
func credentialStore(mode, stateDir string, logger *slog.Logger) (files.CredentialStore, error) {
	file := &files.FileCredentials{Path: filepath.Join(stateDir, "credentials.json")}
	if mode == "file" {
		return file, nil
	}
	keyring, err := files.NewSecretService("synctrain")
	if err != nil {
		if mode == "keyring" {
			return nil, fmt.Errorf("no keyring: %w", err)
		}
		logger.Info("No keyring: credentials are kept in a file only this user can read", "reason", err)
		return file, nil
	}
	if moved, err := files.MoveCredentials(file, keyring); err != nil {
		logger.Warn("Could not move credentials into the keyring", "error", err)
	} else if moved > 0 {
		logger.Info("Moved credentials into the keyring", "count", moved)
	}
	return keyring, nil
}

// notifier shows desktop notifications, at most one of a kind every ten minutes
type notifier struct {
	log   *slog.Logger
	mutex sync.Mutex
	last  map[string]time.Time
	ids   map[string]uint32
}

func newNotifier(logger *slog.Logger) *notifier {
	return &notifier{log: logger, last: map[string]time.Time{}, ids: map[string]uint32{}}
}

func (n *notifier) notify(notice files.Notice) {
	n.mutex.Lock()
	if time.Since(n.last[notice.Kind]) < 10*time.Minute {
		n.mutex.Unlock()
		return
	}
	n.last[notice.Kind] = time.Now()
	replaces := n.ids[notice.Kind]
	n.mutex.Unlock()
	go func() {
		conn, err := dbus.ConnectSessionBus()
		if err != nil {
			n.log.Debug("No desktop notifications", "error", err)
			return
		}
		defer conn.Close()
		summary := "Synctrain"
		switch notice.Kind {
		case "review":
			summary = "Changes can't be sent"
		case "unreachable":
			summary = "A gateway can't be reached"
		}
		var id uint32
		err = conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").Call(
			"org.freedesktop.Notifications.Notify", 0, "Synctrain", replaces, "folder-remote", summary, notice.Message,
			[]string{}, map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant("com.gordonbyrnes.Synctrain")},
			int32(-1)).Store(&id)
		if err != nil {
			n.log.Debug("Could not show a notification", "error", err)
			return
		}
		n.mutex.Lock()
		n.ids[notice.Kind] = id
		n.mutex.Unlock()
	}()
}

// bookmarksPath is GTK's list of the places in the sidebar of Nautilus and the file chooser
func bookmarksPath() string {
	return filepath.Join(xdgDir("XDG_CONFIG_HOME", ".config"), "gtk-3.0", "bookmarks")
}

// syncBookmarks puts each folder in the sidebar of Nautilus and the file chooser, and takes out bookmarks of folders
// that are no longer in the mount. Other bookmarks are left as they are.
func syncBookmarks(mount string, folders []files.Folder, logger *slog.Logger) {
	path := bookmarksPath()
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("Could not read GTK's bookmarks", "error", err)
		return
	}
	prefix := files.FileURI(mount) + "/"
	want := map[string]string{}
	for _, f := range folders {
		want[files.FileURI(filepath.Join(mount, f.DirName))] = f.Label
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(existing))
	for scanner.Scan() {
		line := scanner.Text()
		uri, _, _ := strings.Cut(line, " ")
		if strings.HasPrefix(uri, prefix) {
			if _, ok := want[uri]; !ok {
				// A folder that was removed
				continue
			}
			delete(want, uri)
		}
		out.WriteString(line + "\n")
	}
	if len(want) == 0 && out.Len() == len(existing) {
		return
	}
	for uri, label := range want {
		fmt.Fprintf(&out, "%s %s\n", uri, label)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
		return
	}
	tmp := path + ".synctrain"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
	}
}

// excludeFromBackups adds the mount to Déjà Dup's excluded folders, when Déjà Dup is installed: it backs up the home
// folder, and would download every file in the mount
func excludeFromBackups(mount string, logger *slog.Logger) {
	out, err := exec.Command("gsettings", "get", "org.gnome.DejaDup", "exclude-list").Output()
	if err != nil {
		// Not installed
		return
	}
	current := strings.TrimSpace(string(out))
	if strings.Contains(current, "'"+mount+"'") {
		return
	}
	var list string
	switch {
	case current == "@as []" || current == "[]":
		list = "['" + mount + "']"
	case strings.HasSuffix(current, "]"):
		list = strings.TrimSuffix(current, "]") + ", '" + mount + "']"
	default:
		return
	}
	if err := exec.Command("gsettings", "set", "org.gnome.DejaDup", "exclude-list", list).Run(); err != nil {
		logger.Warn("Could not exclude the folders from Déjà Dup's backups", "error", err)
		return
	}
	logger.Info("Excluded the folders from Déjà Dup's backups", "path", mount)
}
