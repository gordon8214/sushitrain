// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
)

// The advanced configuration editor is the counterpart of the web GUI's Advanced dialog. Its fields come from the
// configuration's structs by reflection, under the names they have in the REST API, so that a field a new version of
// the fork adds shows up by itself. Only the few lists below name fields.

// The sections of the configuration the editor works on. A folder's or device's has its ID after the prefix.
const (
	AdvancedSectionOptions        = "options"
	AdvancedSectionGUI            = "gui"
	AdvancedSectionLDAP           = "ldap"
	AdvancedSectionDefaultFolder  = "defaults/folder"
	AdvancedSectionDefaultDevice  = "defaults/device"
	AdvancedSectionDefaultIgnores = "defaults/ignores"
	AdvancedSectionFolderPrefix   = "folder/"
	AdvancedSectionDevicePrefix   = "device/"
)

// How a field's value is edited, and what its text looks like
const (
	AdvancedKindBool   = "bool"   // "true" or "false"
	AdvancedKindInt    = "int"    // a whole number
	AdvancedKindFloat  = "float"  // a number
	AdvancedKindString = "string" // text
	AdvancedKindEnum   = "enum"   // one of the field's options
	AdvancedKindList   = "list"   // one item per line
	AdvancedKindJSON   = "json"   // anything else, in the REST API's JSON
)

const advancedJSONIndent = "  "

var (
	ErrAdvancedSectionNotFound = errors.New("this folder or device no longer exists")
	errAdvancedNotBool         = errors.New("enter true or false")
	errAdvancedNotWholeNumber  = errors.New("enter a whole number")
	errAdvancedNotNumber       = errors.New("enter a number")
	errAdvancedNotOption       = errors.New("choose one of the options")
	errAdvancedTooLong         = errors.New("this is too long a time")
)

// Fields that are shown but can't be changed here. A new ID makes another folder or device, and the app sets whether a
// device is paused from its own list (AppState.updateDeviceSuspension). Fields the engine keeps empty in defaults (tagged
// nodefault) can't be changed there either.
var advancedReadOnlyFields = []string{"folder.id", "device.deviceID", AdvancedSectionDevicePrefix + ".paused"}

// Fields that are hidden on iOS: the app sets them itself, at every start (applyAppDefaults, applyIOSFolderSettings) or
// as it goes to the background, or they do nothing there. Folders stay in the app's container, whose path changes, the
// app can't change who owns a file and writes no audit log there, and only the web GUI uses its notifications. The GUI
// and LDAP sections are unavailable altogether.
var advancedFieldsHiddenOnIOS = []string{
	"options.auditEnabled",
	"options.auditFile",
	"options.autoUpgradeIntervalH",
	"options.crURL",
	"options.crashReportingEnabled",
	"options.progressUpdateIntervalS",
	"options.reconnectionIntervalS",
	"options.relayReconnectIntervalM",
	"options.releasesURL",
	"options.startBrowser",
	"options.unackedNotificationIDs",
	"options.upgradeToPreReleases",
	"options.urAccepted",
	"options.urInitialDelayS",
	"options.urSeen",
	"options.urURL",
	"options.urUniqueId",
	"folder.copyOwnershipFromParent",
	"folder.junctionsAsDirs",
	"folder.path",
	"folder.scanProgressIntervalS",
	"folder.sendOwnership",
	"folder.syncOwnership",
	AdvancedSectionDefaultFolder + ".blockIndexing",
	AdvancedSectionDefaultFolder + ".fsWatcherEnabled",
	AdvancedSectionDefaultFolder + ".ignorePerms",
	AdvancedSectionDefaultFolder + ".rescanIntervalS",
}

