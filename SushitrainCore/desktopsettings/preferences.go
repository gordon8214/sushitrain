// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package desktopsettings owns Linux desktop preferences for both native UI and MCP.
package desktopsettings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
)

type Preferences struct {
	HideDotfiles                bool
	PreviewImages, PreviewMedia bool
	ImageLimitMiB               int
	Tray, Grid                  bool
}

func Path() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "synctrain", "desktop.json")
}
func Read(path string) (Preferences, error) {
	p := Preferences{HideDotfiles: true, PreviewImages: true, PreviewMedia: true, ImageLimitMiB: 16}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.ImageLimitMiB < 1 || p.ImageLimitMiB > 100 {
		p.ImageLimitMiB = 16
	}
	return p, nil
}
func Values(p Preferences) map[string]string {
	out := map[string]string{}
	v := reflect.ValueOf(p)
	for i := 0; i < v.NumField(); i++ {
		out[v.Type().Field(i).Name] = fmt.Sprint(v.Field(i).Interface())
	}
	return out
}

// Update merges only edited fields against the latest persisted value. The
// cross-process lock also coordinates the native window with its background daemon.
func Update(ctx context.Context, path string, baseline Preferences, changes map[string]string) (Preferences, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Preferences{}, err
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return Preferences{}, err
	}
	if !locked {
		return Preferences{}, errors.New("desktop settings are busy")
	}
	defer lock.Unlock()
	current, err := Read(path)
	if err != nil {
		return current, err
	}
	before, now := Values(baseline), Values(current)
	result := current
	value := reflect.ValueOf(&result).Elem()
	for key, text := range changes {
		field := value.FieldByName(key)
		if !field.IsValid() {
			return current, fmt.Errorf("unknown desktop setting %s", key)
		}
		if now[key] != before[key] && now[key] != text {
			return current, errors.New("desktop settings changed; refresh before saving")
		}
		switch field.Kind() {
		case reflect.Bool:
			if text != "true" && text != "false" {
				return current, fmt.Errorf("%s: enter true or false", key)
			}
			field.SetBool(text == "true")
		case reflect.Int:
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 || n > 100 {
				return current, errors.New("image preview size must be 1–100 MiB")
			}
			field.SetInt(int64(n))
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".desktop-")
	if err != nil {
		return current, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(result)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return result, err
}
func Register(a *adminsock.Server, path string) {
	type editor struct {
		value   Preferences
		expires time.Time
	}
	var mu sync.Mutex
	editors := map[string]editor{}
	for _, method := range []string{"get", "save", "close"} {
		a.Register("mcp.preferences."+method, func(ctx context.Context, b json.RawMessage) (any, error) {
			var p desktopapi.Request
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			for token, e := range editors {
				if time.Now().After(e.expires) {
					delete(editors, token)
				}
			}
			switch method {
			case "get":
				if p.Section != "app" && p.Section != "files" {
					return nil, errors.New("choose app or files desktop settings")
				}
				if len(editors) >= 64 {
					return nil, errors.New("close an unused desktop settings editor")
				}
				value, err := Read(path)
				if err != nil {
					return nil, err
				}
				token := "app:" + uuid.NewString()
				editors[token] = editor{value, time.Now().Add(30 * time.Minute)}
				fields := []desktopapi.Field{}
				for key, value := range Values(value) {
					kind := "bool"
					if key == "ImageLimitMiB" {
						kind = "int"
					}
					fields = append(fields, desktopapi.Field{Path: key, Label: key, Kind: kind, Value: value})
				}
				return desktopapi.Editor{Token: token, Section: "app", Fields: fields}, nil
			case "close":
				delete(editors, p.Token)
				return nil, nil
			case "save":
				e, ok := editors[p.Token]
				if !ok {
					return nil, errors.New("the desktop editor expired; reopen settings")
				}
				value, err := Update(ctx, path, e.value, p.Values)
				if err != nil {
					return nil, err
				}
				delete(editors, p.Token)
				return value, nil
			}
			return nil, errors.New("unsupported desktop settings operation")
		})
	}
}
