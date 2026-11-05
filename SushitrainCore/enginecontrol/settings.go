// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	core "t-shaped.nl/sushitrain/v2/src"
	"time"
	"unicode"
)

type Field = desktopapi.Field
type Editor = desktopapi.Editor

type draft struct {
	value    any
	advanced *core.AdvancedSection
	fields   []Field
	save     func() error
	expires  time.Time
}

func label(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && unicode.IsUpper(c) && (unicode.IsLower(r[i-1]) || (i+1 < len(r) && unicode.IsLower(r[i+1]))) {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}
func (c *Control) getSettings(section string) (Editor, error) {
	for token, d := range c.editors {
		if time.Now().After(d.expires) {
			delete(c.editors, token)
		}
	}
	if len(c.editors) >= 64 {
		return Editor{}, errors.New("close another settings editor before opening this one")
	}
	d := &draft{expires: time.Now().Add(draftLifetime)}
	cl := c.Client
	for _, prefix := range []string{"device/", "basic/device/"} {
		if strings.HasPrefix(section, prefix) && !slices.Contains(c.peers(), strings.TrimPrefix(section, prefix)) {
			return Editor{}, errors.New("invalid device ID")
		}
	}
	switch {
	case section == "engine":
		v := cl.EngineOptions()
		d.value = v
		d.save = func() error { return cl.SetEngineOptions(v) }
	case section == "connections":
		v := cl.ConnectionOptions()
		d.value = v
		d.save = func() error { return cl.SetConnectionOptions(v) }
	case section == "gui":
		v := cl.GUISettings()
		if v == nil {
			return Editor{}, errors.New("the settings are unavailable")
		}
		baseline := *v
		d.value = v
		d.save = func() error { return cl.SetGUISettings(mergeSettings(cl.GUISettings(), &baseline, v)) }
	case section == "proxy":
		v := cl.ProxySettings()
		if v == nil {
			return Editor{}, errors.New("the settings are unavailable")
		}
		baseline := *v
		d.value = v
		d.save = func() error { return cl.SetProxySettings(mergeSettings(cl.ProxySettings(), &baseline, v)) }
	case section == "defaults/folder":
		v := cl.DefaultFolderSettings()
		d.value = v
		d.save = func() error { return cl.SetDefaultFolderSettings(v) }
	case section == "defaults/device":
		v := cl.DefaultDeviceSettings()
		d.value = v
		d.save = func() error { return cl.SetDefaultDeviceSettings(v) }
	case section == "defaults/versioning":
		v := cl.DefaultVersioningSettings()
		d.value = v
		d.save = func() error { return cl.SetDefaultVersioningSettings(v) }
	case section == "defaults/ignores":
		v := &struct{ Patterns string }{cl.DefaultIgnoreLines()}
		d.value = v
		d.save = func() error { return cl.SetDefaultIgnoreLines(v.Patterns) }
	case strings.HasPrefix(section, "folder/"):
		f := cl.FolderWithID(strings.TrimPrefix(section, "folder/"))
		if f == nil || !f.Exists() {
			return Editor{}, errors.New("folder no longer exists")
		}
		v := f.Settings()
		d.value = v
		d.save = func() error { return f.SetSettings(v) }
	case strings.HasPrefix(section, "versioning/"):
		f := cl.FolderWithID(strings.TrimPrefix(section, "versioning/"))
		if f == nil || !f.Exists() {
			return Editor{}, errors.New("folder no longer exists")
		}
		v := f.VersioningSettings()
		d.value = v
		d.save = func() error { return f.SetVersioningSettings(v) }
	case strings.HasPrefix(section, "device/"):
		v := cl.PeerWithID(strings.TrimPrefix(section, "device/")).Settings()
		d.value = v
		d.save = func() error { return cl.PeerWithID(strings.TrimPrefix(section, "device/")).SetSettings(v) }
	case strings.HasPrefix(section, "basic/") || section == "bandwidth":
		id := strings.TrimPrefix(section, "basic/")
		if section == "bandwidth" {
			id = "options"
		}
		v, err := cl.AdvancedSection(id)
		if err != nil {
			return Editor{}, err
		}
		d.advanced = v
		d.save = func() error { return cl.SaveAdvancedSection(v) }
	default:
		id := strings.TrimPrefix(section, "advanced/")
		v, err := cl.AdvancedSection(id)
		if err != nil {
			return Editor{}, err
		}
		d.advanced = v
		d.save = func() error { return cl.SaveAdvancedSection(v) }
	}
	if d.advanced != nil {
		for i := 0; i < d.advanced.FieldCount(); i++ {
			f := d.advanced.FieldAt(i)
			if !visibleBasic(section, f.Path) {
				continue
			}
			d.fields = append(d.fields, Field{Path: f.Path, Label: f.Label, Group: f.Group, Kind: f.Kind, Value: f.Value,
				IsSecret: f.IsSecret, IsReadOnly: f.IsReadOnly, Options: Strings(f.Options()), IsAdvanced: desktopapi.FolderFieldAdvanced(section, f.Path)})
		}
	} else {
		v := reflect.ValueOf(d.value)
		if v.IsNil() {
			return Editor{}, errors.New("the settings are unavailable")
		}
		v = v.Elem()
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			f := Field{Path: sf.Name, Label: label(sf.Name), Kind: "string", Value: fmt.Sprint(v.Field(i).Interface()), IsAdvanced: desktopapi.FolderFieldAdvanced(section, sf.Name)}
			if v.Field(i).Kind() == reflect.Bool {
				f.Kind = "bool"
			}
			if v.Field(i).Kind() == reflect.Int {
				f.Kind = "int"
			}
			if strings.Contains(sf.Name, "Password") {
				f.IsSecret = true
				f.Value = ""
			}
			if sf.Name == "HasPassword" {
				f.IsReadOnly = true
				f.IsSecret = false
				f.Value = fmt.Sprint(v.Field(i).Interface())
			}
			if strings.Contains(sf.Name, "Networks") || strings.Contains(sf.Name, "Entries") || sf.Name == "Bypass" || sf.Name == "Patterns" || sf.Name == "FeatureFlags" {
				f.Kind = "list"
			}
			if strings.HasPrefix(section, "folder/") && (sf.Name == "Type" || sf.Name == "Path" || sf.Name == "RescanIntervalS" || sf.Name == "FSWatcherEnabled" || sf.Name == "FSWatcherDelayS" || sf.Name == "MaxConflicts" || sf.Name == "BlockIndexing") {
				continue
			}
			f.Options = options(section, sf.Name)
			if len(f.Options) > 0 {
				f.Kind = "enum"
				found := false
				for _, o := range f.Options {
					found = found || o == f.Value
				}
				if !found {
					f.Options = append(f.Options, f.Value)
				}
			}
			d.fields = append(d.fields, f)
		}
	}
	token := uuid.NewString()
	c.editors[token] = d
	return Editor{token, section, d.fields}, nil
}