// AdvancedField is one field of a section, with its current value
type AdvancedField struct {
	// Where the field is in its section, as in the REST API's JSON, such as versioning.fsType
	Path string
	// The field's name for people, generated from its name in the configuration, in English as the names are
	Label string
	// The name for people of the struct in the section that has the field; empty when the section itself has it
	Group string
	// One of the AdvancedKind constants
	Kind string
	// The value as text (see the AdvancedKind constants). Change it to have SaveAdvancedSection change the field.
	Value string
	// A password or key. Its value is never shown, even hashed: Value starts empty, and stays empty to keep it.
	IsSecret   bool
	IsReadOnly bool

	options []string
	saved   string
	// Where the field is in the section's struct
	index []int
	// Whether a list keeps its lines as they are
	verbatim bool
	// What one of the field's units is, for a field that holds a length of time
	unit time.Duration
}

// Options are the values an AdvancedKindEnum field can have
func (f *AdvancedField) Options() *ListOfStrings {
	return List(f.options)
}

// AdvancedSection is a section of the configuration as it was when read, to edit and save (SaveAdvancedSection)
type AdvancedSection struct {
	ID     string
	fields []*AdvancedField
}

func (s *AdvancedSection) FieldCount() int {
	return len(s.fields)
}

func (s *AdvancedSection) FieldAt(index int) *AdvancedField {
	return s.fields[index]
}

// Equal reports whether both hold the same fields, with the same values when read
func (s *AdvancedSection) Equal(other *AdvancedSection) bool {
	return s.ID == other.ID && slices.EqualFunc(s.fields, other.fields, func(a, b *AdvancedField) bool {
		return a.Path == b.Path && a.saved == b.saved
	})
}

// advancedTarget is the struct of a configuration that a section edits, as a pointer
func advancedTarget(conf *config.Configuration, id string) (any, error) {
	switch id {
	case AdvancedSectionOptions:
		return &conf.Options, nil
	case AdvancedSectionGUI:
		return &conf.GUI, nil
	case AdvancedSectionLDAP:
		return &conf.LDAP, nil
	case AdvancedSectionDefaultFolder:
		return &conf.Defaults.Folder, nil
	case AdvancedSectionDefaultDevice:
		return &conf.Defaults.Device, nil
	case AdvancedSectionDefaultIgnores:
		return &conf.Defaults.Ignores, nil
	}
	if folderID, ok := strings.CutPrefix(id, AdvancedSectionFolderPrefix); ok {
		for i := range conf.Folders {
			if conf.Folders[i].ID == folderID {
				return &conf.Folders[i], nil
			}
		}
	}
	if deviceID, ok := strings.CutPrefix(id, AdvancedSectionDevicePrefix); ok {
		for i := range conf.Devices {
			if conf.Devices[i].DeviceID.String() == deviceID {
				return &conf.Devices[i], nil
			}
		}
	}
	return nil, ErrAdvancedSectionNotFound
}

// isAdvancedDefaults reports whether a section holds what new folders and devices start with
func isAdvancedDefaults(id string) bool {
	return id == AdvancedSectionDefaultFolder || id == AdvancedSectionDefaultDevice
}

// advancedFieldKeys are the names by which the lists of fields can name a field of a section. The name of a section's
// kind also stands for the defaults of new ones; "defaults/folder.x" and "device/.x" stand for those or the others alone.
func advancedFieldKeys(id, path string) []string {
	switch {
	case id == AdvancedSectionDefaultFolder:
		return []string{"folder." + path, id + "." + path}
	case id == AdvancedSectionDefaultDevice:
		return []string{"device." + path, id + "." + path}
	case strings.HasPrefix(id, AdvancedSectionFolderPrefix):
		return []string{"folder." + path}
	case strings.HasPrefix(id, AdvancedSectionDevicePrefix):
		return []string{"device." + path, AdvancedSectionDevicePrefix + "." + path}
	default:
		return []string{id + "." + path}
	}
}

func advancedFieldListed(list []string, id, path string) bool {
	return slices.ContainsFunc(advancedFieldKeys(id, path), func(key string) bool { return slices.Contains(list, key) })
}

