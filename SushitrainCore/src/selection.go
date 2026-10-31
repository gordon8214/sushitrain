// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gobwas/glob"
	"golang.org/x/exp/slog"
	"t-shaped.nl/sushitrain/v2/gateway"
)

type selection struct {
	lines []string
}

// onDemandPattern marks a selective folder as on demand: the app shows all of it in Finder through the remote file
// service, which selects the paths it writes. The line also keeps conflict copies out of the last pattern: when a peer
// wins a conflict, the engine renames this device's edit to a conflict copy, which would otherwise be ignored and never
// reach other devices.
const onDemandPattern = "!*.sync-conflict-*"

func newSelection(lines []string) *selection {
	sel := &selection{
		lines: lines,
	}

	if sel.isSelectiveIgnore() {
		lines, err := cleanSelectiveSelection(sel.lines)
		if !sel.isSelectiveIgnore() || err != nil {
			panic("selective status changed after removeNestedSelections")
		}
		sel.lines = lines
	}

	return sel
}

func isCommentPattern(pattern string) bool {
	return len(pattern) == 0 || strings.HasPrefix(pattern, "//")
}

func lineSet(lines []string) map[string]struct{} {
	set := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		set[line] = struct{}{}
	}
	return set
}

// hasSelectedParent reports whether one of the selection lines selects line through a parent directory. Given a
// selection of "/a/some", "/a/some/thing" is selected by it, but "/a/something" and "/a/something/else" are not.
func hasSelectedParent(line string, selected map[string]struct{}) bool {
	for {
		// The slash at index 1 is the one of "!/"
		slash := strings.LastIndexByte(line, '/')
		if slash <= 1 {
			return false
		}
		line = line[:slash]
		if _, ok := selected[line]; ok {
			return true
		}
	}
}

func cleanSelectiveSelection(lines []string) ([]string, error) {
	result := make([]string, 0)

	// First, find all selection patterns
	selectionPatterns := make([]string, 0)

	for idx, line := range lines {
		if idx == len(lines)-1 && line == "*" {
			continue
		} else if line == onDemandPattern {
			// Put back below, after the global ignores
			continue
		} else if isSelectionPattern(line) {
			selectionPatterns = append(selectionPatterns, line)
		} else if isGlobalIgnorePattern(line) {
			result = append(result, line)
		} else if isCommentPattern(line) {
			// Throw these out
			continue
		} else {
			return nil, fmt.Errorf("invalid pattern: %s", line)
		}
	}

	slices.Sort(selectionPatterns)

	if slices.Contains(lines, onDemandPattern) {
		result = append(result, onDemandPattern)
	}

	// Write new ignore lines for selection patterns
	selected := lineSet(selectionPatterns)
	for _, line := range lines {
		if isSelectionPattern(line) {
			if hasSelectedParent(line, selected) {
				slog.Warn("selection contains a path that is also selected by a parent; removing", "path", line)
				continue
			}
			result = append(result, line)
		}
	}

	result = append(result, "*")
	return result, nil
}

func (sel *selection) setSelective(selective bool) error {
	isSelective := sel.isSelectiveIgnore()
	if selective == isSelective {
		slog.Warn("selection SetSelective called, but selectivity is already the desired value")
		// Nothing to be changed
		return nil
	}

	newLines := Filter(sel.lines, func(line string) bool {
		return isGlobalIgnorePattern(line)
	})

	if selective {
		// We're making an existing folder selective. We're going to copy over any existing old global ignores and append the "*" pattern
		newLines = append(newLines, "*")
	} else {
		// We're making a selective folder non-selective. Copy over any global ignores, but ignore nothing else
	}

	slog.Info("selection setSelective", "selective", selective, "newLines", newLines)

	sel.lines = newLines
	if sel.isSelectiveIgnore() != selective {
		panic(fmt.Sprintf("SetSelective failed (selective=%t)", selective))
	}

	return nil
}

// Returns whether the provided set of ignore lines are valid for 'selective' mode
func (sel *selection) isSelectiveIgnore() bool {
	if len(sel.lines) == 0 {
		return false
	}

	// At the beginning, we allow zero or more patterns that start with "(?d)" and do not contain "*" or "/" (global ignores)
	// Then, we allow zero or more patterns that start with "!/" and do not contain '*' (selection patterns)
	// The last pattern must be  '*'
	inSelectionPatterns := false
	for idx, pattern := range sel.lines {
		// The last line must be "*"
		if idx == len(sel.lines)-1 {
			if pattern != "*" {
				return false
			}
		} else {
			if isCommentPattern(pattern) {
				continue
			} else if pattern[0] == '!' {
				// Allow patterns that start with '!/' and disallow global ignore patterns from that point onwards
				// These patterns may not contain '*', apart from the line that marks an on-demand folder
				if !isSelectionPattern(pattern) && pattern != onDemandPattern {
					return false
				}
				inSelectionPatterns = true
			} else if isGlobalIgnorePattern(pattern) {
				if inSelectionPatterns {
					return false
				} else {
					continue
				}
			} else {
				return false
			}
		}
	}

	return true
}

