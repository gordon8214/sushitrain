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
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"
	"t-shaped.nl/sushitrain/v2/files"
)

// credentialStore is where credentials are kept: the desktop's keyring when there is one (the credentials in the
// file are moved into it), the file otherwise
func credentialStore(mode, stateDir string, logger *slog.Logger) (files.CredentialStore, error) {
	file := &files.FileCredentials{Path: filepath.Join(stateDir, "credentials.json")}
	switch mode {
	case "file":
		return file, nil
	case "auto", "keyring":
	default:
		return nil, fmt.Errorf("--credentials %q: give auto, keyring or file", mode)
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
		// Those left in the file are still read from it, until a later start moves them
		logger.Warn("Could not move credentials into the keyring", "error", err)
		return &fallbackCredentials{keyring: keyring, file: file}, nil
	} else if moved > 0 {
		logger.Info("Moved credentials into the keyring", "count", moved)
	}
	return keyring, nil
}

// fallbackCredentials keeps credentials in the keyring, and reads those not moved there yet from the file
type fallbackCredentials struct {
	keyring files.CredentialStore
	file    *files.FileCredentials
}

func (f *fallbackCredentials) Credential(serviceID string) (string, error) {
	if credential, err := f.keyring.Credential(serviceID); err != nil || credential != "" {
		return credential, err
	}
	return f.file.Credential(serviceID)
}

func (f *fallbackCredentials) SetCredential(serviceID, credential string) error {
	return f.keyring.SetCredential(serviceID, credential)
}

func (f *fallbackCredentials) DeleteCredential(serviceID string) error {
	if err := f.file.DeleteCredential(serviceID); err != nil {
		return err
	}
	return f.keyring.DeleteCredential(serviceID)
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

// bookmarksMutex keeps two syncs (of folders added and removed at once) from writing the file over each other
var bookmarksMutex sync.Mutex

// syncBookmarks puts each folder in the sidebar of Nautilus and the file chooser, and takes out bookmarks of folders
// that are no longer in the mount. Other bookmarks, also of places inside the folders, are left as they are.
func syncBookmarks(mount string, folders []files.Folder, logger *slog.Logger) {
	bookmarksMutex.Lock()
	defer bookmarksMutex.Unlock()
	path := bookmarksPath()
	// A link (from a dotfiles repository, say) stays one: the file it points to is written
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("Could not read GTK's bookmarks", "error", err)
		return
	}
	prefix := files.FileURI(mount) + "/"
	want := map[string]string{}
	for _, f := range folders {
		want[files.FileURI(filepath.Join(mount, f.DirName))] = bookmarkLabel(f)
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(existing))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		uri, _, _ := strings.Cut(line, " ")
		// A folder's own bookmark is the URI of its directory in the mount, one name below it
		if rest, ok := strings.CutPrefix(uri, prefix); ok && rest != "" && !strings.Contains(rest, "/") {
			label, ok := want[uri]
			if !ok {
				// A folder that was removed
				continue
			}
			delete(want, uri)
			line = uri + " " + label
		}
		out.WriteString(line + "\n")
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("Could not read GTK's bookmarks", "error", err)
		return
	}
	for uri, label := range want {
		fmt.Fprintf(&out, "%s %s\n", uri, label)
	}
	if bytes.Equal(out.Bytes(), existing) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bookmarks-*")
	if err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(out.Bytes())
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		logger.Warn("Could not write GTK's bookmarks", "error", err)
	}
}

// bookmarkLabel is a folder's label as one line of the bookmarks file: the gateway chose it, and a line break in it
// would add a bookmark of its own
func bookmarkLabel(f files.Folder) string {
	label := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return ' '
		}
		return r
	}, f.Label))
	if label == "" {
		return f.DirName
	}
	return label
}

// excludeFromBackups adds the mount to Déjà Dup's excluded folders, when Déjà Dup is installed: it backs up the home
// folder, and would download every file in the mount
func excludeFromBackups(mount string, logger *slog.Logger) {
	out, err := exec.Command("gsettings", "get", "org.gnome.DejaDup", "exclude-list").Output()
	if err != nil {
		// Not installed
		return
	}
	current, ok := parseStringArray(string(out))
	if !ok {
		logger.Warn("Could not read Déjà Dup's excluded folders", "value", strings.TrimSpace(string(out)))
		return
	}
	if slices.Contains(current, mount) {
		return
	}
	list := formatStringArray(append(current, mount))
	if err := exec.Command("gsettings", "set", "org.gnome.DejaDup", "exclude-list", list).Run(); err != nil {
		logger.Warn("Could not exclude the folders from Déjà Dup's backups", "error", err)
		return
	}
	logger.Info("Excluded the folders from Déjà Dup's backups", "path", mount)
}

// parseStringArray reads an array of strings as gsettings prints one (GVariant text): either quote, and backslash
// escapes
func parseStringArray(text string) ([]string, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "@as"))
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		return nil, false
	}
	text = text[1 : len(text)-1]
	list := []string{}
	for i := 0; i < len(text); {
		switch c := text[i]; c {
		case ' ', ',':
			i++
		case '\'', '"':
			var b strings.Builder
			for i++; i < len(text) && text[i] != c; i++ {
				if text[i] == '\\' && i+1 < len(text) {
					i++
				}
				b.WriteByte(text[i])
			}
			if i >= len(text) {
				return nil, false
			}
			i++
			list = append(list, b.String())
		default:
			return nil, false
		}
	}
	return list, true
}

// formatStringArray writes an array of strings as GVariant text
func formatStringArray(list []string) string {
	quote := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	quoted := make([]string, len(list))
	for i, v := range list {
		quoted[i] = "'" + quote.Replace(v) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
