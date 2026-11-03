package adminsock

import (
	"context"
	"testing"
)

func TestWindowsPipeExclusive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := testEndpoint(t)
	first := NewServer()
	if err := first.Listen(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	other := NewServer()
	if err := other.Listen(ctx, path, false); err == nil {
		other.Close()
		t.Fatal("took over an active named pipe")
	}
	if err := NewServer().Listen(ctx, testEndpoint(t), true); err == nil {
		t.Fatal("enabled group access on a private user pipe")
	}
}
