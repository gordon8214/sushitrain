// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/structutil"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

// Usage and crash reporting are the user's choice, and off until they turn them on. Syncthing's services send usage
// and failure reports by themselves. Crash reports are the work of Syncthing's monitor process, which the app doesn't
// have, so the app saves its crashes and sends them at the next start (reportCrashes).

// The file next to the configuration that records that the app turned reporting off for it once, so that turning it on
// later lasts
const reportingMarkerName = "synctrain-reporting-reset"

const (
	crashLogPattern = "synctrain-crash-*.log"
	auditLogPattern = "audit-*.log"
	// A crash log the app is done with, kept for a look: it was sent, or crash reporting was off when it was found
	crashLogDoneSuffix = ".done.log"
	// What Syncthing keeps its own crash and audit logs for
	logRetention     = 7 * 24 * time.Hour
	crashSendTimeout = time.Minute
)

// prepareReporting turns reporting off once for a configuration being loaded on macOS, as one from Syncthing.app can
// have it on, by the user's choice in another app or as its default. Afterwards it stays as the user sets it, also in
// the web GUI or through the REST API. It reports whether to write the marker once the configuration is saved.
func prepareReporting(conf *config.Configuration, configDir string) bool {
	_, err := os.Stat(filepath.Join(configDir, reportingMarkerName))
	if err == nil {
		fillReportingURLs(&conf.Options)
		return false
	}
	conf.Options.URAccepted = -1
	conf.Options.CREnabled = false
	return errors.Is(err, os.ErrNotExist)
}

func writeReportingMarker(configDir string) {
	if err := writeFileSynced(filepath.Join(configDir, reportingMarkerName), nil); err != nil {
		slog.Warn("could not record that reporting was turned off", "cause", err)
	}
}

// fillReportingURLs gives reporting that is on a place to report to, as the configurations the app makes have none.
// Syncthing sends its failure reports with usage reporting on, to the crash reporting address.
func fillReportingURLs(opts *config.OptionsConfiguration) {
	var defaults config.OptionsConfiguration
	structutil.SetDefaults(&defaults)
	if opts.URAccepted > 0 && opts.URURL == "" {
		opts.URURL = defaults.URURL
	}
	if (opts.URAccepted > 0 || opts.CREnabled) && opts.CRURL == "" {
		opts.CRURL = defaults.CRURL
	}
}

// startCrashLog has the Go runtime save a crash of this process in a new file in dir, as well as print it, and returns
// the file's path. The file starts with the version line of a crash report, as the crash may come after an update.
func startCrashLog(dir string, started time.Time) (string, error) {
	path := filepath.Join(dir, started.UTC().Format("synctrain-crash-2006-01-02-15-04-05")+".log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(build.LongVersion + "\n"); err != nil {
		return "", err
	}
	return path, debug.SetCrashOutput(file, debug.CrashOptions{})
}

// reportCrashes goes through the crash logs in dir, except current, the one this process writes to. With crash
// reporting on it sends each crash. With it off, it sets them aside, so that turning it on later doesn't send crashes
// from when it was off. It then removes logs Syncthing's own start would have removed.
func reportCrashes(ctx context.Context, opts config.OptionsConfiguration, dir string, current string) {
	for _, log := range logsMatching(dir, crashLogPattern) {
		if log == current || strings.HasSuffix(log, crashLogDoneSuffix) {
			continue
		}
		report, err := readCrashReport(log)
		if err != nil {
			slog.Warn("could not read a crash log", "path", log, "cause", err)
			continue
		}
		if report == nil {
			// The app didn't crash
			_ = os.Remove(log)
			continue
		}
		if opts.CREnabled {
			if err := sendCrashReport(ctx, opts.CRURL, report); err != nil {
				// To try again at the next start
				slog.Warn("could not send a crash report", "path", log, "cause", err)
				continue
			}
			slog.Info("sent a crash report", "path", log)
		}
		_ = os.Rename(log, strings.TrimSuffix(log, ".log")+crashLogDoneSuffix)
	}
	removeOldLogs(dir, current)
}

// removeOldLogs removes crash and audit logs older than a week, except the crash log still in use
func removeOldLogs(dir string, current string) {
	for _, pattern := range []string{crashLogPattern, auditLogPattern} {
		for _, log := range logsMatching(dir, pattern) {
			if info, err := os.Stat(log); err == nil && log != current && time.Since(info.ModTime()) > logRetention {
				_ = os.Remove(log)
			}
		}
	}
}

// logsMatching lists the files in dir whose names match a pattern. Unlike filepath.Glob, it doesn't take the name of the
// directory for part of the pattern: a folder the user chose can be called anything.
func logsMatching(dir string, pattern string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("could not list the logs", "directory", dir, "cause", err)
		return nil
	}
	var logs []string
	for _, entry := range entries {
		if matched, err := filepath.Match(pattern, entry.Name()); err == nil && matched {
			logs = append(logs, filepath.Join(dir, entry.Name()))
		}
	}
	return logs
}

// readCrashReport makes the report of a crash log in the layout of Syncthing's crash reports; nil for a log without a
// crash. Having no log lines in it, it needs none of the filtering that Syncthing's reports do.
func readCrashReport(log string) ([]byte, error) {
	info, err := os.Stat(log)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(log)
	if err != nil {
		return nil, err
	}
	version, trace, _ := bytes.Cut(data, []byte("\n"))
	if len(bytes.TrimSpace(trace)) == 0 {
		return nil, nil
	}
	return fmt.Appendf(nil, "%s\nPanic at %s\n%s", version, info.ModTime().Format(time.RFC3339), trace), nil
}

// sendCrashReport sends a report to the crash reporting server at urlBase, as Syncthing does: under the hash of its
// contents, unless the server has it already
func sendCrashReport(ctx context.Context, urlBase string, report []byte) error {
	url := fmt.Sprintf("%s/%x", urlBase, sha256.Sum256(report))
	status, err := crashRequest(ctx, http.MethodHead, url, nil)
	if err != nil || status == http.StatusOK {
		return err
	}
	status, err = crashRequest(ctx, http.MethodPut, url, report)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the server answered with status %d", status)
	}
	return nil
}

func crashRequest(ctx context.Context, method string, url string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, crashSendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", build.UserAgent())
	response, err := tlsutil.ShortLivedHTTPClient.Do(request)
	if err != nil {
		return 0, err
	}
	response.Body.Close()
	return response.StatusCode, nil
}