// newAdvancedSection reads a section of a configuration
func newAdvancedSection(conf *config.Configuration, id string, ios bool) (*AdvancedSection, error) {
	if ios && (id == AdvancedSectionGUI || id == AdvancedSectionLDAP) {
		return nil, ErrGUINotAvailable
	}
	target, err := advancedTarget(conf, id)
	if err != nil {
		return nil, err
	}
	section := &AdvancedSection{ID: id}
	section.addFields(reflect.ValueOf(target).Elem(), "", "", nil, ios)
	return section, nil
}

// addFields adds the fields of a struct, those of structs it has after its own
func (s *AdvancedSection) addFields(structure reflect.Value, prefix, group string, index []int, ios bool) {
	type nested struct {
		value reflect.Value
		name  string
		index []int
	}
	var structs []nested

	for i := range structure.NumField() {
		member := structure.Type().Field(i)
		name, _, _ := strings.Cut(member.Tag.Get("json"), ",")
		if !member.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = member.Name
		}
		path := prefix + name
		if ios && advancedFieldListed(advancedFieldsHiddenOnIOS, s.ID, path) {
			continue
		}
		value := structure.Field(i)
		fieldIndex := append(slices.Clone(index), i)
		if value.Kind() == reflect.Struct && !isTextField(value) {
			structs = append(structs, nested{value, name, fieldIndex})
			continue
		}

		field := &AdvancedField{Path: path, Label: advancedLabel(name), Group: group, index: fieldIndex, unit: advancedUnit(name)}
		field.IsReadOnly = advancedFieldListed(advancedReadOnlyFields, s.ID, path) ||
			(isAdvancedDefaults(s.ID) && member.Tag.Get("nodefault") == "true")
		field.IsSecret = value.Kind() == reflect.String && isSecretName(name)
		field.verbatim = structure.Type() == reflect.TypeFor[config.Ignores]()
		field.describe(value)
		s.fields = append(s.fields, field)
	}

	for _, inner := range structs {
		s.addFields(inner.value, prefix+inner.name+".", advancedLabel(inner.name), inner.index, ios)
	}
}

// isTextField reports whether a value is text to the engine, such as a device ID, whatever it is underneath
func isTextField(value reflect.Value) bool {
	_, isMarshaler := value.Interface().(encoding.TextMarshaler)
	_, isUnmarshaler := value.Addr().Interface().(encoding.TextUnmarshaler)
	return isMarshaler && isUnmarshaler
}

// isSecretName reports whether a field's name says that its value is to be kept from view
func isSecretName(name string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc([]string{"password", "passphrase", "apikey", "secret", "token"}, func(word string) bool {
		return strings.Contains(name, word)
	})
}

// describe sets the kind and the value of a field. Value and saved are the same until someone edits the field.
func (f *AdvancedField) describe(value reflect.Value) {
	switch {
	case value.Type() == reflect.TypeFor[config.FilesystemType]():
		f.Kind, f.saved = AdvancedKindEnum, marshalAdvancedText(value)
		f.options = advancedFilesystemTypes(f.saved)
	case value.CanInt() && isTextField(value):
		f.Kind, f.options = AdvancedKindEnum, advancedEnumOptions(value.Type())
		f.saved = marshalAdvancedText(value)
	case isTextField(value):
		f.Kind, f.saved = AdvancedKindString, marshalAdvancedText(value)
	case value.Kind() == reflect.Bool:
		f.Kind, f.saved = AdvancedKindBool, strconv.FormatBool(value.Bool())
	case value.CanInt():
		f.Kind, f.saved = AdvancedKindInt, strconv.FormatInt(value.Int(), 10)
	case value.CanUint():
		f.Kind, f.saved = AdvancedKindInt, strconv.FormatUint(value.Uint(), 10)
	case value.CanFloat():
		f.Kind, f.saved = AdvancedKindFloat, strconv.FormatFloat(value.Float(), 'f', -1, 64)
	case value.Kind() == reflect.String:
		f.Kind, f.saved = AdvancedKindString, value.String()
	case value.Type().ConvertibleTo(reflect.TypeFor[[]string]()):
		f.Kind, f.saved = AdvancedKindList, strings.Join(value.Convert(reflect.TypeFor[[]string]()).Interface().([]string), "\n")
	default:
		// Lists of structs and maps have no control of their own: the web GUI leaves them out
		f.Kind = AdvancedKindJSON
		var text strings.Builder
		encoder := json.NewEncoder(&text)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", advancedJSONIndent)
		if err := encoder.Encode(value.Interface()); err == nil {
			f.saved = strings.TrimSuffix(text.String(), "\n")
		}
	}
	if f.IsSecret {
		f.saved = ""
	}
	f.Value = f.saved
}

