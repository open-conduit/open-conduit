package extproc

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// TestCallAdmittedAsksAgainAfterARefusal: overloaded means the request was
// not served, so the host repeats it. Call, the negative control, returns
// the refusal, and any other error is not repeated.
func TestCallAdmittedAsksAgainAfterARefusal(t *testing.T) {
	var calls atomic.Int32
	var refusals atomic.Int32
	ext := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		calls.Add(1)
		if method == "executor.forget" {
			return nil, protocol.Errorf(protocol.CodeConflict, "no")
		}
		if refusals.Add(-1) >= 0 {
			return nil, protocol.Errorf(protocol.CodeOverloaded, "busy")
		}
		return struct{}{}, nil
	}
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	host := jsonrpc.NewConn(ar, aw, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }, jsonrpc.Options{})
	peer := jsonrpc.NewConn(br, bw, ext, jsonrpc.Options{})
	t.Cleanup(func() { _ = host.Close(); _ = peer.Close() })
	p := &Process{conn: host, enabled: map[string]bool{"executor.stop": true, "executor.forget": true}}
	p.o.MinBackoff = time.Microsecond
	ctx := context.Background()

	refusals.Store(1)
	if err := p.Call(ctx, "executor.stop", nil, nil); protocol.ErrorCode(err) != protocol.CodeOverloaded {
		t.Fatalf("call: %v, want overloaded", err)
	}
	refusals.Store(3)
	calls.Store(0)
	if err := p.CallAdmitted(ctx, "executor.stop", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("%d calls, want 4", got)
	}
	calls.Store(0)
	if err := p.CallAdmitted(ctx, "executor.forget", nil, nil); protocol.ErrorCode(err) != protocol.CodeConflict {
		t.Fatalf("forget: %v, want conflict", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d calls of a definite refusal, want 1", got)
	}
}
