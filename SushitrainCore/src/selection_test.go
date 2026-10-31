package sushitrain

import (
	"errors"
	"slices"
	"testing"
)

func TestRemoveNested(t *testing.T) {
	beforeAfter := [][][]string{
		[][]string{
			[]string{"!/a", "!/a/b", "*"}, []string{"!/a", "*"},
			[]string{"!/a", "!/a", "*"}, []string{"!/a", "*"},
			[]string{"!/a", "!/a", "!/a/b", "*"}, []string{"!/a", "*"},
			[]string{"(?d).DS_Store", "!/a", "!/a", "!/a/b", "*"}, []string{"!/a", "*"},
		},
	}

	for _, ba := range beforeAfter {
		sel := newSelection(ba[0])
		if !slices.Equal(sel.lines, ba[1]) {
			t.Errorf("mismatch: %s %s", sel.lines, ba[1])
		}
	}
}

func TestIsSelective(t *testing.T) {
	goodFiles := [][]string{
		[]string{"*"},
		[]string{"!/a", "*"},
		[]string{"!/a", "", "!/b", "*"},
		[]string{"(?d).DS_Store", "!/2016-02-13", "!/b", "*"},
		[]string{"(?d).DS_Store", "!/2016-02-13", "!/b", "!/b/c", "*"},
		[]string{"(?d).DS_Store", "!/2016-02-13", "!/b", "// some comment in between", "!/b/c", "*"},
	}

	for _, file := range goodFiles {
		if !newSelection(file).isSelectiveIgnore() {
			t.Errorf("file is not selective ignore: %s", file)
		}
	}

	badFiles := [][]string{
		[]string{""},
		[]string{"!a", "*"},
		[]string{"!/a", "*", "// last line cant be a comment"},
		[]string{"!/a", "", "b", "*"},
		[]string{"(?d).DS_Store", "!/2016-02-13", "(?d)*.txt", "!/b", "*"},
		[]string{"(?d).DS_Store", "!/2016-02-13", "(?d)**s/*.txt", "!/b", "*"},
	}

	for _, file := range badFiles {
		if newSelection(file).isSelectiveIgnore() {
			t.Errorf("file is selective ignore: %s", file)
		}
	}
}

func TestChanges(t *testing.T) {
	lines := []string{"(?d).DS_Store", "(?d)*.json", "(?d)*.json", "!/a/b", "*"}

	sel := newSelection(lines)
	if !sel.isSelectiveIgnore() {
		t.Errorf("file is not selective ignore but it should be")
	}

	// Remove invalid file selection and check if we are still selective
	sel.setExplicitlySelected(map[string]bool{
		"!/x/y/z": false,
	})

	if !sel.isSelectiveIgnore() {
		t.Errorf("file is not selective ignore after change 1 but it should be")
	}

	// Remove file selection and check if we are still selective
	sel.setExplicitlySelected(map[string]bool{
		"!/a/b": false,
	})

	if !sel.isSelectiveIgnore() {
		t.Errorf("file is not selective ignore after change 2 but it should be")
	}

	// Add a file selection and check if we are still selective
	sel.setExplicitlySelected(map[string]bool{
		"!/q/w/e/r": true,
	})

	if !sel.isSelectiveIgnore() {
		t.Errorf("file is not selective ignore after change 3 but it should be")
	}

	// Set global patterns and check if we are still selective
	err := sel.setGlobalIgnorePatterns([]string{"(?d)*.json", "(?d)*.txt", "(?d).DS_Store"})
	if err != nil {
		t.Errorf("SetGlobalIgnorePatterns failed: %e", err)
	}

	if !sel.isSelectiveIgnore() {
		t.Errorf("file is not selective ignore after change 4 but it should be")
	}
}

func TestOnDemandSelection(t *testing.T) {
	sel := newSelection([]string{"(?d).DS_Store", "!/a", "*"})
	if sel.isOnDemand() {
		t.Fatal("a plain selective folder is not on demand")
	}
	if err := sel.setOnDemand(); err != nil {
		t.Fatal(err)
	}
	want := []string{"(?d).DS_Store", onDemandPattern, "!/a", "*"}
	if !slices.Equal(sel.lines, want) || !sel.isOnDemand() || !sel.isSelectiveIgnore() {
		t.Fatalf("lines %q", sel.lines)
	}
	// It stays through a load, which cleans a selection, and isn't a selected path
	sel = newSelection(sel.lines)
	if !slices.Equal(sel.lines, want) {
		t.Fatalf("lines after loading %q", sel.lines)
	}
	if paths := sel.selectedPaths(); !slices.Equal(paths, []string{"a"}) {
		t.Fatalf("selected paths %q", paths)
	}
	sel.filterSelectedPaths(func(string) bool { return false })
	if !sel.isOnDemand() {
		t.Fatalf("filtering paths dropped the marker: %q", sel.lines)
	}
	if err := (&selection{lines: []string{"*"}}).setOnDemand(); err != nil {
		t.Fatal(err)
	}
	if err := (&selection{lines: []string{"foo"}}).setOnDemand(); err == nil {
		t.Fatal("a folder that isn't selective became on demand")
	}
}

