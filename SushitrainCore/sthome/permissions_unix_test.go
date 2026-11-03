//go:build !windows

package sthome

import (
	"os"
	"testing"
)

func makeUnreadable(t *testing.T, path string) { t.Helper(); must(t, os.Chmod(path, 0)) }

func assertPrivateKey(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", info.Mode().Perm())
	}
}
