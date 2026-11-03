//go:build !windows

package sushitrain

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// guiForSettings is a GUI that is on, with a password, on an address that nothing else uses
func TestGUISettingsCheckASocket(t *testing.T) {
	dir := socketDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	gui := guiForSettings(t)
	for _, address := range []string{file, filepath.Join(dir, "missing", "gui.sock")} {
		if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = address }); err == nil {
			t.Errorf("%s was accepted", address)
		}
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "precious" {
		t.Fatalf("the file is gone: %q (%v)", data, err)
	}

	socket := filepath.Join(dir, "gui.sock")
	moved, _, err := applyGUISettings(gui, func(s *GUISettings) { s.Address = socket })
	if err != nil || moved.Network() != "unix" {
		t.Fatalf("%s (%v)", moved.Network(), err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check left its socket: %v", err)
	}

	// The socket it has is not in the way of itself
	gui.RawAddress = file
	if _, _, err := applyGUISettings(gui, func(s *GUISettings) { s.UseTLS = true }); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
}

func TestASocketThatCannotBeUsedTurnsTheGUIOff(t *testing.T) {
	dir := socketDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{file, filepath.Join(dir, "missing", "gui.sock")} {
		path := writeGUIConfig(t, t.TempDir(), true, address)
		if gui, notice := loadGUI(t, path, false); gui.Enabled || notice == "" {
			t.Errorf("%s: GUI on %v (%q)", address, gui.Enabled, notice)
		}
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "precious" {
		t.Fatalf("the file is gone: %q (%v)", data, err)
	}
}

func TestASocketAnEarlierRunLeftIsUsedAgain(t *testing.T) {
	socket := filepath.Join(socketDir(t), "gui.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()

	path := writeGUIConfig(t, t.TempDir(), true, socket)
	if gui, notice := loadGUI(t, path, false); !gui.Enabled || notice != "" {
		t.Fatalf("GUI on %v (%q)", gui.Enabled, notice)
	}
}
