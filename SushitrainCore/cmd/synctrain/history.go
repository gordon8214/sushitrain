//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"encoding/json"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/history"
	"time"
)

func (d *desktopUI) historySources() {
	d.text("Include versions from gateways with the same Syncthing folder ID and file path. Read access is sufficient. Pairing for history does not mount folders.")
	d.button("Pair with a History Source…", func() { d.input("Pair with a History Source", "Pairing link from the gateway", "", d.pairHistory) })
	d.load("history.sources", nil, func(b json.RawMessage) {
		var list []history.Source
		if err := json.Unmarshal(b, &list); err != nil {
			d.error(err)
			return
		}
		g := d.group("History Sources")
		for _, source := range list {
			source := source
			r := adw.NewSwitchRow()
			r.SetUseMarkup(false)
			r.SetTitle(source.Name)
			r.SetSubtitle(source.Endpoint.BaseURL())
			r.SetActive(source.Enabled)
			r.NotifyProperty("active", func() {
				d.run("history.enable", history.Request{ID: source.Endpoint.ServiceID, Enabled: r.Active()}, nil)
			})
			g.Add(r)
			forget := row("Forget “"+source.Name+"”", "")
			forget.AddSuffix(textButton("Forget…", func() {
				d.w.confirm("Forget This History Source?", "Its archived files stay on the gateway.", "Forget", true, func() { d.run("history.forget", history.Request{ID: source.Endpoint.ServiceID}, nil) })
			}))
			g.Add(forget)
		}
		if len(list) == 0 {
			g.Add(row("No history sources", "Pair with a gateway that keeps versions of your files."))
		}
	})
}
func (d *desktopUI) pairLink(link string) {
	if d.mode == modeSynchronize || d.route == "History Sources" {
		d.pairHistory(link)
	} else {
		d.input("Pair with a Gateway", "Approve this machine on the gateway after continuing.", link, func(link string) { d.run("pair", map[string]string{"Link": link}, nil) })
	}
}
func (d *desktopUI) pairHistory(link string) {
	d.startView()
	dialog := adw.NewDialog()
	dialog.SetTitle("Pairing with a History Source")
	box := gtk.NewBox(gtk.OrientationVertical, 16)
	box.SetMarginTop(24)
	box.SetMarginBottom(24)
	box.SetMarginStart(24)
	box.SetMarginEnd(24)
	box.Append(gtk.NewLabel("Approve this machine on the gateway."))
	cancel := textButton("Cancel", func() { dialog.Close() })
	box.Append(cancel)
	dialog.SetChild(box)
	dialog.SetContentWidth(460)
	id := uuid.NewString()
	closed := false
	completed := false
	ctx, stop := context.WithCancel(d.ctx)
	dialog.ConnectClosed(func() {
		closed = true
		stop()
		go d.request(context.Background(), "history.cancel", history.Request{ID: id}, nil)
		if !completed && !d.closed {
			d.showRoute()
		}
	})
	dialog.Present(d.w.win)
	background(func() (json.RawMessage, error) {
		var result json.RawMessage
		err := d.request(ctx, "history.pair", history.Request{Link: link, ID: id}, &result)
		return result, err
	}, func(_ json.RawMessage, err error) {
		if closed {
			return
		}
		if err != nil {
			box.Append(gtk.NewLabel("Pairing could not finish: " + err.Error()))
			cancel.SetLabel("Close")
			return
		}
		completed = true
		dialog.Close()
		d.navigate("History Sources")
	})
}
func (d *desktopUI) appendRemoteVersions(folder, file, name string, item int64, excludeDevice, excludeService string) {
	d.fetch("history.list", history.Request{Folder: folder, Path: file}, func(b json.RawMessage, err error) {
		if err != nil {
			d.group("Other History Sources").Add(row("Could not load sources", err.Error()))
			return
		}
		var sources []history.Listing
		if err = json.Unmarshal(b, &sources); err != nil {
			d.error(err)
			return
		}
		for _, source := range sources {
			source := source
			if excludeService != "" && source.Source.Endpoint.ServiceID == excludeService || excludeDevice != "" && source.DeviceID == excludeDevice {
				continue
			}
			g := d.group(source.Source.Name)
			if source.Error != "" {
				g.Add(row("Could not load versions", source.Error))
				continue
			}
			if len(source.Versions) == 0 {
				g.Add(row("No archived versions", ""))
			}
			for _, v := range source.Versions {
				version := v
				r := row(time.Unix(0, version.ModifiedNs).Local().Format("January 2, 2006 3:04:05 PM"), humanBytes(version.Size))
				request := history.Request{ID: source.Source.Endpoint.ServiceID, Folder: folder, Path: file, Version: version.ID, ExpectedTag: version.EntityTag}
				r.AddSuffix(textButton("Preview", func() {
					d.run("history.download", request, func(b json.RawMessage) {
						var local string
						json.Unmarshal(b, &local)
						d.preview(local, name, version.Size)
					})
				}))
				r.AddSuffix(textButton("Save a Copy…", func() {
					d.pickSave(name, func(dest string) {
						copy := request
						copy.Destination = dest
						d.run("history.export", copy, func(json.RawMessage) { d.w.toast("Saved a copy.") })
					})
				}))
				r.AddSuffix(textButton("Restore…", func() {
					d.w.confirm("Restore This Version?", "The destination's current content is preserved. Changes made during the download prevent replacement.", "Restore", true, func() {
						captureMethod := "engine.captureRestore"
						var capture any = desktopapi.Request{Folder: folder, Path: file}
						if d.mode == modeFiles {
							captureMethod = "files.captureRestore"
							capture = map[string]any{"Item": item}
						}
						d.run(captureMethod, capture, func(b json.RawMessage) {
							var token string
							json.Unmarshal(b, &token)
							d.run("history.download", request, func(b json.RawMessage) {
								var local string
								json.Unmarshal(b, &local)
								method := "engine.restoreDownloaded"
								var p any = desktopapi.Request{Folder: folder, Path: file, Name: local, Token: token, Offset: version.ModifiedNs}
								if d.mode == modeFiles {
									method = "files.restoreDownloaded"
									p = map[string]any{"Token": token, "Path": local, "Modified": version.ModifiedNs}
								}
								d.run(method, p, func(json.RawMessage) {
									if d.mode == modeFiles {
										d.w.toast("Restore saved. Waiting for its gateway…")
										d.fetch("files.wait", map[string]any{"Item": item}, func(_ json.RawMessage, err error) {
											if err != nil {
												d.w.inform("Restore Is Pending", err.Error()+" Review Pending Changes before trying again.")
											} else {
												d.w.toast("Restored the selected version.")
											}
										})
									} else {
										d.w.toast("Restored the selected version.")
										d.refreshView()
									}
								})
							})
						})
					})
				}))
				g.Add(r)
			}
		}
	})
}
func (d *desktopUI) filesVersions(r browserRow) {
	d.subpage(func() {
		d.reset("Previous Versions — " + r.name)
		g := d.group("Folder Gateway")
		d.fetch("files.historyTarget", map[string]any{"Item": r.item}, func(b json.RawMessage, err error) {
			if err != nil {
				d.group("Other History Sources").Add(row("File history is unavailable", err.Error()))
				return
			}
			var target files.HistoryTarget
			json.Unmarshal(b, &target)
			d.appendRemoteVersions(target.Folder, target.Path, r.name, r.item, target.DeviceID, target.ServiceID)
		})
		d.fetch("files.versions", map[string]any{"Item": r.item}, func(b json.RawMessage, err error) {
			if err != nil {
				g.Add(row("Could not load gateway history", err.Error()))
				return
			}
			var list files.Versions
			json.Unmarshal(b, &list)
			if len(list.Versions) == 0 {
				g.Add(row("No versions on this gateway", "Other paired sources are listed separately."))
			}
			for _, v := range list.Versions {
				version := v
				row := row(version.ModTime.Local().Format("January 2, 2006 3:04:05 PM"), humanBytes(version.Size))
				row.AddSuffix(textButton("Preview", func() {
					d.run("files.versionPreview", map[string]any{"Item": r.item, "Version": version.ID, "Token": version.EntityTag}, func(b json.RawMessage) {
						var file string
						json.Unmarshal(b, &file)
						d.preview(file, r.name, version.Size)
					})
				}))
				row.AddSuffix(textButton("Save a Copy…", func() {
					d.pickSave(r.name, func(dest string) {
						d.run("files.versionExport", map[string]any{"Item": r.item, "Version": version.ID, "Token": version.EntityTag, "Path": dest}, func(json.RawMessage) { d.w.toast("Saved a copy.") })
					})
				}))
				if list.Writable {
					row.AddSuffix(textButton("Restore as Copy", func() {
						d.run("files.versionCopy", map[string]any{"Item": r.item, "Version": version.ID, "Token": version.EntityTag}, func(json.RawMessage) { d.w.toast("Saved a copy on the gateway.") })
					}))
					row.AddSuffix(textButton("Restore…", func() {
						d.w.confirm("Restore This Version?", "Its gateway preserves the current version before replacement.", "Restore", true, func() {
							d.run("files.versionRestore", map[string]any{"Item": r.item, "Version": version.ID, "Token": version.EntityTag}, func(json.RawMessage) { d.w.toast("Restored the selected version."); d.refreshView() })
						})
					}))
				}
				g.Add(row)
			}
		})
	})
}