func isSelectionPattern(pattern string) bool {
	return strings.HasPrefix(pattern, "!/") && !strings.Contains(pattern, "*")
}

func isGlobalIgnorePattern(pattern string) bool {
	return strings.HasPrefix(pattern, "(?d)") && !strings.Contains(pattern, "**") && !strings.Contains(pattern, "/")
}

func (sel *selection) globalIgnorePatterns() []string {
	patterns := make([]string, 0)
	for _, pattern := range sel.lines {
		if isGlobalIgnorePattern(pattern) {
			patterns = append(patterns, pathForIgnoreLine(pattern))
		}
	}
	return patterns
}

func (sel *selection) setGlobalIgnorePatterns(patterns []string) error {
	// Check whether the patterns supplied are valid global ignores
	for _, pattern := range patterns {
		if !isGlobalIgnorePattern(pattern) {
			return fmt.Errorf("pattern is not a valid global ignore pattern: '%s'", pattern)
		}
	}

	// Build new list of patterns; start with the new global ignores, then append the existing patterns *except* the
	// old global ignores
	newSel := make([]string, 0)
	newSel = append(newSel, patterns...)
	for _, pattern := range sel.lines {
		if !isGlobalIgnorePattern(pattern) {
			newSel = append(newSel, pattern)
		}
	}

	sel.lines = newSel
	return nil
}

func globPatternFromGlobalIgnorePattern(pattern string) string {
	return strings.TrimPrefix(pattern, "(?d)")
}

// Returns a set of globs that correspond to the global ignores in a selective folder.
func (sel *selection) globalIgnoreGlobs() ([]glob.Glob, error) {
	globs := make([]glob.Glob, 0)

	for _, pattern := range sel.lines {
		if isGlobalIgnorePattern(pattern) {
			globPattern := globPatternFromGlobalIgnorePattern(pattern)
			gl, err := glob.Compile(globPattern, '/')
			if err != nil {
				return nil, err
			}

			// Unrooted patterns in Syncthing will also match in subdirectories, therefore add a glob that also matches
			// these.
			glRecursive, err := glob.Compile("**/"+globPattern, '/')
			if err != nil {
				return nil, err
			}

			globs = append(globs, gl, glRecursive)
		}
	}

	return globs, nil
}

