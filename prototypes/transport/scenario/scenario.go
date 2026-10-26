// Package scenario is the shared workload of the 07-01 transport
// compatibility prototype. The same extension-side methods and host-side
// callbacks run over both candidate transports (JSON-RPC 2.0 over stdio and
// go-plugin over gRPC), so the comparison measures the transports, not two
// different programs. It is evidence for a protocol decision, not production
// code: nothing in the main module imports it.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrTransportClosed is what a host sees when the extension process or its
// connection is gone (crash, exit, broken pipe). It is distinct from a
// structured Error, which the extension authored.
var ErrTransportClosed = errors.New("transport closed")

// ErrIncompatible is a handshake refusal: no common protocol version.
var ErrIncompatible = errors.New("incompatible protocol version")

// Error is a structured, extension-authored refusal with a stable code. Both
// transports must carry it verbatim (the OperationError shape).
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Caller sends one request to the other side and waits for its result.
type Caller func(ctx context.Context, method string, params any) (json.RawMessage, error)

// Peer is the host-side view of one running extension process.
type Peer interface {
	// Manifest is what the extension returned during the handshake.
	Manifest() json.RawMessage
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Close stops the process (gracefully, then forcibly) and releases it.
	Close() error
}

// Starter launches binary as an extension whose host callbacks are served by
// host, and completes the handshake offering versions.
type Starter func(ctx context.Context, binary string, env []string, versions []int, host HostHandler) (Peer, error)

// HostHandler serves extension-to-host calls (KV, artifacts, push).
type HostHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Manifest is the prototype's minimal manifest, returned by the handshake.
type Manifest struct {
	Name     string   `json:"name"`
	Protocol int      `json:"protocol"`
	Provides []string `json:"provides"`
}

// SupportedVersions reads PROTO_VERSIONS (comma-separated, default "1"),
// which tests use to force a version mismatch.
func SupportedVersions() []int {
	raw := os.Getenv("PROTO_VERSIONS")
	if raw == "" {
		return []int{1}
	}
	var out []int
	for _, p := range strings.Split(raw, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// Negotiate picks the highest version both sides support.
func Negotiate(offered, supported []int) (int, error) {
	best := 0
	for _, o := range offered {
		for _, s := range supported {
			if o == s && o > best {
				best = o
			}
		}
	}
	if best == 0 {
		return 0, ErrIncompatible
	}
	return best, nil
}

// cancelledAt is the extension-side time (unix nanos) at which the most recent
// "sleep" observed its context being cancelled; the host reads it via "stats".
var cancelledAt atomic.Int64

// Extension is the extension-side dispatcher shared by both transports.
func Extension(ctx context.Context, method string, params json.RawMessage, host Caller) (any, error) {
	switch method {
	case "echo":
		return params, nil
	case "fail":
		return nil, &Error{Code: "not_found", Message: "the item does not exist"}
	case "sleep":
		var p struct{ MS int }
		_ = json.Unmarshal(params, &p)
		select {
		case <-ctx.Done():
			cancelledAt.Store(time.Now().UnixNano())
			return nil, &Error{Code: "cancelled", Message: "request cancelled"}
		case <-time.After(time.Duration(p.MS) * time.Millisecond):
			return "slept", nil
		}
	case "stats":
		return map[string]int64{"cancelled_at": cancelledAt.Load()}, nil
	case "kv":
		// N sequential extension-to-host calls while serving one host call:
		// the shape of a tool handler or projector reading its namespace.
		var p struct{ N int }
		_ = json.Unmarshal(params, &p)
		sum := 0
		for i := 0; i < p.N; i++ {
			raw, err := host(ctx, "kv.get", map[string]any{"namespace": "proto", "key": strconv.Itoa(i)})
			if err != nil {
				return nil, err
			}
			var v struct{ Value int }
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			sum += v.Value
		}
		return sum, nil
	case "artifact":
		// Chunked upload with one acknowledgement per chunk: the
		// request/response pair is the flow control.
		var p struct{ Bytes, Chunk int }
		_ = json.Unmarshal(params, &p)
		chunk := make([]byte, p.Chunk)
		for i := range chunk {
			chunk[i] = byte(i)
		}
		sent := 0
		for sent < p.Bytes {
			n := min(p.Chunk, p.Bytes-sent)
			if _, err := host(ctx, "artifact.write", map[string]any{"data": chunk[:n]}); err != nil {
				return nil, err
			}
			sent += n
		}
		return sent, nil
	case "push":
		// An extension-initiated delivery (a socket receiver): the
		// extension acknowledges upstream only after the host's answer.
		raw, err := host(ctx, "ingress.push", map[string]any{"delivery": "d-1"})
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case "print":
		// Stray output from extension code (a library logging to stdout)
		// must not corrupt the protocol stream.
		fmt.Println("stray stdout line")
		return "printed", nil
	case "crash":
		os.Exit(3)
	}
	return nil, &Error{Code: "method_not_found", Message: fmt.Sprintf("unknown method %q", method)}
}

// HostStats counts host-side callback traffic.
type HostStats struct {
	KVGets, ArtifactBytes, Pushes atomic.Int64
}

// Host returns the host-side callback handler used by every scenario.
func Host(stats *HostStats) HostHandler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "kv.get":
			var p struct{ Namespace, Key string }
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			if p.Namespace != "proto" {
				return nil, &Error{Code: "forbidden", Message: "namespace not granted"}
			}
			i, _ := strconv.Atoi(p.Key)
			stats.KVGets.Add(1)
			return map[string]int{"value": i * 2}, nil
		case "artifact.write":
			var p struct{ Data []byte }
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			stats.ArtifactBytes.Add(int64(len(p.Data)))
			return map[string]bool{"ok": true}, nil
		case "ingress.push":
			stats.Pushes.Add(1)
			return map[string]bool{"accepted": true}, nil
		}
		return nil, &Error{Code: "method_not_found", Message: "unknown host method " + method}
	}
}
