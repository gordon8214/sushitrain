//go:build !windows

package sushitrain

import (
	"errors"
	"syscall"
)

func auditDiskPath(string) bool { return true }

func nativeFolderComponent(string) bool { return true }

func addressInUse(err error) bool         { return errors.Is(err, syscall.EADDRINUSE) }
func nativeFolderName(name string) string { return name }
func securePrivateFile(string) error      { return nil }
func secureLogDirectory(string) error     { return nil }
