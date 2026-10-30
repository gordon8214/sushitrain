// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

const advancedTestFolder = "folder-1"

var advancedTestSections = []string{
	AdvancedSectionOptions, AdvancedSectionGUI, AdvancedSectionLDAP, AdvancedSectionDefaultFolder,
	AdvancedSectionDefaultDevice, AdvancedSectionDefaultIgnores, AdvancedSectionFolderPrefix + advancedTestFolder,
	AdvancedSectionDevicePrefix + protocol.LocalDeviceID.String(),
}

// advancedTestConfiguration has a folder, the local device and a free GUI address
func advancedTestConfiguration(t *testing.T) config.Configuration {
	t.Helper()
	conf := config.New(protocol.LocalDeviceID)
	folder := conf.Defaults.Folder.Copy()
	folder.ID = advancedTestFolder
	folder.Label = "Folder one"
	folder.Path = t.TempDir()
	conf.SetFolder(folder)
	conf.GUI = guiForSettings(t)
	// The configuration removes this one by itself, now that the GUI has a password
	conf.Options.UnackedNotificationIDs = []string{}
	return conf
}

// advancedTestClient serves a configuration, with the given checks on changes
func advancedTestClient(t *testing.T, verifiers ...config.Committer) (*Client, config.Wrapper) {
	t.Helper()
	wrapper := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), advancedTestConfiguration(t), protocol.LocalDeviceID,
		events.NoopLogger)
	serveConfiguration(t, wrapper)
	for _, verifier := range verifiers {
		wrapper.Subscribe(verifier)
	}
	return (&Client{}).withTestRun(wrapper, nil, events.NoopLogger), wrapper
}

func advancedField(t *testing.T, section *AdvancedSection, path string) *AdvancedField {
	t.Helper()
	for _, field := range section.fields {
		if field.Path == path {
			return field
		}
	}
	t.Fatalf("%s has no field %s", section.ID, path)
	return nil
}

// saveAdvancedField edits one field of a section and saves it
func saveAdvancedField(t *testing.T, client *Client, id, path, value string) error {
	t.Helper()
	section, err := client.AdvancedSection(id)
	if err != nil {
		t.Fatal(err)
	}
	advancedField(t, section, path).Value = value
	return client.SaveAdvancedSection(section)
}

// jsonFieldPaths are the paths of a struct's fields, walking the struct apart from the code it tests
func jsonFieldPaths(structure reflect.Type, prefix string) []string {
	var paths []string
	for field := range structure.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		_, hasParts := reflect.New(field.Type).Interface().(interface{ UnmarshalText([]byte) error })
		if field.Type.Kind() == reflect.Struct && !hasParts {
			paths = append(paths, jsonFieldPaths(field.Type, prefix+name+".")...)
		} else {
			paths = append(paths, prefix+name)
		}
	}
	return paths
}

func TestAdvancedSectionsListEveryField(t *testing.T) {
	conf := advancedTestConfiguration(t)
	for _, id := range advancedTestSections {
		section, err := newAdvancedSection(&conf, id, false)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		target, _ := advancedTarget(&conf, id)
		want := jsonFieldPaths(reflect.TypeOf(target).Elem(), "")
		var got []string
		for _, field := range section.fields {
			got = append(got, field.Path)
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s:\n want %v\n  got %v", id, want, got)
		}
	}
}

