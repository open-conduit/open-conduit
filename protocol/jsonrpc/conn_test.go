package jsonrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// pair connects two peers over pipes.
func pair(t *testing.T, a, b jsonrpc.Handler, o jsonrpc.Options) (*jsonrpc.Conn, *jsonrpc.Conn) {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	ca := jsonrpc.NewConn(ar, aw, a, o)
	cb := jsonrpc.NewConn(br, bw, b, o)
	t.Cleanup(func() { _ = ca.Close(); _ = cb.Close() })
	return ca, cb
}

func none(context.Context, string, json.RawMessage) (any, error) {
	return nil, protocol.Errorf(protocol.CodeMethodNotFound, "none")
}

func TestNestedCallbacksAndErrors(t *testing.T) {
	var host, ext2 *jsonrpc.Conn
	ext := func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "echo":
			// A nested callback to the host while serving its request.
			var out string
			if err := ext2.Call(ctx, "kv.get", nil, &out); err != nil {
				return nil, err
			}
			return out, nil
		case "refuse":
			return nil, &extension.OperationError{Code: "already_merged", Message: "carry on"}
		case "adopt":
			return nil, extension.ErrNotAdoptable
		case "leak":
			return nil, errors.New("password=hunter2")
		}
		return nil, protocol.Errorf(protocol.CodeMethodNotFound, method)
	}
	hostHandler := func(context.Context, string, json.RawMessage) (any, error) { return "value", nil }
	host, ext2 = pair(t, hostHandler, ext, jsonrpc.Options{})
	var out string
	if err := host.Call(context.Background(), "echo", nil, &out); err != nil || out != "value" {
		t.Fatalf("nested callback: %q %v", out, err)
	}
	var oe *extension.OperationError
	if err := host.Call(context.Background(), "refuse", nil, nil); !errors.As(err, &oe) || oe.Code != "already_merged" {
		t.Fatalf("operation error: %v", err)
	}
	if err := host.Call(context.Background(), "adopt", nil, nil); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("sentinel: %v", err)
	}
	if err := host.Call(context.Background(), "leak", nil, nil); protocol.ErrorCode(err) != protocol.CodeInternal || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("unauthored error: %v", err)
	}
	if err := host.Call(context.Background(), "missing", nil, nil); protocol.ErrorCode(err) != protocol.CodeMethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestCancellationReachesTheHandler(t *testing.T) {
	seen := make(chan struct{})
	ext := func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		close(seen)
		return nil, ctx.Err()
	}
	host, _ := pair(t, none, ext, jsonrpc.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Call(ctx, "slow", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v", err)
	}
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation never reached the handler")
	}
}

func TestOverloadRefusesWithoutBlockingTheReader(t *testing.T) {
	release := make(chan struct{})
	ext := func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "block" {
			<-release
		}
		return "ok", nil
	}
	host, _ := pair(t, none, ext, jsonrpc.Options{MaxInflight: 2})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = host.Call(context.Background(), "block", nil, nil) }()
	}
	time.Sleep(50 * time.Millisecond)
	if err := host.Call(context.Background(), "quick", nil, nil); protocol.ErrorCode(err) != protocol.CodeOverloaded {
		t.Fatalf("over the bound: %v, want overloaded", err)
	}
	close(release)
	wg.Wait()
	if err := host.Call(context.Background(), "quick", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// A malformed, truncated or oversized message closes the connection and
// fails pending calls with ErrTransport, never a result or a hang.
func TestMalformedStreamsCloseTheConnection(t *testing.T) {
	for name, line := range map[string]string{
		"invalid_json": "not json\n",
		"truncated":    `{"jsonrpc":"2.0","id":1,"res`,
		"oversized":    `{"jsonrpc":"2.0","method":"x","params":"` + strings.Repeat("a", 2048) + `"}` + "\n",
		"not_jsonrpc":  `{"id":1,"result":1}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			pr, pw := io.Pipe()
			_, sink := io.Pipe()
			var protocolErr error
			var mu sync.Mutex
			c := jsonrpc.NewConn(pr, sink, none, jsonrpc.Options{MaxMessage: 1024, OnProtocolError: func(err error) { mu.Lock(); protocolErr = err; mu.Unlock() }})
			done := make(chan error, 1)
			go func() { done <- c.Call(context.Background(), "pending", nil, nil) }()
			time.Sleep(10 * time.Millisecond)
			go func() { _, _ = pw.Write([]byte(line)); _ = pw.Close() }()
			select {
			case err := <-done:
				if !errors.Is(err, protocol.ErrTransport) {
					t.Fatalf("pending call: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("hung")
			}
			<-c.Done()
			mu.Lock()
			defer mu.Unlock()
			if name != "truncated" && protocolErr == nil {
				t.Fatal("protocol error not reported")
			}
		})
	}
}
