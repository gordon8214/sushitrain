// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build darwin

package hostbridge

import (
	"context"
	"errors"
	"os"
	core "t-shaped.nl/sushitrain/v2/src"
	"testing"
	"time"
)

type pauseDelegate struct{ entered chan *Cancellation }

func (d *pauseDelegate) Call(method string, _ []byte, cancellation *Cancellation) ([]byte, error) {
	if method == "native.methods" {
		return []byte("[]"), nil
	}
	if method != "native.pause" {
		return nil, errors.New("unexpected native call")
	}
	d.entered <- cancellation
	<-cancellation.ctx.Done()
	return nil, cancellation.ctx.Err()
}

func TestPauseUsesOperationCancellation(t *testing.T) {
	directory, err := os.MkdirTemp("", "mcp-pause-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	delegate := &pauseDelegate{entered: make(chan *Cancellation, 1)}
	server, err := NewServer(&core.Client{}, delegate, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.engine.Pause(ctx, []string{"peer"}, true) }()
	var cancellation *Cancellation
	select {
	case cancellation = <-delegate.entered:
	case <-time.After(time.Second):
		t.Fatal("pause did not enter native adapter")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !cancellation.IsCanceled() {
			t.Fatal("operation context was lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceling the operation did not cancel native pause")
	}
}