func TestAdvancedFieldsComeFromTheStructure(t *testing.T) {
	type inner struct {
		Mode config.PullOrder `json:"mode"`
	}
	type newFields struct {
		NewSwitch bool              `json:"newSwitch"`
		NewCount  int               `json:"newCount"`
		NewRatio  float64           `json:"newRatio"`
		NewName   string            `json:"newName,omitempty"`
		NewList   []string          `json:"newList"`
		NewMap    map[string]string `json:"newMap"`
		NewOrder  config.PullOrder  `json:"newOrder"`
		NewPeer   protocol.DeviceID `json:"newPeer"`
		NewInner  inner             `json:"newInner"`
		Old       bool              `json:"-"`
		hidden    bool
	}
	value := newFields{NewCount: 3, NewRatio: 0.5, NewName: "name", NewList: []string{"a", "b"}, NewPeer: protocol.LocalDeviceID}
	_ = value.hidden

	section := &AdvancedSection{ID: "test"}
	section.addFields(reflect.ValueOf(&value).Elem(), "", "", nil, false)
	want := map[string]struct{ kind, value, label string }{
		"newSwitch":     {AdvancedKindBool, "false", "New Switch"},
		"newCount":      {AdvancedKindInt, "3", "New Count"},
		"newRatio":      {AdvancedKindFloat, "0.5", "New Ratio"},
		"newName":       {AdvancedKindString, "name", "New Name"},
		"newList":       {AdvancedKindList, "a\nb", "New List"},
		"newMap":        {AdvancedKindJSON, "null", "New Map"},
		"newOrder":      {AdvancedKindEnum, "random", "New Order"},
		"newPeer":       {AdvancedKindString, protocol.LocalDeviceID.String(), "New Peer"},
		"newInner.mode": {AdvancedKindEnum, "random", "Mode"},
	}
	if len(section.fields) != len(want) {
		t.Fatalf("%d fields, not %d", len(section.fields), len(want))
	}
	for _, field := range section.fields {
		expected, ok := want[field.Path]
		if !ok {
			t.Fatalf("unexpected field %s", field.Path)
		}
		if field.Kind != expected.kind || field.Value != expected.value || field.Label != expected.label {
			t.Errorf("%s: %+v, not %+v", field.Path, field, expected)
		}
	}
	if last := section.fields[len(section.fields)-1]; last.Path != "newInner.mode" || last.Group != "New Inner" {
		t.Errorf("a struct's fields should come last, in a group: %+v", last)
	}
}

func TestAdvancedFieldsHaveTheControlsForTheirTypes(t *testing.T) {
	conf := advancedTestConfiguration(t)
	section, err := newAdvancedSection(&conf, AdvancedSectionFolderPrefix+advancedTestFolder, false)
	if err != nil {
		t.Fatal(err)
	}
	for path, kind := range map[string]string{
		"paused":                AdvancedKindBool,
		"rescanIntervalS":       AdvancedKindInt,
		"fsWatcherDelayS":       AdvancedKindFloat,
		"label":                 AdvancedKindString,
		"filesystemType":        AdvancedKindEnum,
		"type":                  AdvancedKindEnum,
		"devices":               AdvancedKindJSON,
		"minDiskFree.value":     AdvancedKindFloat,
		"minDiskFree.unit":      AdvancedKindString,
		"versioning.fsType":     AdvancedKindEnum,
		"versioning.params":     AdvancedKindJSON,
		"xattrFilter.macOSMode": AdvancedKindEnum,
		"xattrFilter.entries":   AdvancedKindJSON,
	} {
		if field := advancedField(t, section, path); field.Kind != kind {
			t.Errorf("%s is %s, not %s", path, field.Kind, kind)
		}
	}
	if options := advancedField(t, section, "type").options; !slices.Equal(options,
		[]string{"sendreceive", "sendonly", "receiveonly", "receiveencrypted"}) {
		t.Errorf("folder types: %v", options)
	}
	if options := advancedField(t, section, "xattrFilter.macOSMode").options; !slices.Equal(options, []string{"syncable", "all"}) {
		t.Errorf("macOS modes: %v", options)
	}
}

