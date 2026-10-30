// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
)

const crashTrace = "panic: test\n\ngoroutine 1 [running]:\nmain.main()\n"

// crashServer records what a crash reporting server gets; it knows the reports in known
type crashServer struct {
	*httptest.Server
	mutex    sync.Mutex
	known    map[string]bool
	received map[string]string
	failing  bool
}

func newCrashServer(t *testing.T) *crashServer {
	t.Helper()
	server := &crashServer{known: map[string]bool{}, received: map[string]string{}}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		hash := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case server.failing:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodHead && !server.known[hash]:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			server.received[hash] = string(body)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeCrashLog(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCrashesAreSentInTheLayoutOfSyncthingsReports(t *testing.T) {
	server := newCrashServer(t)
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "syncthing v2.1.7\n"+crashTrace)
	crashed := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(log, crashed, crashed); err != nil {
		t.Fatal(err)
	}

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")

	if len(server.received) != 1 {
		t.Fatalf("received %d reports", len(server.received))
	}
	want := "syncthing v2.1.7\nPanic at " + crashed.Local().Format(time.RFC3339) + "\n" + crashTrace
	if report := server.received[fmt.Sprintf("%x", sha256.Sum256([]byte(want)))]; report != want {
		t.Fatalf("received %v", server.received)
	}
	if exists(log) {
		t.Fatal("the log would be sent again")
	}
	if !exists(strings.TrimSuffix(log, ".log") + crashLogDoneSuffix) {
		t.Fatal("the log was not kept")
	}

	// Not again at the next start
	server.received = map[string]string{}
	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")
	if len(server.received) != 0 {
		t.Fatalf("received %v", server.received)
	}
}

func TestACrashTheServerKnowsIsNotSentAgain(t *testing.T) {
	server := newCrashServer(t)
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n"+crashTrace)
	report, err := readCrashReport(log)
	if err != nil {
		t.Fatal(err)
	}
	server.known[fmt.Sprintf("%x", sha256.Sum256(report))] = true

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")
	if len(server.received) != 0 || exists(log) {
		t.Fatalf("received %v, log left: %v", server.received, exists(log))
	}
}

func TestCrashesFromWhileReportingWasOffAreNeverSent(t *testing.T) {
	server := newCrashServer(t)
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n"+crashTrace)

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: false, CRURL: server.URL}, dir, "")
	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")

	if len(server.received) != 0 {
		t.Fatalf("received %v", server.received)
	}
	if exists(log) || !exists(strings.TrimSuffix(log, ".log")+crashLogDoneSuffix) {
		t.Fatal("the log was not set aside")
	}
}

func TestACrashIsSentAgainAfterAFailure(t *testing.T) {
	server := newCrashServer(t)
	server.failing = true
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n"+crashTrace)

	opts := config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}
	reportCrashes(t.Context(), opts, dir, "")
	if !exists(log) {
		t.Fatal("the log is gone")
	}

	server.failing = false
	reportCrashes(t.Context(), opts, dir, "")
	if len(server.received) != 1 {
		t.Fatalf("received %v", server.received)
	}
}

func TestLogsWithoutACrashAreRemoved(t *testing.T) {
	server := newCrashServer(t)
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n")
	current := writeCrashLog(t, dir, "synctrain-crash-2.log", "version\n")

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, current)

	if exists(log) || !exists(current) || len(server.received) != 0 {
		t.Fatalf("log left: %v, current left: %v, received %v", exists(log), exists(current), server.received)
	}
}

func TestAnOldCrashIsSentBeforeItsLogExpires(t *testing.T) {
	server := newCrashServer(t)
	dir := t.TempDir()
	log := writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n"+crashTrace)
	old := time.Now().Add(-logRetention - time.Hour)
	if err := os.Chtimes(log, old, old); err != nil {
		t.Fatal(err)
	}

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")

	if len(server.received) != 1 {
		t.Fatalf("received %v", server.received)
	}
}

