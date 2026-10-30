// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

// What a Syncthing home directory holds that makes up a device's configuration, identity and database (and the
// database of Syncthing v1, which the engine migrates)
var homeItems = []string{"config.xml", "cert.pem", "key.pem", "https-cert.pem", "https-key.pem", "index-v2",
	"index-v0.14.0.db"}

var (
	ErrHomeLocked   = errors.New("another Syncthing is running on this configuration")
	ErrHomeInUse    = errors.New("the destination already has files in it")
	ErrHomeNested   = errors.New("the source and destination must be different, and neither inside the other")
	ErrHomeTooNew   = errors.New("the configuration or database is from a newer Syncthing than this engine")
	ErrNoGUIToReach = errors.New("this Syncthing has no REST API on this computer to ask")
)

// Summary describes a Syncthing home directory without changing anything in it
type Summary struct {
	// From cert.pem; empty when there is none
	DeviceID string `json:"deviceID"`
	// Why the device certificate and key can't be used, when they can't; a home without a usable identity is not taken
	// over (the engine would make another)
	IdentityError string `json:"identityError,omitempty"`
	// 0 when there is no config.xml
	ConfigVersion int             `json:"configVersion"`
	Folders       []FolderSummary `json:"folders"`
	Devices       int             `json:"devices"`
	// The IDs of the other devices that are paused
	PausedDevices []string `json:"pausedDevices"`
	GUIEnabled    bool     `json:"guiEnabled"`
	// As in the configuration (whatever STGUIADDRESS this process has)
	GUIAddress      string   `json:"guiAddress"`
	GUINetwork      string   `json:"guiNetwork"`
	ListenAddresses []string `json:"listenAddresses"`
	// The newest schema of its databases; 0 when there is none
	SchemaVersion int `json:"schemaVersion"`
	// Whether this engine could not use it: the configuration or database is from a newer Syncthing
	TooNew bool `json:"tooNew"`
	// Whether a Syncthing runs on it now (holds its lock), or that can't be told
	Locked bool `json:"locked"`
}

type FolderSummary struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Path           string `json:"path"`
	FilesystemType string `json:"filesystemType"`
	Paused         bool   `json:"paused"`
}

// guiNetwork is a GUI address's network as in its configuration (not as STGUIADDRESS would make it)
func guiNetwork(raw string) string {
	if strings.HasPrefix(raw, "/") {
		return "unix"
	}
	return "tcp"
}

// loadIdentity loads a home directory's device certificate and key
func loadIdentity(dir string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	return cert, nil
}

// Summarize reads what a Syncthing home directory holds. Nothing is written there, and no database is opened for
// writing.
func Summarize(dir string) (Summary, error) {
	summary := Summary{Locked: LockHeld(dir)}
	myID := protocol.EmptyDeviceID
	if cert, err := loadIdentity(dir); err == nil {
		myID = protocol.NewDeviceID(cert.Certificate[0])
		summary.DeviceID = myID.String()
	} else {
		summary.IdentityError = err.Error()
	}
	configPath := filepath.Join(dir, "config.xml")
	if _, err := os.Stat(configPath); err == nil {
		cfg, version, err := config.Load(configPath, myID, events.NoopLogger)
		var pathErr *fs.PathError
		switch {
		case errors.As(err, &pathErr):
			return summary, fmt.Errorf("%w: %w", ErrUnreadableConfiguration, err)
		case errors.Is(err, io.EOF):
			return summary, fmt.Errorf("%w: it is empty or truncated", ErrConfiguration)
		case err != nil:
			return summary, fmt.Errorf("%w: %w", ErrConfiguration, err)
		}
		summary.ConfigVersion = version
		summary.TooNew = version > config.CurrentVersion
		raw := cfg.RawCopy()
		for _, folder := range raw.Folders {
			summary.Folders = append(summary.Folders, FolderSummary{ID: folder.ID, Label: folder.Label, Path: folder.Path,
				FilesystemType: folder.FilesystemType.String(), Paused: folder.Paused})
		}
		for _, device := range raw.Devices {
			if myID == protocol.EmptyDeviceID || device.DeviceID != myID {
				summary.Devices++
				if device.Paused {
					summary.PausedDevices = append(summary.PausedDevices, device.DeviceID.String())
				}
			}
		}
		summary.GUIEnabled = raw.GUI.Enabled
		summary.GUIAddress = raw.GUI.RawAddress
		summary.GUINetwork = guiNetwork(raw.GUI.RawAddress)
		summary.ListenAddresses = raw.Options.RawListenAddresses
	} else if !errors.Is(err, os.ErrNotExist) {
		return summary, fmt.Errorf("%w: %w", ErrUnreadableConfiguration, err)
	}
	version, err := newestSchema(filepath.Join(dir, "index-v2"), true)
	if err != nil {
		return summary, err
	}
	summary.SchemaVersion = version
	summary.TooNew = summary.TooNew || version > SupportedSchemaVersion
	return summary, nil
}