func TestEveryEnumHasItsOptions(t *testing.T) {
	conf := advancedTestConfiguration(t)
	for _, id := range advancedTestSections {
		section, err := newAdvancedSection(&conf, id, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range section.fields {
			if field.Kind == AdvancedKindEnum && (len(field.options) < 2 || !slices.Contains(field.options, field.Value)) {
				t.Errorf("%s of %s has %q with options %v", field.Path, id, field.Value, field.options)
			}
		}
	}
	section, _ := newAdvancedSection(&conf, AdvancedSectionLDAP, false)
	if options := advancedField(t, section, "transport").options; !slices.Equal(options, []string{"plain", "tls", "starttls"}) {
		t.Errorf("a gap in the numbers of an enum should not matter: %v", options)
	}
}

func TestAdvancedLabels(t *testing.T) {
	for name, label := range map[string]string{
		"rescanIntervalS":                 "Rescan Interval (seconds)",
		"relayReconnectIntervalM":         "Relay Reconnect Interval (minutes)",
		"autoUpgradeIntervalH":            "Auto Upgrade Interval (hours)",
		"useTLS":                          "Use TLS",
		"natEnabled":                      "NAT Enabled",
		"id":                              "ID",
		"deviceID":                        "Device ID",
		"apiKey":                          "API Key",
		"urURL":                           "UR URL",
		"unackedNotificationIDs":          "Unacked Notification IDs",
		"maxConcurrentIncomingRequestKiB": "Max Concurrent Incoming Request KiB",
		"connectionPriorityQuicLan":       "Connection Priority QUIC LAN",
		"S":                               "S",
		"remoteGUIPort":                   "Remote GUI Port",
		"macOSMode":                       "macOS Mode",
		"crURL":                           "CR URL",
		"fsWatcherEnabled":                "FS Watcher Enabled",
		"localAnnounceMCAddr":             "Local Announce MC Addr",
	} {
		if got := advancedLabel(name); got != label {
			t.Errorf("%s: %q, not %q", name, got, label)
		}
	}
}

func TestSavingAnAdvancedFieldChangesOnlyThatField(t *testing.T) {
	client, wrapper := advancedTestClient(t)

	section, err := client.AdvancedSection(AdvancedSectionOptions)
	if err != nil {
		t.Fatal(err)
	}
	advancedField(t, section, "keepTemporariesH").Value = "48"
	// What another change does after the section was read stays
	if err := client.changeConfiguration(func(conf *config.Configuration) { conf.Options.MaxSendKbps = 1234 }); err != nil {
		t.Fatal(err)
	}
	before := wrapper.RawCopy()
	if err := client.SaveAdvancedSection(section); err != nil {
		t.Fatal(err)
	}

	before.Options.KeepTemporariesH = 48
	if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatalf("configuration changed otherwise:\n%+v\n%+v", before.Options, after.Options)
	}
}