func TestKeepSelected(t *testing.T) {
	sel := newSelection([]string{onDemandPattern, "!/selected", "*"})

	// Selected already, by itself or through a parent: nothing to save
	for _, path := range []string{"selected", "selected/child/file"} {
		if changed, err := sel.keep([]string{path}); changed || err != nil {
			t.Fatalf("%s: %v %v", path, changed, err)
		}
	}
	// Not through a name that only starts the same, also below it
	if changed, err := sel.keep([]string{"selected2/file"}); !changed || err != nil {
		t.Fatalf("selected2/file: %v %v", changed, err)
	}
	if changed, err := sel.keep([]string{"selected2"}); !changed || err != nil {
		t.Fatalf("selected2: %v %v", changed, err)
	}
	// A directory with something selected in it, which setExplicitlySelected refuses
	if _, err := sel.keep([]string{"dir/inside"}); err != nil {
		t.Fatal(err)
	}
	if changed, err := sel.keep([]string{"dir"}); !changed || err != nil {
		t.Fatalf("dir: %v %v", changed, err)
	}
	if sel.lines[len(sel.lines)-1] != "*" || !sel.isOnDemand() {
		t.Fatalf("lines %q", sel.lines)
	}
	if !sel.isKept("dir/inside") || !sel.isKept("selected2") {
		t.Fatalf("lines %q", sel.lines)
	}
	// Names with what the pattern language means something by are escaped
	if _, err := sel.keep([]string{"odd [name]!.txt"}); err != nil {
		t.Fatal(err)
	}
	if !sel.isKept("odd [name]!.txt") {
		t.Fatalf("lines %q", sel.lines)
	}
	if !newSelection(sel.lines).isOnDemand() {
		t.Fatalf("not on demand after loading: %q", sel.lines)
	}

	for _, path := range []string{"star*.txt", "line\nbreak", "ends in a space "} {
		before := slices.Clone(sel.lines)
		if _, err := sel.keep([]string{path}); err == nil {
			t.Fatalf("selected %q", path)
		}
		if !slices.Equal(before, sel.lines) {
			t.Fatalf("%q changed the selection: %q", path, sel.lines)
		}
	}

	plain := newSelection([]string{"!/a", "*"})
	if _, err := plain.keep([]string{"b"}); err == nil {
		t.Fatal("kept a path in a plain selective folder")
	}
}

func TestSelectionOfNamesThatStartTheSame(t *testing.T) {
	lines := []string{"!/Project", "!/Project Archive/a.txt", "!/Project/inside", "*"}
	want := []string{"!/Project", "!/Project Archive/a.txt", "*"}
	sel := newSelection(lines)
	if !slices.Equal(sel.lines, want) {
		t.Fatalf("lines %q", sel.lines)
	}
	if !sel.isKept("Project/inside") || sel.isKept("Project Archive") || sel.isKept("Project Archive/b.txt") {
		t.Fatalf("kept by the wrong lines: %q", sel.lines)
	}
	if err := sel.setExplicitlySelected(map[string]bool{"Project Archive/b.txt": true}); err != nil {
		t.Fatalf("selecting beside a name that starts the same: %v", err)
	}
	if err := sel.setExplicitlySelected(map[string]bool{"Project/other": true}); err == nil {
		t.Fatal("selected what a parent selects")
	}
}

func TestOnDemandRefusesDeselecting(t *testing.T) {
	sel := newSelection([]string{onDemandPattern, "!/a", "*"})
	if err := sel.setExplicitlySelected(map[string]bool{"a": false}); !errors.Is(err, errOnDemandSelection) {
		t.Fatalf("deselecting: %v", err)
	}
	if err := sel.setExplicitlySelected(map[string]bool{"b": true}); err != nil {
		t.Fatalf("selecting: %v", err)
	}
}