// newestSchema is the newest schema version of the databases in a directory, 0 when there are none. Read as
// immutable, nothing is created next to them, but a schema change still in a write-ahead log is missed.
func newestSchema(dbDir string, immutable bool) (int, error) {
	entries, err := os.ReadDir(dbDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	newest := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		version, err := schemaVersion(filepath.Join(dbDir, entry.Name()), immutable)
		if err != nil {
			return 0, fmt.Errorf("reading the schema of %s: %w", entry.Name(), err)
		}
		newest = max(newest, version)
	}
	return newest, nil
}

// LockHeld reports whether a Syncthing holds the lock on a home directory. When that can't be told (the lock file
// can't be opened), it reports true: nothing should go ahead as if no Syncthing ran.
func LockHeld(dir string) bool {
	lockPath := filepath.Join(dir, "syncthing.lock")
	if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		return false
	} else if err != nil {
		return true
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return true
	}
	if locked {
		_ = lock.Unlock()
		return false
	}
	return true
}

// LockHome takes the lock Syncthing takes on its home directory (a flock on syncthing.lock), so that no Syncthing starts
// on it while the returned file stays open; closing it releases the lock. ErrHomeLocked when a Syncthing runs on it.
func LockHome(dir string) (*os.File, error) {
	lockPath := filepath.Join(dir, "syncthing.lock")
	file, err := os.OpenFile(lockPath, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHomeLocked
		}
		return nil, err
	}
	// A Syncthing quitting removes its lock file after unlocking it, and one starting then locks a new file: the lock
	// held must be the file's
	held, heldErr := file.Stat()
	current, currentErr := os.Stat(lockPath)
	if heldErr != nil || currentErr != nil || !os.SameFile(held, current) {
		file.Close()
		return nil, ErrHomeLocked
	}
	return file, nil
}

// AddressInUse reports whether something listens on a TCP address (host:port), as another Syncthing's GUI or sync port,
// by connecting to it (never listening there, which could get in the way of a Syncthing starting). An address that
// isn't one (such as a unix socket's path) is not in use.
func AddressInUse(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	// Something listening on all addresses answers on loopback
	hosts := []string{host}
	if host == "" || host == "0.0.0.0" || host == "::" {
		hosts = []string{"127.0.0.1", "::1"}
	}
	for _, host := range hosts {
		if connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond); err == nil {
			connection.Close()
			return true
		}
	}
	return false
}

// Import copies a Syncthing home directory's configuration, identity and database into dst, which must not exist or be
// an empty directory. The source's lock is held while copying, so no Syncthing runs on it meanwhile; the copy is
// assembled next to dst and renamed into place, so dst appears whole or not at all. A home without a usable device
// identity, or from a newer Syncthing, is refused: the engine would start anew, or could not use it.
func Import(src, dst string) error {
	src, dst = filepath.Clean(src), filepath.Clean(dst)
	if src == dst || strings.HasPrefix(dst, src+string(filepath.Separator)) ||
		strings.HasPrefix(src, dst+string(filepath.Separator)) {
		return ErrHomeNested
	}
	if err := checkEmptyDestination(dst); err != nil {
		return err
	}
	if _, err := loadIdentity(src); err != nil {
		return err
	}

	lock, err := LockHome(src)
	if err != nil {
		return err
	}
	defer lock.Close()

	summary, err := Summarize(src)
	if err != nil {
		return err
	}
	if summary.TooNew {
		return ErrHomeTooNew
	}

	staging := dst + ".importing"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := copyHome(src, staging); err != nil {
		return fail(err)
	}
	// Read normally (in the copy), a newer schema still in a write-ahead log shows too
	if err := CheckSchema(filepath.Join(staging, "index-v2")); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrHomeTooNew, err))
	}
	if err := syncDir(staging); err != nil {
		return fail(err)
	}
	if err := removeEmptyDestination(dst); err != nil {
		return fail(err)
	}
	if err := os.Rename(staging, dst); err != nil {
		return fail(err)
	}
	return syncDir(filepath.Dir(dst))
}