func TestSavingAnUnchangedSectionChangesNothing(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.RawCopy()
	for _, id := range advancedTestSections {
		section, err := client.AdvancedSection(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.SaveAdvancedSection(section); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
}

func TestAdvancedFieldsOfEveryKindAreSaved(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	folderSection := AdvancedSectionFolderPrefix + advancedTestFolder
	peer := protocol.DeviceID{1}
	for _, change := range []struct{ section, path, value string }{
		{folderSection, "paused", "true"},
		{folderSection, "rescanIntervalS", "120"},
		{folderSection, "fsWatcherDelayS", "2.5"},
		{folderSection, "label", "Renamed"},
		{folderSection, "order", "newestFirst"},
		{folderSection, "minDiskFree.value", "5"},
		{folderSection, "minDiskFree.unit", "GiB"},
		{folderSection, "versioning.type", "simple"},
		{folderSection, "versioning.params", `{"keep": "3"}`},
		{folderSection, "xattrFilter.entries", `[{"match": "*", "permit": true}]`},
		{AdvancedSectionDefaultDevice, "addresses", "tcp://one\n\n  tcp://two  \n"},
		{AdvancedSectionDefaultDevice, "introducedBy", ""},
		{AdvancedSectionDevicePrefix + protocol.LocalDeviceID.String(), "introducedBy", peer.String()},
		{AdvancedSectionDefaultIgnores, "lines", "one\n\n// two \n\n"},
		{AdvancedSectionOptions, "listenAddresses", "default"},
	} {
		if err := saveAdvancedField(t, client, change.section, change.path, change.value); err != nil {
			t.Fatalf("%s: %v", change.path, err)
		}
	}

	folder, ok := wrapper.Folder(advancedTestFolder)
	if !ok {
		t.Fatal("the folder is gone")
	}
	if !folder.Paused || folder.RescanIntervalS != 120 || folder.FSWatcherDelayS != 2.5 || folder.Label != "Renamed" ||
		folder.Order != config.PullOrderNewestFirst || folder.MinDiskFree != (config.Size{Value: 5, Unit: "GiB"}) ||
		folder.Versioning.Type != "simple" || folder.Versioning.Params["keep"] != "3" ||
		!slices.Equal(folder.XattrFilter.Entries, []config.XattrFilterEntry{{Match: "*", Permit: true}}) {
		t.Fatalf("folder: %+v", folder)
	}
	if addresses := wrapper.DefaultDevice().Addresses; !slices.Equal(addresses, []string{"tcp://one", "tcp://two"}) {
		t.Fatalf("a list drops empty lines and spaces: %q", addresses)
	}
	if lines := wrapper.DefaultIgnores().Lines; !slices.Equal(lines, []string{"one", "", "// two "}) {
		t.Fatalf("ignore patterns keep their lines, up to the last: %q", lines)
	}
	if device, _ := wrapper.Device(protocol.LocalDeviceID); device.IntroducedBy != peer {
		t.Fatalf("device: %+v", device)
	}
}

func TestInvalidAdvancedValuesChangeNothing(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.RawCopy()
	folderSection := AdvancedSectionFolderPrefix + advancedTestFolder
	for _, change := range []struct {
		section, path, value string
		want                 error
	}{
		{folderSection, "rescanIntervalS", "soon", errAdvancedNotWholeNumber},
		{folderSection, "rescanIntervalS", "1.5", errAdvancedNotWholeNumber},
		{folderSection, "rescanIntervalS", "99999999999999999999", errAdvancedNotWholeNumber},
		{folderSection, "fsWatcherDelayS", "NaN", errAdvancedNotNumber},
		{folderSection, "fsWatcherDelayS", "Inf", errAdvancedNotNumber},
		{folderSection, "paused", "maybe", errAdvancedNotBool},
		{folderSection, "type", "sideways", errAdvancedNotOption},
		{folderSection, "filesystemType", "fake", errAdvancedNotOption},
		{folderSection, "filesystemType", "Basic", errAdvancedNotOption},
		{folderSection, "versioning.fsType", "nonsense", errAdvancedNotOption},
		{folderSection, "scanProgressIntervalS", "10000000000", errAdvancedTooLong},
		{folderSection, "scanProgressIntervalS", "-10000000000", errAdvancedTooLong},
		{folderSection, "fsWatcherDelayS", "1e300", errAdvancedTooLong},
		{AdvancedSectionOptions, "autoUpgradeIntervalH", "9999999999", errAdvancedTooLong},
	} {
		err := saveAdvancedField(t, client, change.section, change.path, change.value)
		if !errors.Is(err, change.want) || !strings.HasPrefix(err.Error(), change.path+": ") {
			t.Errorf("%s = %s: %v", change.path, change.value, err)
		}
	}
	for _, change := range []struct{ section, path, value string }{
		{folderSection, "versioning.params", "{"},
		{folderSection, "versioning.params", `{"keep": 3}`},
		{folderSection, "versioning.params", `{} {}`},
		{folderSection, "versioning.params", `{"keep": "3"}}`},
		{folderSection, "xattrFilter.entries", `[]]`},
		{folderSection, "xattrFilter.entries", `[] ]]] trailing`},
		{folderSection, "devices", `[{"device": "typo"}]`},
		{folderSection, "devices", `[{"deviceID": "not a device"}]`},
		{AdvancedSectionDefaultDevice, "introducedBy", "not a device"},
	} {
		if err := saveAdvancedField(t, client, change.section, change.path, change.value); err == nil {
			t.Errorf("%s = %s was accepted", change.path, change.value)
		}
	}

	// A change that is refused takes the other changes of the section with it
	section, _ := client.AdvancedSection(folderSection)
	advancedField(t, section, "label").Value = "Renamed"
	advancedField(t, section, "copiers").Value = "many"
	if err := client.SaveAdvancedSection(section); !errors.Is(err, errAdvancedNotWholeNumber) {
		t.Errorf("save: %v", err)
	}

	if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
}

func TestTheEnginesErrorRefusesAnAdvancedChange(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.RawCopy()
	folderSection := AdvancedSectionFolderPrefix + advancedTestFolder
	err := saveAdvancedField(t, client, folderSection, "path", "")
	if err == nil || !strings.Contains(err.Error(), "folder has empty path") {
		t.Fatalf("the configuration's own check should refuse an empty path: %v", err)
	}

	if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatal("changed")
	}

	checked, checkedWrapper := advancedTestClient(t, rejectAll{})
	before = checkedWrapper.RawCopy()
	err = saveAdvancedField(t, checked, folderSection, "label", "Renamed")
	if err == nil || err.Error() != "rejected" {
		t.Fatalf("a check of the engine's should refuse: %v", err)
	}
	if after := checkedWrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatal("changed")
	}
}

