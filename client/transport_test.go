package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A request that never reaches the daemon is ErrUnreachable; one that ran out
// of the caller's time, or whose connection broke mid-answer, is not, so a
// slow operation is never reported as a daemon that is down.
func TestTransportErrorsSayWhatFailed(t *testing.T) {
	dir, err := os.MkdirTemp("", "occl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dial := func(t *testing.T, socket string) *Client {
		t.Helper()
		c, err := New("unix://"+socket, "tok")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	serve := func(t *testing.T, name string, h http.HandlerFunc) string {
		t.Helper()
		socket := filepath.Join(dir, name)
		ln, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return socket
	}

	t.Run("no_socket", func(t *testing.T) {
		err := dial(t, filepath.Join(dir, "missing")).Do(context.Background(), "POST", "toolbelt/call", nil, map[string]string{}, "", nil)
		if !errors.Is(err, ErrUnreachable) || !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("err = %v, want ErrUnreachable wrapping ENOENT", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		// The handler is never reached: the context has already ended, as
		// it has when an operation outlasts the caller's deadline.
		socket := serve(t, "slow", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer cancel()
		err := dial(t, socket).Do(ctx, "POST", "toolbelt/call", nil, map[string]string{}, "", nil)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want DeadlineExceeded and not ErrUnreachable", err)
		}
	})
	t.Run("broken_mid_answer", func(t *testing.T) {
		socket := serve(t, "broken", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		err := dial(t, socket).Do(context.Background(), "POST", "toolbelt/call", nil, map[string]string{}, "", nil)
		if err == nil || errors.Is(err, ErrUnreachable) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a transport error that is neither unreachable nor a deadline", err)
		}
	})
}
