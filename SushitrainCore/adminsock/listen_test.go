package adminsock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCall(t *testing.T) {
	path := testEndpoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer()
	server.Register("echo", func(ctx context.Context, params json.RawMessage) (any, error) {
		var p struct{ Text string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Text == "" {
			return nil, errors.New("nothing to echo")
		}
		return map[string]string{"text": p.Text}, nil
	})
	if err := server.Listen(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var out struct{ Text string }
	if err := Call(path, "echo", map[string]string{"text": "hi"}, &out); err != nil || out.Text != "hi" {
		t.Fatalf("echo: %v, %+v", err, out)
	}
	if err := Call(path, "echo", map[string]string{}, nil); err == nil || err.Error() != "nothing to echo" {
		t.Fatalf("method error came back as %v", err)
	}
	if err := Call(path, "missing", nil, nil); err == nil {
		t.Fatal("an unknown method succeeded")
	}
}