// checkEmptyDestination refuses a destination that exists as anything but an empty directory (Finder's .DS_Store
// aside)
func checkEmptyDestination(dst string) error {
	info, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrHomeInUse
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".DS_Store" {
			return ErrHomeInUse
		}
	}
	return nil
}

func removeEmptyDestination(dst string) error {
	if err := checkEmptyDestination(dst); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dst, ".DS_Store"))
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrHomeInUse, err)
	}
	return nil
}

func copyHome(src, dst string) error {
	for _, item := range homeItems {
		from := filepath.Join(src, item)
		info, err := os.Lstat(from)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			if err := copyFileSynced(from, filepath.Join(dst, item)); err != nil {
				return err
			}
		case info.IsDir():
			if err := copyDatabase(from, filepath.Join(dst, item)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is neither a file nor a directory", item)
		}
	}
	return nil
}

// copyDatabase copies a database directory: its database files with their write-ahead logs and shared memory. Anything
// else in it (a link, a directory) would leave a database behind, so it stops the copy; only the temporary directory
// Syncthing makes there on Linux is left out.
func copyDatabase(from, to string) error {
	if err := os.Mkdir(to, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".tmp" && entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s in %s is not a file", entry.Name(), filepath.Base(from))
		}
		if err := copyFileSynced(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
			return err
		}
	}
	return syncDir(to)
}

// copyFileSynced copies a file (as a clone where the volume can, which takes no time or space) for this user only,
// and syncs it
func copyFileSynced(from, to string) error {
	if err := cloneFile(from, to); err != nil {
		if err := copyFileContents(from, to); err != nil {
			return err
		}
	}
	target, err := os.OpenFile(to, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := target.Chmod(0o600); err != nil {
		target.Close()
		return err
	}
	if err := target.Sync(); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

func copyFileContents(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

// syncDir syncs a directory, so that the entries made or renamed in it last
func syncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// isLocalHost reports whether a GUI host is this computer's own: unspecified (all addresses), a loopback address, or
// localhost. A name other than localhost resolves elsewhere.
func isLocalHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// RequestShutdown asks the Syncthing running on a home directory to shut down, through its REST API on this computer
// (the address and API key in its configuration). It first checks that what answers there is that Syncthing (its
// device ID), so that the key goes nowhere else. It returns once the request was accepted, not when it has stopped:
// wait for LockHeld to turn false.
func RequestShutdown(dir string) error {
	cert, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	myID := protocol.NewDeviceID(cert.Certificate[0])
	cfg, _, err := config.Load(filepath.Join(dir, "config.xml"), myID, events.NoopLogger)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	gui := cfg.RawCopy().GUI
	host, port, err := net.SplitHostPort(gui.RawAddress)
	if !gui.Enabled || guiNetwork(gui.RawAddress) != "tcp" || err != nil || !isLocalHost(host) {
		return ErrNoGUIToReach
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	scheme := "http"
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	if gui.RawUseTLS {
		// Its own self-signed certificate, on this computer; the device ID is checked below
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		// The key is for this address only
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := scheme + "://" + net.JoinHostPort(host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	health, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/noauth/health", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(health)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.Header.Get("X-Syncthing-ID") != myID.String() {
		return fmt.Errorf("%w: another program answers at %s", ErrNoGUIToReach, net.JoinHostPort(host, port))
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rest/system/shutdown", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", gui.APIKey)
	response, err = client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: it refused the API key in its configuration", ErrNoGUIToReach)
	default:
		return fmt.Errorf("Syncthing refused to shut down: %s", response.Status)
	}
}