func TestCrashesAreFoundWhateverTheFolderIsCalled(t *testing.T) {
	server := newCrashServer(t)
	dir := filepath.Join(t.TempDir(), "Config[1]")
	sibling := filepath.Join(filepath.Dir(dir), "Config1")
	for _, path := range []string{dir, sibling} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCrashLog(t, dir, "synctrain-crash-1.log", "version\n"+crashTrace)
	neighbor := writeCrashLog(t, sibling, "synctrain-crash-2.log", "neighbor\n"+crashTrace)

	reportCrashes(t.Context(), config.OptionsConfiguration{CREnabled: true, CRURL: server.URL}, dir, "")

	if len(server.received) != 1 {
		t.Fatalf("received %v", server.received)
	}
	if !exists(neighbor) {
		t.Fatal("another folder's log was handled")
	}
}

func TestLogsAreRemovedAfterAWeek(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-logRetention - time.Hour)
	expired := []string{
		writeCrashLog(t, dir, "synctrain-crash-1.done.log", "version\n"+crashTrace),
		writeCrashLog(t, dir, "audit-1.log", "{}\n"),
	}
	current := writeCrashLog(t, dir, "synctrain-crash-2.log", "version\n")
	other := writeCrashLog(t, dir, "synctrain-2026-01-01-00-00-00.log", "the app's own log\n")
	recent := writeCrashLog(t, dir, "audit-2.log", "{}\n")
	for _, path := range append([]string{current, other}, expired...) {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	reportCrashes(t.Context(), config.OptionsConfiguration{}, dir, current)

	for _, path := range expired {
		if exists(path) {
			t.Errorf("%s was kept", path)
		}
	}
	for _, path := range []string{current, other, recent} {
		if !exists(path) {
			t.Errorf("%s was removed", path)
		}
	}
}

// When run with the variable set, this is the process that crashes: it saves the crash in the directory it names
func TestCrashHelper(t *testing.T) {
	dir := os.Getenv("SYNCTRAIN_TEST_CRASH_DIR")
	if dir == "" {
		t.Skip("only runs as the helper of TestACrashOfTheProcessIsSaved")
	}
	if _, err := startCrashLog(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	panic("the test's crash")
}

func TestACrashOfTheProcessIsSaved(t *testing.T) {
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
	// The temporary directory is for what the crash leaves behind, which it doesn't get to clean up
	helper.Env = append(os.Environ(), "SYNCTRAIN_TEST_CRASH_DIR="+dir, "TMPDIR="+t.TempDir())
	if err := helper.Run(); err == nil {
		t.Fatal("the helper did not crash")
	}

	logs, err := filepath.Glob(filepath.Join(dir, crashLogPattern))
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs: %v, %v", logs, err)
	}
	report, err := readCrashReport(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(report), "\n", 3)
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "syncthing ") || !strings.HasPrefix(lines[1], "Panic at ") ||
		!strings.HasPrefix(lines[2], "panic: the test's crash") {
		t.Fatalf("report: %s", report)
	}
}

func TestReportsCarryAVersionCrashServersAccept(t *testing.T) {
	version, host, user, longVersion := build.Version, build.Host, build.User, build.LongVersion
	t.Cleanup(func() {
		build.Version, build.Host, build.User, build.LongVersion = version, host, user, longVersion
	})

	setBuildInfo()

	// What Syncthing's crash receiver parses, and refuses what it can't
	parsed, err := build.ParseVersion(build.LongVersion)
	if err != nil {
		t.Fatalf("%q: %v", build.LongVersion, err)
	}
	if parsed.Tag != "v2.1.7" || !slices.Contains(parsed.Extra, "synctrain") {
		t.Fatalf("%q: %+v", build.LongVersion, parsed)
	}
}

func TestReportingIsLeftAloneOnceMarked(t *testing.T) {
	dir := t.TempDir()
	conf := config.Configuration{Options: config.OptionsConfiguration{URAccepted: 3, CREnabled: true}}
	if !prepareReporting(&conf, dir) {
		t.Fatal("the marker would not be written")
	}
	if conf.Options.URAccepted != -1 || conf.Options.CREnabled {
		t.Fatalf("not turned off: %+v", conf.Options)
	}

	writeReportingMarker(dir)
	conf.Options.URAccepted, conf.Options.CREnabled = 3, true
	if prepareReporting(&conf, dir) {
		t.Fatal("the marker would be written again")
	}
	if conf.Options.URAccepted != 3 || !conf.Options.CREnabled {
		t.Fatalf("turned off again: %+v", conf.Options)
	}
}
