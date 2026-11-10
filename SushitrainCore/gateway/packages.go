// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"path"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

// PackageTypes tells which directories the host counts as packages: one document, like an Icon Composer icon or an
// Xcode project, rather than a folder to browse. On macOS these are the types that conform to com.apple.package,
// including those of third-party apps. Clients on systems that know fewer of them (iOS) show such directories as
// documents too.
//
// The service asks from its own goroutines, sometimes while it holds a folder's lock, so the answer has to come
// quickly: an implementation must not wait for the main thread or call back into the service.
type PackageTypes interface {
	// IsPackageExtension reports whether a directory whose name has this extension (without the dot, in lower case)
	// is a package
	IsPackageExtension(ext string) bool
}

const (
	// How long the host's answer for an extension is kept: listings ask for many directories, and a newly installed
	// app's types count once this has passed
	packageAnswerLifetime = time.Minute
	// Answers kept at most; names with many different "extensions" (dates, build numbers) only cost the host's time
	packageAnswersKept = 4096
	// Longer extensions name no type, and aren't asked about
	packageExtensionMaxLength = 64
)

type packageAnswer struct {
	isPackage bool
	at        time.Time
}

// packageCache keeps the host's answers per extension, which the host gives regardless of case
type packageCache struct {
	types PackageTypes
	now   func() time.Time

	mutex   sync.Mutex
	answers map[string]packageAnswer
}

func newPackageCache(types PackageTypes) *packageCache {
	if types == nil {
		return nil
	}
	return &packageCache{types: types, now: time.Now, answers: make(map[string]packageAnswer)}
}

// packageExtension is the extension of a name as macOS takes it (NSString.pathExtension), in lower case: none when the
// name's only dots lead it (".bundle"), or when the extension has a space
func packageExtension(name string) string {
	ext := strings.TrimPrefix(path.Ext(strings.TrimLeft(name, ".")), ".")
	if strings.Contains(ext, " ") {
		return ""
	}
	return strings.ToLower(ext)
}

// isPackage reports whether a directory with this name is a package. A nil cache (a host without package types)
// counts none.
func (c *packageCache) isPackage(name string) bool {
	if c == nil {
		return false
	}
	ext := packageExtension(name)
	if ext == "" || len(ext) > packageExtensionMaxLength || strings.Trim(ext, "0123456789") == "" {
		return false
	}
	now := c.now()
	c.mutex.Lock()
	answer, ok := c.answers[ext]
	c.mutex.Unlock()
	if ok && now.Sub(answer.at) < packageAnswerLifetime && !now.Before(answer.at) {
		return answer.isPackage
	}
	// Asked without the lock, so that a slow answer holds up no other listing; requests asking at the same time get
	// the same answer
	isPackage := c.types.IsPackageExtension(ext)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if len(c.answers) >= packageAnswersKept {
		for key, kept := range c.answers {
			if now.Sub(kept.at) >= packageAnswerLifetime || now.Before(kept.at) {
				delete(c.answers, key)
			}
		}
		if len(c.answers) >= packageAnswersKept {
			clear(c.answers)
		}
	}
	c.answers[ext] = packageAnswer{isPackage: isPackage, at: now}
	return isPackage
}

// itemOf is the item of a row as a client sees it. Every item the service sends goes through it.
func (s *Service) itemOf(r itemRow, canWrite bool) Item {
	item := r.baseItem(canWrite)
	if r.kind == protocol.FileInfoTypeDirectory && !r.deleted && r.path != "" {
		item.Package = s.packages.isPackage(item.Name)
	}
	return item
}