func marshalAdvancedText(value reflect.Value) string {
	text, err := value.Interface().(encoding.TextMarshaler).MarshalText()
	if err != nil {
		return ""
	}
	return string(text)
}

// advancedFilesystemTypes are the file systems a folder can be on, with the one it is on. Unlike what the configuration
// otherwise holds these are not listed anywhere: a type that doesn't exist leaves a folder in error, and Syncthing's
// fake one, for its tests, has no files, which the engine takes for all of them having been deleted.
func advancedFilesystemTypes(current string) []string {
	types := slices.Concat([]string{string(config.FilesystemTypeBasic), filesystemTypeTrash}, registeredCustomFilesystemTypes())
	if !slices.Contains(types, current) {
		types = append(types, current)
	}
	return types
}

// advancedEnumOptions finds the values of an enumeration in the configuration: the numbers that its text stands for.
// Numbers without a meaning have no text, or text that parses as another number.
func advancedEnumOptions(enum reflect.Type) []string {
	const maxEnumValue = 64
	var options []string
	for number := range int64(maxEnumValue) {
		value := reflect.New(enum).Elem()
		value.SetInt(number)
		text := marshalAdvancedText(value)
		if text == "" {
			continue
		}
		parsed := reflect.New(enum)
		err := parsed.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(text))
		if err == nil && parsed.Elem().Int() == number {
			options = append(options, text)
		}
	}
	return options
}

// advancedLabelWords stay as they are in a label, with longer ones before those they contain
var advancedLabelWords = []string{
	"IDs", "ID", "URL", "UR", "API", "GUI", "QUIC", "TCP", "UDP", "NAT", "LAN", "WAN", "KiB", "MiB", "GiB", "TiB", "CR",
	"FS", "MC", "macOS",
}

// The last word of a name that says in which unit its value is
var advancedNameUnits = map[string]struct {
	label    string
	duration time.Duration
}{
	"S":  {"(seconds)", time.Second},
	"M":  {"(minutes)", time.Minute},
	"H":  {"(hours)", time.Hour},
	"Ms": {"(milliseconds)", time.Millisecond},
}

// advancedNameWords splits a field's name in words
func advancedNameWords(name string) []string {
	var words []string
	word := ""
	flush := func() {
		if word != "" {
			words = append(words, word)
			word = ""
		}
	}
	for rest := name; rest != ""; {
		known := ""
		for _, candidate := range advancedLabelWords {
			if strings.HasPrefix(rest, candidate) {
				known = candidate
				break
			}
		}
		if known != "" {
			flush()
			words = append(words, known)
			rest = rest[len(known):]
			continue
		}
		next := rune(rest[0])
		if word != "" && unicode.IsUpper(next) && !unicode.IsUpper(rune(word[len(word)-1])) {
			flush()
		}
		word += string(next)
		rest = rest[1:]
	}
	flush()
	return words
}

// advancedUnit is what one of a field's units is, as its name says, when it holds a length of time; otherwise 0
func advancedUnit(name string) time.Duration {
	words := advancedNameWords(name)
	if len(words) < 2 {
		return 0
	}
	return advancedNameUnits[words[len(words)-1]].duration
}