func TestRemoteManagedFoldersStayPausedInTheAdvancedEditor(t *testing.T) {
	set := loadRemoteManagedForTest(t, filepath.Join(t.TempDir(), remoteManagedFileName))
	if err := set.set(advancedTestFolder, true); err != nil {
		t.Fatal(err)
	}
	folder := config.New(protocol.LocalDeviceID).Defaults.Folder.Copy()
	folder.ID, folder.Path = advancedTestFolder, t.TempDir()
	wrapper := wrapRemoteManaged(t, set, folder)
	client := (&Client{}).withTestRun(wrapper, nil, events.NoopLogger)

	err := saveAdvancedField(t, client, AdvancedSectionFolderPrefix+advancedTestFolder, "paused", "false")
	if !errors.Is(err, errRemoteManaged) {
		t.Fatalf("resuming: %v", err)
	}
}

func TestIdentitiesCannotBeChangedInTheAdvancedEditor(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.RawCopy()
	for _, change := range []struct{ section, path string }{
		{AdvancedSectionFolderPrefix + advancedTestFolder, "id"},
		{AdvancedSectionDevicePrefix + protocol.LocalDeviceID.String(), "deviceID"},
		{AdvancedSectionDefaultFolder, "id"},
		{AdvancedSectionDefaultDevice, "deviceID"},
		{AdvancedSectionDefaultDevice, "introducedBy"},
		{AdvancedSectionOptions, "urAccepted"},
		{AdvancedSectionDevicePrefix + protocol.LocalDeviceID.String(), "paused"},
	} {
		section, err := client.AdvancedSection(change.section)
		if err != nil {
			t.Fatal(err)
		}
		field := advancedField(t, section, change.path)
		if !field.IsReadOnly {
			t.Errorf("%s of %s can be changed", change.path, change.section)
		}
		field.Value = "1"
		if err := client.SaveAdvancedSection(section); err == nil {
			t.Errorf("%s of %s was saved", change.path, change.section)
		}
	}
	if after := wrapper.RawCopy(); !reflect.DeepEqual(before, after) {
		t.Fatal("changed")
	}
}

func TestDefaultsOfNewDevicesCanBePaused(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := saveAdvancedField(t, client, AdvancedSectionDefaultDevice, "paused", "true"); err != nil {
		t.Fatal(err)
	}
	if !wrapper.DefaultDevice().Paused {
		t.Fatal("the default was not saved")
	}
}

func TestFilesystemTypesAreThoseThatExist(t *testing.T) {
	RegisterCustomFilesystemType("advanced-test", nil)
	conf := advancedTestConfiguration(t)
	section, err := newAdvancedSection(&conf, AdvancedSectionFolderPrefix+advancedTestFolder, false)
	if err != nil {
		t.Fatal(err)
	}
	if options := advancedField(t, section, "filesystemType").options; !slices.Contains(options, "basic") ||
		!slices.Contains(options, filesystemTypeTrash) || !slices.Contains(options, "advanced-test") || slices.Contains(options, "fake") {
		t.Fatalf("options: %v", options)
	}

	// Whatever a folder already has stays a choice
	folder := conf.Folders[0]
	folder.FilesystemType = "from-an-older-version"
	conf.SetFolder(folder)
	section, _ = newAdvancedSection(&conf, AdvancedSectionFolderPrefix+advancedTestFolder, false)
	if field := advancedField(t, section, "filesystemType"); !slices.Contains(field.options, field.Value) {
		t.Fatalf("%q is not in %v", field.Value, field.options)
	}
}

func TestJSONFieldsShowWhatWasEntered(t *testing.T) {
	client, _ := advancedTestClient(t)
	folderSection := AdvancedSectionFolderPrefix + advancedTestFolder
	if err := saveAdvancedField(t, client, folderSection, "xattrFilter.entries", `[{"match": "a&b<c", "permit": true}]`); err != nil {
		t.Fatal(err)
	}
	section, _ := client.AdvancedSection(folderSection)
	if value := advancedField(t, section, "xattrFilter.entries").Value; !strings.Contains(value, "a&b<c") {
		t.Fatalf("value: %s", value)
	}
}

