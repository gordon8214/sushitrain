// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package desktoptray implements an optional StatusNotifierItem without linking
// a second GTK version or requiring a particular desktop shell extension.
package desktoptray

import (
	"context"
	"fmt"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
	"os"
	"time"
)

const itemInterface = "org.kde.StatusNotifierItem"
const menuInterface = "com.canonical.dbusmenu"

type Action struct {
	Label string
	Run   func()
}
type Tray struct {
	conn       *dbus.Conn
	actions    []Action
	cancel     context.CancelFunc
	properties *prop.Properties
}
type Layout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}
type PropertyGroup struct {
	ID         int32
	Properties map[string]dbus.Variant
}
type Pixmap struct {
	Width, Height int32
	Data          []byte
}
type ToolTip struct {
	IconName           string
	Pixmaps            []Pixmap
	Title, Description string
}

func New(actions []Action) (*Tray, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Tray{conn: conn, actions: actions, cancel: cancel}
	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		cancel()
		if err == nil {
			err = fmt.Errorf("the previous tray item is still closing; try again")
		}
		return nil, err
	}
	if err = conn.Export(t, "/StatusNotifierItem", itemInterface); err != nil {
		t.Close()
		return nil, err
	}
	if err = conn.Export(t, "/MenuBar", menuInterface); err != nil {
		t.Close()
		return nil, err
	}
	t.properties, err = prop.Export(conn, "/StatusNotifierItem", prop.Map{itemInterface: {"Category": {Value: "ApplicationStatus"}, "Id": {Value: "synctrain"}, "Title": {Value: "Synctrain"}, "Status": {Value: "Active", Emit: prop.EmitTrue}, "IconName": {Value: "com.gordonbyrnes.Synctrain"}, "IconPixmap": {Value: []Pixmap{}}, "WindowId": {Value: uint32(0)}, "OverlayIconName": {Value: ""}, "OverlayIconPixmap": {Value: []Pixmap{}}, "AttentionIconName": {Value: ""}, "AttentionIconPixmap": {Value: []Pixmap{}}, "ToolTip": {Value: ToolTip{Title: "Synctrain", Pixmaps: []Pixmap{}}}, "ItemIsMenu": {Value: false}, "Menu": {Value: dbus.ObjectPath("/MenuBar")}}})
	if err != nil {
		t.Close()
		return nil, err
	}
	_, err = prop.Export(conn, "/MenuBar", prop.Map{menuInterface: {"Version": {Value: uint32(3)}, "TextDirection": {Value: "ltr"}, "Status": {Value: "normal"}, "IconThemePath": {Value: []string{}}}})
	if err != nil {
		t.Close()
		return nil, err
	}
	for _, obj := range []struct {
		path  dbus.ObjectPath
		iface string
	}{{"/StatusNotifierItem", itemInterface}, {"/MenuBar", menuInterface}} {
		node := &introspect.Node{Interfaces: []introspect.Interface{introspect.IntrospectData, prop.IntrospectData, {Name: obj.iface, Methods: introspect.Methods(t)}}}
		conn.Export(introspect.NewIntrospectable(node), obj.path, "org.freedesktop.DBus.Introspectable")
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		owner := ""
		for {
			var next string
			conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.kde.StatusNotifierWatcher").Store(&next)
			if next != "" && next != owner {
				call := conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").CallWithContext(ctx, "org.kde.StatusNotifierWatcher.RegisterStatusNotifierItem", 0, name)
				if call.Err == nil {
					owner = next
				}
			} else if next == "" {
				owner = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return t, nil
}
func (t *Tray) Close() { t.cancel(); t.conn.Close() }
func (t *Tray) Activate(x, y int32) *dbus.Error {
	if len(t.actions) > 0 {
		t.actions[0].Run()
	}
	return nil
}
func (t *Tray) SecondaryActivate(x, y int32) *dbus.Error           { return t.Activate(x, y) }
func (t *Tray) ContextMenu(x, y int32) *dbus.Error                 { return nil }
func (t *Tray) Scroll(delta int32, orientation string) *dbus.Error { return nil }
func (t *Tray) GetLayout(parent, depth int32, properties []string) (uint32, Layout, *dbus.Error) {
	root := Layout{ID: 0, Properties: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, Children: []dbus.Variant{}}
	for i, a := range t.actions {
		child := Layout{ID: int32(i + 1), Properties: map[string]dbus.Variant{"label": dbus.MakeVariant(a.Label), "enabled": dbus.MakeVariant(true), "visible": dbus.MakeVariant(true)}, Children: []dbus.Variant{}}
		if parent == child.ID {
			return 1, child, nil
		}
		root.Children = append(root.Children, dbus.MakeVariant(child))
	}
	return 1, root, nil
}
func (t *Tray) GetGroupProperties(ids []int32, names []string) ([]PropertyGroup, *dbus.Error) {
	out := []PropertyGroup{}
	for _, id := range ids {
		_, l, _ := t.GetLayout(id, 0, names)
		out = append(out, PropertyGroup{id, l.Properties})
	}
	return out, nil
}
func (t *Tray) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	_, l, _ := t.GetLayout(id, 0, nil)
	if v, ok := l.Properties[name]; ok {
		return v, nil
	}
	return dbus.MakeVariant(""), nil
}
func (t *Tray) Event(id int32, event string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if event == "clicked" && id > 0 && int(id) <= len(t.actions) {
		t.actions[id-1].Run()
	}
	return nil
}
func (t *Tray) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }
