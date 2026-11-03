//go:build !windows

package sthome

import "os"

func syncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func privateImportedHome(path string) error { return os.Chmod(path, 0o700) }
