// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// newSyncView is Synchronize mode
func newSyncView(w *mainWindow) (gtk.Widgetter, func()) {
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	page := adw.NewStatusPage()
	page.SetTitle("Synchronize")
	toolbar.SetContent(page)
	return toolbar, func() {}
}

// setUpSynchronize starts Synchronize mode
func (w *mainWindow) setUpSynchronize() {
	w.toast("Synchronize mode isn't available yet")
}
