package sushitrain

import (
	"errors"
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

// Reject device namespaces and alternate streams before attempting an open.
func auditDiskPath(path string) bool {
	lower := strings.ToLower(path)
	if strings.HasPrefix(lower, `\\.\`) || strings.HasPrefix(lower, `\\?\globalroot\`) || strings.HasPrefix(lower, `\\?\pipe\`) {
		return false
	}
	volume := filepath.VolumeName(path)
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(path, volume), func(c rune) bool { return c == '\\' || c == '/' }) {
		if !winplatform.ValidComponent(part) {
			return false
		}
	}
	return true
}

func nativeFolderComponent(name string) bool { return winplatform.ValidComponent(name) }

func addressInUse(err error) bool          { return errors.Is(err, windows.WSAEADDRINUSE) }
func nativeFolderName(name string) string  { return strings.TrimRight(name, ". ") }
func securePrivateFile(path string) error  { return winplatform.PrivateFile(path) }
func secureLogDirectory(path string) error { return winplatform.PrivateDirectory(path) }
