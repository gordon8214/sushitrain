//go:build !windows

package sushitrain

import (
	"path/filepath"
	"syscall"
	"testing"
)

func auditPipeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