// advancedLabel makes a name for people out of a field's name, as the web GUI does (rescanIntervalS becomes Rescan
// Interval (seconds))
func advancedLabel(name string) string {
	words := advancedNameWords(name)
	last := len(words) - 1
	if unit, ok := advancedNameUnits[words[last]]; ok && last > 0 {
		words[last] = unit.label
	}
	for i, word := range words {
		known := slices.IndexFunc(advancedLabelWords, func(candidate string) bool { return strings.EqualFold(candidate, word) })
		if known >= 0 {
			words[i] = advancedLabelWords[known]
		} else if !strings.HasPrefix(word, "(") {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

// set puts the field's edited value in the configuration
func (f *AdvancedField) set(value reflect.Value) error {
	text := f.Value
	switch f.Kind {
	case AdvancedKindBool:
		parsed, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return errAdvancedNotBool
		}
		value.SetBool(parsed)
	case AdvancedKindInt:
		return f.setInt(value, strings.TrimSpace(text))
	case AdvancedKindFloat:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), value.Type().Bits())
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return errAdvancedNotNumber
		}
		if !f.fitsADuration(parsed) {
			return errAdvancedTooLong
		}
		value.SetFloat(parsed)
	case AdvancedKindEnum:
		if !slices.Contains(f.options, text) {
			return errAdvancedNotOption
		}
		return value.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(text))
	case AdvancedKindString:
		if isTextField(value) {
			return value.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(text))
		}
		value.SetString(text)
	case AdvancedKindList:
		value.Set(reflect.ValueOf(advancedListItems(text, f.verbatim)).Convert(value.Type()))
	case AdvancedKindJSON:
		return setAdvancedJSON(value, text)
	}
	return nil
}

func (f *AdvancedField) setInt(value reflect.Value, text string) error {
	if value.CanUint() {
		parsed, err := strconv.ParseUint(text, 10, 64)
		if err != nil || value.OverflowUint(parsed) {
			return errAdvancedNotWholeNumber
		}
		if !f.fitsADuration(float64(parsed)) {
			return errAdvancedTooLong
		}
		value.SetUint(parsed)
		return nil
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value.OverflowInt(parsed) {
		return errAdvancedNotWholeNumber
	}
	if !f.fitsADuration(float64(parsed)) {
		return errAdvancedTooLong
	}
	value.SetInt(parsed)
	return nil
}

// fitsADuration reports whether the engine can turn a number into the length of time the field holds. Where it can't,
// the result is not what was entered, and the engine limits only some of these fields when it prepares a configuration.
func (f *AdvancedField) fitsADuration(number float64) bool {
	return f.unit == 0 || math.Abs(number) <= float64(math.MaxInt64/int64(f.unit))
}

// setAdvancedJSON replaces a value with the one in the JSON, in which a name the value has no field for is a mistake
func setAdvancedJSON(value reflect.Value, text string) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	parsed := reflect.New(value.Type())
	if err := decoder.Decode(parsed.Interface()); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("there is more text after the value")
	}
	value.Set(parsed.Elem())
	return nil
}

// advancedListItems splits the text of a list in items, leaving out empty lines, which aren't items. Ignore patterns
// keep them, and their spaces, as the web GUI does: there they are only layout, and a pattern can end in a space.
func advancedListItems(text string, verbatim bool) []string {
	items := []string{}
	for line := range strings.SplitSeq(text, "\n") {
		if !verbatim {
			line = strings.TrimSpace(line)
		}
		if line != "" || verbatim {
			items = append(items, line)
		}
	}
	if verbatim {
		for len(items) > 0 && items[len(items)-1] == "" {
			items = items[:len(items)-1]
		}
	}
	return items
}