func TestAdvancedSectionsOfWhatIsGoneAreRefused(t *testing.T) {
	client, _ := advancedTestClient(t)
	section, err := client.AdvancedSection(AdvancedSectionFolderPrefix + advancedTestFolder)
	if err != nil {
		t.Fatal(err)
	}
	advancedField(t, section, "label").Value = "Renamed"
	if err := client.changeConfiguration(func(conf *config.Configuration) { conf.Folders = nil }); err != nil {
		t.Fatal(err)
	}
	if err := client.SaveAdvancedSection(section); !errors.Is(err, ErrAdvancedSectionNotFound) {
		t.Fatalf("save: %v", err)
	}
	if _, err := client.AdvancedSection(section.ID); !errors.Is(err, ErrAdvancedSectionNotFound) {
		t.Fatalf("read: %v", err)
	}
	if _, err := (&Client{}).AdvancedSection(AdvancedSectionOptions); !errors.Is(err, ErrStillLoading) {
		t.Fatalf("without a configuration: %v", err)
	}
}

func TestAdvancedSecretsStayHidden(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.GUI()
	section, err := client.AdvancedSection(AdvancedSectionGUI)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"password", "apiKey"} {
		if field := advancedField(t, section, path); !field.IsSecret || field.Value != "" {
			t.Fatalf("%s: %+v", path, field)
		}
	}
	if err := client.SaveAdvancedSection(section); err != nil {
		t.Fatal(err)
	}
	if wrapper.GUI() != before {
		t.Fatal("leaving a secret empty should keep it")
	}

	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "password", "new password"); err != nil {
		t.Fatal(err)
	}
	gui := wrapper.GUI()
	if gui.Password == "new password" || gui.CompareHashedPassword("new password") != nil {
		t.Fatalf("the password was not hashed: %q", gui.Password)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "apiKey", "a-new-key"); err != nil {
		t.Fatal(err)
	}
	if wrapper.GUI().APIKey != "a-new-key" {
		t.Fatal("the API key did not change")
	}
}

func TestAdvancedGUIChangesFollowTheRulesOfTheGUISettings(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.GUI()

	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "address", ""); !errors.Is(err, ErrGUIAddressRequired) {
		t.Errorf("no address: %v", err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "address", "127.0.0.1:0"); !errors.Is(err, ErrGUIPortRequired) {
		t.Errorf("port 0: %v", err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "authMode", "ldap"); !errors.Is(err, ErrGUILDAPAddressRequired) {
		t.Errorf("LDAP without a server: %v", err)
	}
	if wrapper.GUI() != before {
		t.Fatal("changed")
	}

	// Without a user there is no password either
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "user", ""); err != nil {
		t.Fatal(err)
	}
	if gui := wrapper.GUI(); gui.User != "" || gui.Password != "" {
		t.Fatalf("user: %+v", gui)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "password", "secret"); !errors.Is(err, ErrGUIUserRequired) {
		t.Fatalf("a password without a user: %v", err)
	}

	address := freeGUIAddress(t)
	client.guiNotice.Store("the GUI moved")
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "address", address); err != nil {
		t.Fatal(err)
	}
	if wrapper.GUI().RawAddress != address || client.GUIStatusMessage() != "" {
		t.Fatalf("moving: %q, %q", wrapper.GUI().RawAddress, client.GUIStatusMessage())
	}
}

