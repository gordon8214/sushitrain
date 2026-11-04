// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package previewcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sparsePreview(t *testing.T, root, name string, size int64, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	when := time.Now().Add(-age)
	if err = os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestQuotaRetainsRecentPreviews(t *testing.T) {
	root := t.TempDir()
	old := sparsePreview(t, root, "old", MaxFile, 10*time.Minute)
	recent := sparsePreview(t, root, "recent", MaxFile, time.Minute)
	if err := Ensure(root, MaxFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old preview not evicted", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("recent preview evicted", err)
	}
	sparsePreview(t, root, "another", MaxFile, time.Minute)
	if err := Ensure(root, 1); err == nil {
		t.Fatal("cache exceeded quota by evicting an active preview")
	}
}
func TestExpiryDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	os.WriteFile(keep, []byte("data"), 0600)
	os.Symlink(outside, filepath.Join(root, "alias"))
	expired := sparsePreview(t, root, "expired", 10, 25*time.Hour)
	if err := Ensure(root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatal("expired preview retained", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("cache followed external symlink", err)
	}
	if err := Ensure(root, MaxFile+1); err == nil {
		t.Fatal("oversized preview accepted")
	}
}

func TestOpenPreviewCannotBeEvicted(t *testing.T) {
	root := t.TempDir()
	active := sparsePreview(t, root, "active", MaxFile, 10*time.Minute)
	release, err := Pin(active)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-10 * time.Minute)
	os.Chtimes(active, when, when)
	sparsePreview(t, root, "recent", MaxFile, time.Minute)
	if err = Ensure(root, 1); err == nil {
		t.Fatal("evicted an active preview")
	}
	release()
	if err = Ensure(root, 1); err != nil {
		t.Fatal("released preview cannot be evicted", err)
	}
	if _, err = os.Stat(active); !os.IsNotExist(err) {
		t.Fatal("released preview retained", err)
	}
}

func TestWaitingReservationCanBeCanceled(t *testing.T) {
	root := t.TempDir()
	release, err := Reserve(t.Context(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if second, err := Reserve(ctx, root, 1); !errors.Is(err, context.DeadlineExceeded) {
		if second != nil {
			second()
		}
		t.Fatal("quota wait ignored cancellation", err)
	}
}