func (sel *selection) isGloballyIgnored(path string) (bool, error) {
	for _, pattern := range sel.lines {
		if isGlobalIgnorePattern(pattern) {
			globPattern := globPatternFromGlobalIgnorePattern(pattern)
			gl, err := glob.Compile(globPattern, '/')
			if err != nil {
				return false, err
			}
			if gl.Match(path) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (sel *selection) patterns() []string {
	return sel.lines
}

func (sel *selection) setExplicitlySelected(paths map[string]bool) error {
	// This requires a (valid) selective ignore file
	if !sel.isSelectiveIgnore() {
		return fmt.Errorf("ignore file is not valid for selective sync")
	}

	globalIgnoreGlobs, err := sel.globalIgnoreGlobs()
	if err != nil {
		return fmt.Errorf("ignore file contains invalid global ignores: %w", err)
	}

	newLines := sel.lines

	for path, selectPath := range paths {
		line := ignoreLineForSelectingPath(path)
		slog.Info("Edit ignore line", "selected", selectPath, "line", line)

		// What the remote file service wrote stays selected until it knows that other devices have it, or its changes
		// would stop reaching them
		if !selectPath && sel.isOnDemand() {
			return errOnDemandSelection
		}

		// Is ths entry ignored and do we now want to select it? Then deny
		if selectPath {
			for _, globalIgnoreGlob := range globalIgnoreGlobs {
				if globalIgnoreGlob.Match(path) {
					slog.Warn("cannot select path because it is globally ignored", "path", path, "glob", globalIgnoreGlob)
					return fmt.Errorf("cannot select path '%s', because it is globally ignored", path)
				}
			}
		}

		// Is this entry currently selected explicitly?
		currentlySelectedExplicitly := slices.Contains(newLines, line)
		if currentlySelectedExplicitly == selectPath {
			// not changing selecting status for path, it is the status quo
			continue
		}

		// Is this entry currently selected implicitly? (i.e. due to a parent path being selected)
		if hasSelectedParent(line, lineSet(newLines)) {
			return fmt.Errorf("cannot change selection: the path '%s' is already implicitly selected", path)
		}

		// Is this entry a prefix of another explicitly selected entry? Then refuse changes
		prefixLine := line + "/"
		childrenSelectedImplicitly := slices.ContainsFunc(newLines, func(existingLine string) bool {
			return existingLine != prefixLine && strings.HasPrefix(existingLine, prefixLine)
		})

		if childrenSelectedImplicitly {
			matches := Filter(newLines, func(existingLine string) bool {
				return existingLine != line && strings.HasPrefix(existingLine, line)
			})
			slog.Info("selecting", "path", path, "line", line, "matchesExisting", matches)

			return fmt.Errorf("cannot change selection: an item in the subdirectory '%s' is already selected", path)
		}

		// To deselect, remove the relevant ignore line
		countBefore := len(newLines)
		if !selectPath {
			newLines = Filter(newLines, func(l string) bool {
				return l != line
			})
			if len(newLines) != countBefore-1 {
				return fmt.Errorf("failed to remove ignore line '%s'", line)
			}
		} else {
			// To select, append it (but before the last '*')
			newLines = append(newLines[:len(newLines)-1], line, "*")
		}
	}

	sel.lines = newLines

	// We should end up with a valid ignore file
	if !sel.isSelectiveIgnore() {
		panic("ignore file is not selective anymore after SetExplicitlySelected")
	}

	return nil
}

func (sel *selection) isEntryExplicitlySelected(entry *Entry) bool {
	path := entry.info.FileName()
	return sel.isPathExplicitlySelected(path)
}

func (sel *selection) isPathExplicitlySelected(path string) bool {
	ignoreLine := ignoreLineForSelectingPath(path)

	for _, line := range sel.lines {
		if len(line) > 0 && line[0] == '!' {
			if line == ignoreLine {
				return true
			}
		}
	}

	return false
}

func (sel *selection) selectedPaths() []string {
	paths := make([]string, 0)
	for _, pattern := range sel.lines {
		if len(pattern) > 0 && pattern[0] == '!' && pattern != onDemandPattern {
			paths = append(paths, pathForIgnoreLine(pattern))
		}
	}
	return paths
}

func (sel *selection) filterSelectedPaths(retain func(string) bool) {
	newLines := make([]string, 0)

	for _, pattern := range sel.lines {
		// Filter patterns that start with '!', these are selection patterns
		if len(pattern) > 0 && pattern[0] == '!' && pattern != onDemandPattern {
			if retain(pathForIgnoreLine(pattern)) {
				newLines = append(newLines, pattern)
			}
		} else {
			// Allow any other pattern to remain
			newLines = append(newLines, pattern)
		}
	}
	sel.lines = newLines
}

// isOnDemand reports whether this is the selection of an on-demand folder
func (sel *selection) isOnDemand() bool {
	return sel.isSelectiveIgnore() && slices.Contains(sel.lines, onDemandPattern)
}

// setOnDemand marks a selective folder as on demand, keeping what it has selected
func (sel *selection) setOnDemand() error {
	if !sel.isSelectiveIgnore() {
		return errors.New("folder is not a selective folder")
	}
	if sel.isOnDemand() {
		return nil
	}
	lines, err := cleanSelectiveSelection(append([]string{onDemandPattern}, sel.lines...))
	if err != nil {
		return err
	}
	sel.lines = lines
	return nil
}

// isKept reports whether path is selected, by itself or through a parent
func (sel *selection) isKept(path string) bool {
	line := ignoreLineForSelectingPath(path)
	lines := lineSet(sel.lines)
	_, selected := lines[line]
	return selected || hasSelectedParent(line, lines)
}

// keep selects paths the remote file service wrote in an on-demand folder, whatever is selected around them: unlike
// setExplicitlySelected, a selected parent or child is fine, and a directory takes over the lines of what is below it.
// It returns the paths it selected, leaving out those that already were.
func (sel *selection) keep(paths []string) ([]string, error) {
	if !sel.isOnDemand() {
		return nil, errors.New("folder is not an on-demand folder")
	}
	var added []string
	for _, path := range paths {
		if sel.isKept(path) {
			continue
		}
		line := ignoreLineForSelectingPath(path)
		if !gateway.SelectablePath(path) || !isSelectionPattern(line) {
			return nil, fmt.Errorf("the path '%s' cannot be selected", path)
		}
		sel.lines = append(sel.lines[:len(sel.lines)-1], line, "*")
		added = append(added, path)
	}
	if len(added) > 0 {
		selected := lineSet(sel.lines)
		sel.lines = Filter(sel.lines, func(line string) bool { return !hasSelectedParent(line, selected) })
	}
	return added, nil
}

// drop deselects paths of an on-demand folder, and reports whether the selection changed. What a parent selects stays
// selected.
func (sel *selection) drop(paths []string) bool {
	dropped := lineSet(Map(paths, ignoreLineForSelectingPath))
	kept := Filter(sel.lines, func(line string) bool {
		_, ok := dropped[line]
		return !ok
	})
	changed := len(kept) != len(sel.lines)
	sel.lines = kept
	return changed
}

// Escape special characters: https://docs.syncthing.net/users/ignoring.html
var specialChars = []string{"\\", "!", "*", "?", "[", "]", "{", "}"}

// Generate a line for use in the .stignore file that selects the file at `path`. The path should *not* start with a slash.
func ignoreLineForSelectingPath(path string) string {
	for _, sp := range specialChars {
		path = strings.ReplaceAll(path, sp, "\\"+sp)
	}
	return "!/" + path
}

func pathForIgnoreLine(line string) string {
	line = strings.TrimPrefix(line, "!/")
	for _, sp := range specialChars {
		line = strings.ReplaceAll(line, "\\"+sp, sp)
	}
	return line
}