func TestAdvancedLDAPChangesAreChecked(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := client.changeConfiguration(func(conf *config.Configuration) {
		conf.GUI.AuthMode = config.AuthModeLDAP
		conf.LDAP = config.LDAPConfiguration{Address: "ldap.example.com:636", BindDN: "cn=%s"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionLDAP, "address", "no port"); !errors.Is(err, ErrGUILDAPAddressRequired) {
		t.Fatalf("address: %v", err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionLDAP, "transport", "starttls"); err != nil {
		t.Fatal(err)
	}
	if ldap := wrapper.LDAP(); ldap.Transport != config.LDAPTransportStartTLS || ldap.Address != "ldap.example.com:636" {
		t.Fatalf("LDAP: %+v", ldap)
	}

	// The service dials the address as it is saved
	if err := saveAdvancedField(t, client, AdvancedSectionLDAP, "address", " ldap2.example.com:636 "); err != nil {
		t.Fatal(err)
	}
	if address := wrapper.LDAP().Address; address != "ldap2.example.com:636" {
		t.Fatalf("address: %q", address)
	}
}

func TestTurningTheGUIOnOrOffInTheAdvancedEditorNeedsARestart(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if client.IsEngineRestartRequired() {
		t.Fatal("no restart needed yet")
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if !client.IsEngineRestartRequired() {
		t.Fatal("the GUI stops with the engine")
	}

	// Turning it on again leaves nothing to restart for, and gives it a password if it lost its own
	if err := client.changeConfiguration(func(conf *config.Configuration) { conf.GUI.User, conf.GUI.Password = "", "" }); err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if client.IsEngineRestartRequired() {
		t.Fatal("the GUI is as it started")
	}
	if !wrapper.GUI().IsAuthEnabled() {
		t.Fatal("a GUI that goes on should get a password")
	}
}

func TestARunningGUIDoesNotRemoveAFileThatIsInTheWay(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.GUI().RawAddress
	path := filepath.Join(t.TempDir(), "document")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	keeps := func() {
		t.Helper()
		if wrapper.GUI().RawAddress != before {
			t.Fatal("the address moved to the file")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}

	// Together with turning it off in the configuration, as it listens until the engine restarts
	section, err := client.AdvancedSection(AdvancedSectionGUI)
	if err != nil {
		t.Fatal(err)
	}
	advancedField(t, section, "enabled").Value = "false"
	advancedField(t, section, "address").Value = path
	if err := client.SaveAdvancedSection(section); err == nil {
		t.Fatal("the same save should refuse a file")
	}
	keeps()

	// And after that, while the engine has not restarted yet
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedField(t, client, AdvancedSectionGUI, "address", path); err == nil {
		t.Fatal("a later save should refuse a file")
	}
	keeps()
}

func TestIOSHasNoAdvancedGUISettings(t *testing.T) {
	conf := advancedTestConfiguration(t)
	for _, id := range []string{AdvancedSectionGUI, AdvancedSectionLDAP} {
		if _, err := newAdvancedSection(&conf, id, true); !errors.Is(err, ErrGUINotAvailable) {
			t.Fatalf("%s: %v", id, err)
		}
		section, err := newAdvancedSection(&conf, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := section.applyTo(&conf, true); !errors.Is(err, ErrGUINotAvailable) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestIOSHidesWhatMakesNoSenseThere(t *testing.T) {
	conf := advancedTestConfiguration(t)
	paths := func(id string, ios bool) []string {
		section, err := newAdvancedSection(&conf, id, ios)
		if err != nil {
			t.Fatal(err)
		}
		return Map(section.fields, func(field *AdvancedField) string { return field.Path })
	}

	options := paths(AdvancedSectionOptions, true)
	if slices.Contains(options, "startBrowser") || slices.Contains(options, "progressUpdateIntervalS") {
		t.Errorf("options on iOS: %v", options)
	}
	if !slices.Contains(options, "maxSendKbps") {
		t.Error("iOS should keep what works there")
	}
	folder := paths(AdvancedSectionFolderPrefix+advancedTestFolder, true)
	if slices.Contains(folder, "path") || !slices.Contains(folder, "rescanIntervalS") {
		t.Errorf("a folder on iOS: %v", folder)
	}
	defaults := paths(AdvancedSectionDefaultFolder, true)
	if slices.Contains(defaults, "path") || slices.Contains(defaults, "rescanIntervalS") {
		t.Errorf("default folder on iOS: %v", defaults)
	}

	// Every listed field is a real one, so that a renamed field doesn't quietly show up again
	known := map[string]bool{}
	for _, id := range advancedTestSections {
		for _, path := range paths(id, false) {
			for _, key := range advancedFieldKeys(id, path) {
				known[key] = true
			}
		}
	}
	for _, key := range slices.Concat(advancedFieldsHiddenOnIOS, advancedReadOnlyFields) {
		if !known[key] {
			t.Errorf("%s is not a field", key)
		}
	}
}
