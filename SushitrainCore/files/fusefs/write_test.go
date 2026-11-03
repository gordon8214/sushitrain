// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

// TestMain runs the test executable as a daemon when the crash tests start it as one
func TestMain(m *testing.M) {
	if dir := os.Getenv("SYNCTRAIN_TEST_DAEMON"); dir != "" {
		runDaemon(dir)
		return
	}
	os.Exit(m.Run())
}

func testConfig(dir string) files.Config {
	delay, _ := time.ParseDuration(os.Getenv("SYNCTRAIN_TEST_SETTLE"))
	if delay == 0 {
		delay = 100 * time.Millisecond
	}
	return files.Config{StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
		Credentials: &files.FileCredentials{Path: filepath.Join(dir, "credentials.json")}, ClientName: "test",
		AllowLocalGateway: true, SettleDelay: delay,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
}

// runDaemon serves the service in dir at dir/mnt until it is killed
func runDaemon(dir string) {
	s, err := files.Open(testConfig(dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m, err := MountService(filepath.Join(dir, "mnt"), s, Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := s.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
	m.Unmount()
	s.Close()
}

// settled waits until the folder's changes are committed by the gateway
func (m *mounted) settled() {
	m.t.Helper()
	root, ok, err := m.service.Lookup(0, m.folder.DirName)
	if err != nil || !ok {
		m.t.Fatalf("no folder root: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.service.Wait(ctx, root.Item); err != nil {
		m.t.Fatalf("waiting for changes: %v", err)
	}
}

func (m *mounted) gatewayHas(name, want string) {
	m.t.Helper()
	got, ok := m.g.Engine.Read(name)
	if !ok || got != want {
		m.t.Fatalf("the gateway has %q at %s (exists: %v), want %q", got, name, ok, want)
	}
}

func (m *mounted) gatewayLacks(name string) {
	m.t.Helper()
	if _, err := os.Lstat(m.g.Engine.Path(name)); err == nil {
		m.t.Fatalf("the gateway has %s", name)
	}
}

// itemID is the gateway's ID of a file, as its index on the gateway's side sees it: the ID the client knows
func (m *mounted) gatewayID(name string) string {
	m.t.Helper()
	e, err := m.service.Resolve(m.folder.DirName + "/" + name)
	if err != nil {
		m.t.Fatal(err)
	}
	id, err := m.service.GatewayID(e.Item)
	if err != nil || id == "" {
		m.t.Fatalf("no gateway ID for %s: %v", name, err)
	}
	return id
}

// run runs a command, which must succeed without complaining
func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	if name != "git" && len(out) > 0 {
		t.Fatalf("%s %v said:\n%s", name, args, out)
	}
}

func writableMount(t *testing.T, contents map[string]string) *mounted {
	t.Helper()
	e := gatewaytest.NewEngine(t, "docs", nil)
	for name, c := range contents {
		e.Write(name, c)
	}
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	return mountGateway(t, g)
}

func TestWritesThroughTheMount(t *testing.T) {
	m := writableMount(t, map[string]string{"report.txt": "hello world\n", "keep.txt": "keep"})
	docs := m.path("docs")
	id := m.gatewayID("report.txt")

	// A new file, appended to
	if err := os.WriteFile(filepath.Join(docs, "new.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(docs, "new.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("two\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	m.settled()
	m.gatewayHas("new.txt", "one\ntwo\n")

	// Saves the way editors and tools make them: each one replace of the same item
	run(t, docs, "sed", "-i", "s/hello/hi/", "report.txt")
	m.settled()
	m.gatewayHas("report.txt", "hi world\n")
	cmd := exec.Command("gio", "save", filepath.Join(docs, "report.txt"))
	cmd.Stdin = strings.NewReader("saved by gio\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gio save: %v\n%s", err, out)
	}
	m.settled()
	m.gatewayHas("report.txt", "saved by gio\n")
	for _, backupcopy := range []string{"yes", "no", "auto"} {
		run(t, docs, "vi", "-Es", "-u", "NONE", "-c", "set backup backupcopy="+backupcopy, "-c",
			"%s/.*/edited with backupcopy="+backupcopy+"/", "-c", "wq", "report.txt")
		m.settled()
		m.gatewayHas("report.txt", "edited with backupcopy="+backupcopy+"\n")
		m.gatewayLacks("report.txt~")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "report.txt"), []byte("synced by rsync\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, docs, "rsync", "-t", filepath.Join(src, "report.txt"), filepath.Join(docs, "report.txt"))
	m.settled()
	m.gatewayHas("report.txt", "synced by rsync\n")
	if m.gatewayID("report.txt") != id {
		t.Fatal("a save made a new item on the gateway")
	}
	entries, _ := os.ReadDir(m.g.Engine.FolderPath)
	for _, e := range entries {
		if e.Name() != "report.txt" && e.Name() != "keep.txt" && e.Name() != "new.txt" && e.Name() != ".stfolder" &&
			e.Name() != ".stversions" && e.Name() != "archived.txt" {
			t.Errorf("a save left %s on the gateway", e.Name())
		}
	}

	// cp -p and touch set times; chmod is accepted; cp -a copies what it can
	run(t, docs, "cp", "-p", filepath.Join(src, "report.txt"), "copy.txt")
	run(t, docs, "cp", "-a", filepath.Join(src, "report.txt"), "archived.txt")
	run(t, docs, "touch", "-d", "2020-01-02 03:04:05", "copy.txt")
	if err := os.Chmod(filepath.Join(docs, "copy.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.settled()
	m.gatewayHas("copy.txt", "synced by rsync\n")
	if info, err := os.Stat(m.g.Engine.Path("copy.txt")); err != nil || info.ModTime().Year() != 2020 {
		t.Fatalf("the copy's time on the gateway: %v, %v", info.ModTime(), err)
	}

	// truncate(2), and a write through mmap
	if err := os.Truncate(filepath.Join(docs, "copy.txt"), 6); err != nil {
		t.Fatal(err)
	}
	mf, err := os.OpenFile(filepath.Join(docs, "copy.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := unix.Mmap(int(mf.Fd()), 0, 6, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	copy(mem, "MAPPED")
	unix.Msync(mem, unix.MS_SYNC)
	unix.Munmap(mem)
	mf.Close()
	m.settled()
	m.gatewayHas("copy.txt", "MAPPED")

	// An open for writing that writes nothing sends nothing
	before := m.service.SentCount()
	f, err = os.OpenFile(filepath.Join(docs, "keep.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	time.Sleep(500 * time.Millisecond)
	m.settled()
	if m.service.SentCount() != before {
		t.Fatal("opening a file for writing sent something")
	}

	// A save that renames over a file another handle still has open for writing: the stale handle's writes are
	// dropped, as for a file removed on a local filesystem
	held, err := os.OpenFile(filepath.Join(docs, "keep.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "lu123.tmp"), []byte("the save"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(docs, "lu123.tmp"), filepath.Join(docs, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := held.WriteAt([]byte("stale"), 0); err != nil {
		t.Fatal(err)
	}
	held.Close()
	m.settled()
	m.gatewayHas("keep.txt", "the save")

	// Directories: mkdir -p, a tree from tar, a rename, rm -r
	if err := os.MkdirAll(filepath.Join(docs, "a/b/c"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	for i := range 20 {
		p := filepath.Join(tree, "tree", fmt.Sprintf("d%d", i%4), fmt.Sprintf("f%d.txt", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(fmt.Sprint(i)), 0o644)
	}
	run(t, tree, "tar", "cf", "tree.tar", "tree")
	run(t, docs, "tar", "xf", filepath.Join(tree, "tree.tar"))
	if err := os.Rename(filepath.Join(docs, "a"), filepath.Join(docs, "renamed")); err != nil {
		t.Fatal(err)
	}
	m.settled()
	m.gatewayHas("tree/d3/f19.txt", "19")
	if _, err := os.Stat(m.g.Engine.Path("renamed/b/c")); err != nil {
		t.Fatalf("the renamed directories: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(docs, "tree")); err != nil {
		t.Fatal(err)
	}
	m.settled()
	m.gatewayLacks("tree")
	m.refresh()
	if _, err := os.Stat(filepath.Join(docs, "tree")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed tree: %v", err)
	}

	// git clone into the mount
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("repo\n"), 0o644)
	run(t, repo, "git", "-c", "user.name=t", "-c", "user.email=t@example.org", "add", "README")
	run(t, repo, "git", "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "-m", "first")
	run(t, docs, "git", "clone", "-q", repo, "clone")
	m.settled()
	m.gatewayHas("clone/README", "repo\n")
	if _, err := os.Stat(m.g.Engine.Path("clone/.git/HEAD")); err != nil {
		t.Fatalf("the clone's .git: %v", err)
	}

	// What can't be in a folder of a gateway
	if err := os.Symlink("report.txt", filepath.Join(docs, "link")); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("a symbolic link: %v", err)
	}
	if err := os.Link(filepath.Join(docs, "report.txt"), filepath.Join(docs, "hard")); !errors.Is(err,
		fs.ErrPermission) {
		t.Fatalf("a hard link: %v", err)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(docs, "report.txt"), unix.AT_FDCWD,
		filepath.Join(docs, "keep.txt"), unix.RENAME_EXCHANGE); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("exchanging two files: %v", err)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(docs, "report.txt"), unix.AT_FDCWD,
		filepath.Join(docs, "keep.txt"), unix.RENAME_NOREPLACE); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("renaming over an existing file with RENAME_NOREPLACE: %v", err)
	}
}

func TestReadOnlyFolder(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("a.txt", "a")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	m := mountGateway(t, g)
	if err := os.WriteFile(m.path("docs/new.txt"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("writing in a read-only folder: %v", err)
	}
	if err := unix.Access(m.path("docs/a.txt"), unix.W_OK); err == nil {
		t.Fatal("a file of a read-only folder is writable")
	}
	// Keeping works all the same
	if err := unix.Setxattr(m.path("docs/a.txt"), xattrKeep, []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
}

// daemon is the test executable serving a service in a process of its own, to be killed
type daemon struct {
	t      *testing.T
	dir    string
	settle string
	cmd    *exec.Cmd
}

func (d *daemon) start() {
	d.t.Helper()
	d.cmd = exec.Command(os.Args[0], "-test.run=^$")
	d.cmd.Env = append(os.Environ(), "SYNCTRAIN_TEST_DAEMON="+d.dir, "SYNCTRAIN_TEST_SETTLE="+d.settle)
	d.cmd.Stderr = os.Stderr
	if err := d.cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	gatewaytest.WaitFor(d.t, 30*time.Second, "the daemon to mount", func() bool {
		var st unix.Statfs_t
		return unix.Statfs(filepath.Join(d.dir, "mnt"), &st) == nil && st.Type == unix.FUSE_SUPER_MAGIC
	})
}

// kill kills the daemon as a crash would, and unmounts what it leaves
func (d *daemon) kill() {
	d.cmd.Process.Signal(syscall.SIGKILL)
	d.cmd.Wait()
	exec.Command("fusermount3", "-uz", filepath.Join(d.dir, "mnt")).Run()
}

func (d *daemon) stop() {
	d.cmd.Process.Signal(syscall.SIGTERM)
	d.cmd.Wait()
}

func TestKilledDaemon(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("open.txt", "before")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	dir := t.TempDir()
	// Paired and set up in this process, then served by the daemon
	s, err := files.Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	var gw files.Gateway
	go func() {
		var err error
		gw, err = s.Pair(ctx, g.PairingLink(), nil)
		done <- err
	}()
	g.ApproveNext(map[string]string{"docs": "write"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFolder(ctx, gw.ServiceID, "docs"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	mnt := filepath.Join(dir, "mnt", "docs")

	// Killed in the quiet period after an fsync: the file is sent after the restart
	d := &daemon{t: t, dir: dir, settle: "1h"}
	d.start()
	gatewaytest.WaitFor(t, 30*time.Second, "the folder", func() bool {
		_, err := os.Stat(filepath.Join(mnt, "open.txt"))
		return err == nil
	})
	f, err := os.Create(filepath.Join(mnt, "synced.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("fsynced")
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Rename(filepath.Join(mnt, "synced.txt"), filepath.Join(mnt, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	// Killed while a file is open for writing: its content may be torn, so it is kept for review
	w, err := os.OpenFile(filepath.Join(mnt, "open.txt"), os.O_RDWR|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("half")
	w.Sync()
	d.kill()
	w.Close()

	d.settle = "100ms"
	d.start()
	defer d.stop()
	gatewaytest.WaitFor(t, 30*time.Second, "the file written before the crash", func() bool {
		got, ok := e.Read("renamed.txt")
		return ok && got == "fsynced"
	})
	if _, ok := e.Read("synced.txt"); ok {
		t.Fatal("the file's old name reached the gateway")
	}
	buf := make([]byte, 16)
	n, err := unix.Getxattr(filepath.Join(mnt, "open.txt"), xattrState, buf)
	if err != nil || string(buf[:n]) != "blocked" {
		t.Fatalf("a file being written at the crash is %q (%v)", buf[:n], err)
	}
	if got, _ := e.Read("open.txt"); got != "before" {
		t.Fatalf("a torn write reached the gateway: %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(mnt, "open.txt")); err != nil || string(data) != "half" {
		t.Fatalf("it reads %q (%v)", data, err)
	}
}
