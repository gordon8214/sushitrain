// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

// PreviewFile admits and copies the same selected content snapshot.
func (s *Service) PreviewFile(ctx context.Context, item int64, node uint64, limit int64, cache string) (string, error) {
	entry, ok, err := s.Item(item)
	if err != nil {
		return "", err
	}
	if !ok || entry.Node != node {
		return "", ErrVersionChanged
	}
	reader, err := s.OpenNode(entry)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if limit < 1 || limit > previewcache.MaxFile || reader.Size() > limit {
		return "", errors.New("this file exceeds the preview size limit")
	}
	if reader.Size() != entry.Size || reader.ContentID() != "" && reader.ContentID() != entry.ContentID {
		return "", ErrVersionChanged
	}
	if err = os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	release, err := previewcache.Reserve(ctx, cache, reader.Size())
	if err != nil {
		return "", err
	}
	defer release()
	dir, err := os.MkdirTemp(cache, "preview-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	file := filepath.Join(dir, entry.Name)
	dest, err := safecopy.Create(file)
	if err != nil {
		return "", err
	}
	defer dest.Close()
	buf := make([]byte, 128*1024)
	for off := int64(0); off < reader.Size(); {
		n, readErr := reader.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, err = dest.File.Write(buf[:n]); err != nil {
				return "", err
			}
			off += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		if n == 0 {
			return "", io.ErrUnexpectedEOF
		}
	}
	current, ok, err := s.Item(item)
	if err != nil {
		return "", err
	}
	if !ok || current.Node != entry.Node || current.ContentID != entry.ContentID || current.Size != entry.Size || !current.ModTime.Equal(entry.ModTime) {
		return "", ErrVersionChanged
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = dest.Commit(); err != nil {
		return "", err
	}
	success = true
	return file, nil
}
