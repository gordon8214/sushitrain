package sushitrain

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// Inject access failures independently of the host user's filesystem privileges.
type ignoreAccessFilesystem struct {
	fs.Filesystem
	rootErr error
	openErr error
	statErr error
}

func (f *ignoreAccessFilesystem) DirNames(name string) ([]string, error) {
	if f.rootErr != nil {
		return nil, f.rootErr
	}
	return f.Filesystem.DirNames(name)
}
func (f *ignoreAccessFilesystem) Lstat(name string) (fs.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return f.Filesystem.Lstat(name)
}
func (f *ignoreAccessFilesystem) Open(name string) (fs.File, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.Filesystem.Open(name)
}
func (f *ignoreAccessFilesystem) OpenFile(name string, flags int, mode fs.FileMode) (fs.File, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.Filesystem.OpenFile(name, flags, mode)
}

func TestLoadIgnoreMatcherAccess(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		contents                   string
		missingRoot, missingIgnore bool
		rootErr, openErr, statErr  error
		wantErr                    bool
		lines                      []string
	}{
		{name: "readable root without ignore file", missingIgnore: true},
		{name: "ignores everything", contents: "!/keep\n*\n", lines: []string{"!/keep", "*"}},
		{name: "ordinary patterns", contents: "*.tmp\n", lines: []string{"*.tmp"}},
		{name: "missing root", missingRoot: true, missingIgnore: true, wantErr: true},
		{name: "root denied with absent ignore", missingIgnore: true, rootErr: os.ErrPermission, wantErr: true},
		{name: "ignore denied", contents: "*\n", openErr: os.ErrPermission, wantErr: true},
		{name: "metadata denied", statErr: os.ErrPermission, wantErr: true},
		{name: "missing include", contents: "#include missing\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.missingRoot {
				root = filepath.Join(root, "unmounted")
			}
			if !tc.missingIgnore {
				if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(tc.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ffs := &ignoreAccessFilesystem{Filesystem: fs.NewFilesystem(fs.FilesystemTypeBasic, root), rootErr: tc.rootErr, openErr: tc.openErr, statErr: tc.statErr}
			matcher, err := loadIgnoreMatcher(ffs, &CachedIgnore{})
			if tc.wantErr {
				if err == nil || matcher != nil {
					t.Fatalf("expected unavailable matcher, got %v, %v", matcher, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := matcher.Lines(); !slices.Equal(got, tc.lines) {
				t.Fatalf("lines = %q, want %q", got, tc.lines)
			}
		})
	}
}

func TestLoadIgnoreMatcherPermissionRecovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte("*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ffs := &ignoreAccessFilesystem{Filesystem: fs.NewFilesystem(fs.FilesystemTypeBasic, root)}
	cache := &CachedIgnore{}
	if _, err := loadIgnoreMatcher(ffs, cache); err != nil {
		t.Fatal(err)
	}
	// Metadata and timestamps still work, but opening the cached file no longer does.
	ffs.openErr = os.ErrPermission
	if matcher, err := loadIgnoreMatcher(ffs, cache); !errors.Is(err, os.ErrPermission) || matcher != nil {
		t.Fatalf("expected permission error, got %v, %v", matcher, err)
	}
	ffs.openErr = nil
	matcher, err := loadIgnoreMatcher(ffs, cache)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(matcher.Lines(), []string{"*"}) {
		t.Fatalf("the patterns did not recover: %q", matcher.Lines())
	}
	if err := os.Remove(filepath.Join(root, ignoreFileName)); err != nil {
		t.Fatal(err)
	}
	matcher, err = loadIgnoreMatcher(ffs, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(matcher.Lines()) != 0 || cache.matcher != nil {
		t.Fatal("missing ignore file retained stale patterns")
	}
}

// FolderWithID returns a new Folder for every call
func TestIgnoresAreCachedPerFolderUntilTheFileChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ignoreFileName)
	if err := os.WriteFile(path, []byte("!/a\n*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Folders = []config.FolderConfiguration{{ID: "test", Path: root, FilesystemType: config.FilesystemTypeBasic}}
	client := (&Client{}).withTestRun(config.Wrap("", cfg, protocol.EmptyDeviceID, events.NoopLogger),
		&syncthing.App{Internals: &syncthing.Internals{}}, nil)
	first, err := (&Folder{client: client, FolderID: "test"}).loadIgnores()
	if err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: client, FolderID: "test"}
	second, err := folder.loadIgnores()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the patterns were read again")
	}
	if err := os.WriteFile(path, []byte("!/b\n*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	third, err := folder.loadIgnores()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(third.Lines(), []string{"!/b", "*"}) {
		t.Fatalf("a new file: %q", third.Lines())
	}
	folder.forgetIgnores()
	if fourth, err := folder.loadIgnores(); fourth == third || err != nil {
		t.Fatalf("forgotten patterns came back: %v", err)
	}
}

func TestIsSelectiveWithoutARunningEngine(t *testing.T) {
	if mode, err := (&Folder{}).IsSelective(); mode != false || err == nil {
		t.Fatalf("no client: got %v, %v", mode, err)
	}
	if mode, err := (&Folder{client: &Client{}}).IsSelective(); mode != false || err == nil {
		t.Fatalf("not loaded: got %v, %v", mode, err)
	}
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Folders = []config.FolderConfiguration{{ID: "test", Path: t.TempDir(), FilesystemType: config.FilesystemTypeBasic}}
	client := (&Client{}).withTestRun(config.Wrap("", cfg, protocol.EmptyDeviceID, events.NoopLogger), nil, nil)
	if mode, err := (&Folder{client: client, FolderID: "test"}).IsSelective(); mode != false || err == nil {
		t.Fatalf("not started: got %v, %v", mode, err)
	}
}