// applyTo checks the edited fields and puts them in a configuration, which is a copy to throw away if that fails
func (s *AdvancedSection) applyTo(conf *config.Configuration, ios bool) error {
	if ios && (s.ID == AdvancedSectionGUI || s.ID == AdvancedSectionLDAP) {
		return ErrGUINotAvailable
	}
	target, err := advancedTarget(conf, s.ID)
	if err != nil {
		return err
	}
	structure := reflect.ValueOf(target).Elem()
	for _, field := range s.fields {
		if field.Value == field.saved {
			continue
		}
		if field.IsReadOnly {
			return fmt.Errorf("%s: this field can't be changed here", field.Path)
		}
		if err := field.set(structure.FieldByIndex(field.index)); err != nil {
			return fmt.Errorf("%s: %w", field.Path, err)
		}
	}
	return nil
}

// checkAdvancedGUI holds a change to the GUI's or LDAP's fields to the rules of SetGUISettings: such a change must not
// leave the GUI unreachable. A password that changed goes in as the REST API takes it (hashed unless it already is).
// Running is whether the GUI service runs, which it goes on doing after the configuration turned it off, listening
// again on any change of address until the engine restarts.
func checkAdvancedGUI(from *config.Configuration, to *config.Configuration, running bool) error {
	if to.GUI.Password != from.GUI.Password {
		if to.GUI.User == "" {
			return ErrGUIUserRequired
		}
		if err := to.GUI.SetPassword(to.GUI.Password); err != nil {
			return err
		}
	}
	settings := newGUISettings(to.GUI, to.LDAP)
	gui, ldap := to.GUI, to.LDAP
	gui.RawAddress = from.GUI.RawAddress
	gui.Enabled = running || to.GUI.Enabled
	if err := settings.applyTo(&gui, &ldap); err != nil {
		return err
	}
	gui.Enabled = to.GUI.Enabled
	to.GUI = gui
	to.LDAP.Address = ldap.Address
	if to.GUI.Enabled && !from.GUI.Enabled {
		protectGUI(&to.GUI)
	}
	return nil
}

// AdvancedSection reads a section of the configuration to edit: an AdvancedSection constant, or a prefix and an ID
func (clt *Client) AdvancedSection(id string) (*AdvancedSection, error) {
	cfg := clt.cfg()
	if cfg == nil {
		return nil, ErrStillLoading
	}
	conf := cfg.RawCopy()
	return newAdvancedSection(&conf, id, build.IsIOS)
}

// SaveAdvancedSection saves the fields of a section that were edited, which leaves other fields as they are now. The
// configuration goes through the same checks as any change, and when those refuse it, nothing is changed and their
// error is returned. A field the engine then corrects (as it does with a short reconnection interval) shows its new
// value when the section is read again.
func (clt *Client) SaveAdvancedSection(section *AdvancedSection) error {
	r := clt.currentRun()
	if r == nil {
		return ErrStillLoading
	}
	var invalid error
	moved := false
	err := clt.changeConfiguration(func(conf *config.Configuration) {
		to := conf.Copy()
		if invalid = section.applyTo(&to, build.IsIOS); invalid != nil {
			return
		}
		if section.ID == AdvancedSectionGUI || section.ID == AdvancedSectionLDAP {
			if invalid = checkAdvancedGUI(conf, &to, r.guiStarted); invalid != nil {
				return
			}
		}
		moved = to.GUI.RawAddress != conf.GUI.RawAddress
		fillReportingURLs(&to.Options)
		*conf = to
	})
	if invalid != nil {
		return invalid
	}
	if err != nil {
		return err
	}
	if moved {
		// As in SetGUISettings
		clt.guiNotice.Store("")
	}
	return nil
}

// IsEngineRestartRequired reports whether saved changes only take effect when the engine restarts (RestartEngine):
// changes the engine says so of, or turning the GUI on or off, as that starts and stops with the engine
func (clt *Client) IsEngineRestartRequired() bool {
	r := clt.currentRun()
	return r != nil && r.config != nil && (r.config.RequiresRestart() || r.config.GUI().Enabled != r.guiStarted)
}
