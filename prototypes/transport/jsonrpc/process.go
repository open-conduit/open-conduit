package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/prototypes/transport/scenario"
)

// Start launches binary with the protocol on its stdin/stdout, forwards its
// stderr (diagnostics only) and performs the initialize handshake.
func Start(ctx context.Context, binary string, env []string, versions []int, host scenario.HostHandler) (scenario.Peer, error) {
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	p := &peer{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	p.conn = NewConn(stdout, stdin, Handler(host))
	go func() { _ = cmd.Wait(); close(p.exited) }()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := p.conn.Call(hctx, "initialize", map[string]any{"versions": versions})
	if err != nil {
		_ = p.Close()
		var se *scenario.Error
		if errors.As(err, &se) && se.Code == "unsupported_protocol_version" {
			return nil, scenario.ErrIncompatible
		}
		return nil, err
	}
	p.manifest = raw
	return p, nil
}

type peer struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	conn     *Conn
	manifest json.RawMessage
	exited   chan struct{}
}

func (p *peer) Manifest() json.RawMessage { return p.manifest }
func (p *peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.conn.Call(ctx, method, params)
}

// Close closes stdin (the extension's signal to drain and exit), then
// escalates to SIGTERM and SIGKILL.
func (p *peer) Close() error {
	_ = p.stdin.Close()
	for _, sig := range []os.Signal{nil, syscall.SIGTERM, os.Kill} {
		if sig != nil {
			_ = p.cmd.Process.Signal(sig)
		}
		select {
		case <-p.exited:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("extension did not exit")
}

// Serve runs an extension on its own stdin/stdout until the host closes
// stdin. It moves os.Stdout to stderr first, so a stray print in extension
// code cannot corrupt the protocol stream.
func Serve(name string, h func(ctx context.Context, method string, params json.RawMessage, host scenario.Caller) (any, error)) {
	protocol := os.Stdout
	os.Stdout = os.Stderr
	var conn *Conn
	conn = NewConn(os.Stdin, protocol, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			var p struct{ Versions []int }
			_ = json.Unmarshal(params, &p)
			v, err := scenario.Negotiate(p.Versions, scenario.SupportedVersions())
			if err != nil {
				return nil, &scenario.Error{Code: "unsupported_protocol_version", Message: "no common protocol version"}
			}
			return scenario.Manifest{Name: name, Protocol: v, Provides: []string{"echo", "kv", "artifact", "sleep", "push"}}, nil
		}
		return h(ctx, method, params, conn.Call)
	})
	<-conn.Done()
}