// Keep fields untouched by this editor, including secrets, from the latest settings.
func mergeSettings[T any](current, baseline, edited *T) *T {
	if current == nil {
		return edited
	}
	result := *current
	out := reflect.ValueOf(&result).Elem()
	before := reflect.ValueOf(baseline).Elem()
	after := reflect.ValueOf(edited).Elem()
	for i := 0; i < out.NumField(); i++ {
		if out.Type().Field(i).IsExported() && !reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
			out.Field(i).Set(after.Field(i))
		}
	}
	return &result
}
func options(section, name string) []string {
	switch name {
	case "Compression":
		return []string{"metadata", "always", "never"}
	case "AuthMode":
		return []string{"static", "ldap"}
	case "LDAPTransport":
		return []string{"plain", "tls", "starttls"}
	case "Order":
		return []string{"random", "alphabetic", "smallestFirst", "largestFirst", "oldestFirst", "newestFirst"}
	case "BlockPullOrder":
		return []string{"standard", "random", "inOrder"}
	case "BlockCopyMethod":
		return []string{"standard", "all"}
	case "XattrMode":
		return []string{"syncable", "all"}
	case "MinDiskFreeUnit", "MinHomeDiskFreeUnit":
		return []string{"%", "B", "kB", "MB", "GB", "TB"}
	case "Type":
		if strings.Contains(section, "versioning") {
			return []string{"", "simple", "trashcan", "staggered", "external"}
		}
		return []string{"sendreceive", "sendonly", "receiveonly", "receiveencrypted"}
	}
	return nil
}
func (c *Control) saveSettings(p Request) (any, error) {
	d, ok := c.editors[p.Token]
	if !ok || time.Now().After(d.expires) {
		delete(c.editors, p.Token)
		return nil, errors.New("the editor expired; reopen the settings page")
	}
	// Validate every conversion first. A rejected save keeps the original draft usable.
	converted := map[string]reflect.Value{}
	for _, f := range d.fields {
		value, ok := p.Values[f.Path]
		if !ok || f.IsReadOnly || (f.IsSecret && value == "") {
			continue
		}
		if d.advanced == nil {
			dest := reflect.ValueOf(d.value).Elem().FieldByName(f.Path)
			v := reflect.New(dest.Type()).Elem()
			switch dest.Kind() {
			case reflect.Bool:
				b, e := strconv.ParseBool(value)
				if e != nil {
					return nil, fmt.Errorf("%s: enter true or false", f.Label)
				}
				v.SetBool(b)
			case reflect.Int:
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || v.OverflowInt(n) {
					return nil, fmt.Errorf("%s: enter a whole number", f.Label)
				}
				v.SetInt(n)
			default:
				v.SetString(value)
			}
			converted[f.Path] = v
		}
	}
	if d.advanced != nil {
		for i := 0; i < d.advanced.FieldCount(); i++ {
			f := d.advanced.FieldAt(i)
			if v, ok := p.Values[f.Path]; ok && !f.IsReadOnly && (!f.IsSecret || v != "") {
				f.Value = v
			}
		}
	} else {
		v := reflect.ValueOf(d.value).Elem()
		for key, value := range converted {
			v.FieldByName(key).Set(value)
		}
	}
	if err := d.save(); err != nil {
		message := err.Error()
		if field, rest, ok := strings.Cut(message, ":"); ok {
			for _, f := range d.fields {
				if strings.EqualFold(field, f.Path) {
					return nil, fmt.Errorf("%s:%s", f.Label, rest)
				}
			}
		}
		return nil, err
	}
	delete(c.editors, p.Token)
	return map[string]bool{"restartRequired": c.Client.IsEngineRestartRequired()}, nil
}

func visibleBasic(section, path string) bool {
	var fields string
	switch {
	case strings.HasPrefix(section, "basic/folder/"):
		fields = " label type rescanIntervalS fsWatcherEnabled fsWatcherDelayS maxConflicts blockIndexing "
	case strings.HasPrefix(section, "basic/device/"):
		fields = " name addresses introducer untrusted "
	case section == "basic/options":
		fields = " listenAddresses globalAnnounceEnabled globalAnnounceServers localAnnounceEnabled announceLANAddresses relaysEnabled natEnabled stunServers enoughConnections "
	case section == "bandwidth":
		fields = " maxRecvKbps maxSendKbps limitBandwidthInLan trafficClass "
	default:
		return true
	}
	return strings.Contains(fields, " "+path+" ")
}
