// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"encoding/json"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/winplatform"
	"time"
)

type recoveredCapture struct {
	Name, Path string
	Size       int64
	Modified   time.Time
}

func (h *Host) recoveredCaptures() ([]recoveredCapture, error) {
	entries, err := os.ReadDir(filepath.Join(h.State, "Captured"))
	if err != nil {
		return nil, err
	}
	var records []struct {
		Path    string
		Capture struct{ Captured string }
	}
	if data, err := os.ReadFile(filepath.Join(h.State, "projection.json")); err == nil {
		if err := json.Unmarshal(data, &records); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	paths := map[string]string{}
	for _, record := range records {
		paths[record.Capture.Captured] = record.Path
	}
	result := []recoveredCapture{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		result = append(result, recoveredCapture{entry.Name(), paths[entry.Name()], info.Size(), info.ModTime()})
	}
	return result, nil
}

// Resolve the destination parent before checking containment, and use that
// resolved path for the write so an outside junction cannot redirect the copy.
func (h *Host) copyDestination(destination string) (string, error) {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	parent, err := canonicalDirectory(filepath.Dir(destination))
	if err != nil {
		return "", err
	}
	cloud, err := canonicalDirectory(h.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		if relative, err := filepath.Rel(strings.ToLower(cloud), strings.ToLower(parent)); err == nil && filepath.IsLocal(relative) {
			return "", errors.New("save or export outside the Synctrain Explorer location")
		}
	}
	return filepath.Join(parent, filepath.Base(destination)), nil
}

// Go's EvalSymlinks leaves NTFS directory junctions unresolved. Ask Windows for
// the final directory name through a handle that follows those reparse points.
func canonicalDirectory(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	file, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(file)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(file, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", errors.New("the copy destination parent is not a directory")
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(file, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if length >= uint32(len(buffer)) {
		return "", errors.New("the copy destination path is too long")
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

// Export an immutable retained capture; the captured source is never removed by export.
func (h *Host) exportCapture(name, destination string) error {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `\/:`) {
		return errors.New("invalid capture name")
	}
	destination, err := h.copyDestination(destination)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Join(h.State, "Captured"))
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("capture is not a regular file")
	}
	selected, err := root.Open(name)
	if err != nil {
		return err
	}
	input, err := winplatform.SnapshotReader(selected)
	selected.Close()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		output.Close()
		if !complete {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := winplatform.FlushDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	complete = true
	return nil
}
