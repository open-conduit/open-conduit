package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// GET /work/{id}/terminal: an operator's terminal on a Work's session,
// streamed over a WebSocket. The host runs the attach process beside the run
// under a pseudo-terminal, so the operator can be on any machine. Binary
// messages carry the terminal's bytes; text messages carry control
// (TerminalControl). The session holds the Work; its end, however it comes,
// releases the hold.

// TerminalSpec is the process a terminal runs. Contained says it is a
// container provider's attach command, which may need the host's connection
// to that provider; Env is what the provider asked for.
type TerminalSpec struct {
	Argv       []string
	Dir        string
	Env        []string
	Term       string
	Cols, Rows uint16
	Contained  bool
}

// TerminalProcess is a running terminal process. Read and Write are its
// terminal; Wait returns its exit code once it ends; Close kills it and
// releases the terminal, and is safe to call more than once.
type TerminalProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() int
}

// TerminalControl is a text message on the stream: "resize" from the client
// with Cols and Rows, "exit" from the host with the process's Code.
type TerminalControl struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Code int    `json:"code"`
}

const (
	// terminalPing is how often the host checks that the client is still
	// there, so one lost without a close still ends its session.
	terminalPing      = 20 * time.Second
	terminalPingWait  = 15 * time.Second
	terminalWriteWait = 30 * time.Second
	terminalDrain     = 2 * time.Second
	terminalCleanup   = 2 * time.Minute
	terminalReadLimit = 64 << 10
	terminalMaxSize   = 1000
)

var terminalName = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

func terminalSize(q string, fallback uint16) (uint16, error) {
	if q == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 || n > terminalMaxSize {
		return 0, fail(400, "malformed", "cols and rows must be between 1 and 1000")
	}
	return uint16(n), nil
}

// terminalTarget is what the terminal will run: the executor's attach
// command, or for Work that ran in a container a new attach container. It
// applies attach's own confinement rules: a sandboxed run is refused, and a
// contained one is only ever reached inside a container. ref is the attach
// container to remove afterwards.
func (s *Server) terminalTarget(c *call) (spec TerminalSpec, ref string, w domain.Work, err error) {
	t, err := s.attachTarget(c)
	if err != nil {
		return spec, "", w, err
	}
	w = t.work
	if t.sandboxed {
		return spec, "", w, ErrAttachSandboxed
	}
	got, err := s.o.Attach(c.ctx, t.executor, t.work)
	if err != nil {
		return spec, "", w, err
	}
	if len(got.Argv) == 0 {
		return spec, "", w, ErrNothingToAttach
	}
	if !t.contained {
		if ContainerAttachCommand(got.Argv) {
			return spec, "", w, fail(409, "not_contained", "the Work did not run in a container")
		}
		return TerminalSpec{Argv: got.Argv, Dir: got.Dir}, "", w, nil
	}
	if !ContainerAttachCommand(got.Argv) {
		return spec, "", w, ErrAttachSandboxed
	}
	if s.o.ContainerAttach == nil {
		return spec, "", w, ErrAttachUnsupported
	}
	a, err := s.o.ContainerAttach(c.ctx, t.executor, t.work, t.execution)
	if errors.Is(err, extension.ErrSandboxUnavailable) || errors.Is(err, extension.ErrSandboxDenied) || errors.Is(err, extension.ErrContainerRunning) {
		return spec, "", w, fail(422, "container_unavailable", err.Error())
	} else if err != nil {
		return spec, "", w, err
	}
	return TerminalSpec{Argv: a.Argv, Env: a.Env, Contained: true}, a.Ref, w, nil
}

// noTerminal refuses a local attach while a terminal is open on the Work.
func (s *Server) noTerminal(c *call) error {
	return s.read(c.ctx, func(tx storage.Tx) error {
		if _, open, e := runtime.TerminalInTx(tx, domain.ID(c.params[0])); e != nil {
			return e
		} else if open {
			return runtime.ErrTerminalOpen
		}
		return nil
	})
}

func (s *Server) openTerminal(c *call) (int, any, error) {
	if s.o.StartTerminal == nil || s.o.Attach == nil {
		return 0, nil, ErrAttachUnsupported
	}
	if !strings.EqualFold(c.r.Header.Get("Upgrade"), "websocket") {
		return 0, nil, fail(426, "upgrade_required", "this endpoint serves a WebSocket")
	}
	q := c.r.URL.Query()
	cols, err := terminalSize(q.Get("cols"), 80)
	if err != nil {
		return 0, nil, err
	}
	rows, err := terminalSize(q.Get("rows"), 24)
	if err != nil {
		return 0, nil, err
	}
	term := q.Get("term")
	if term == "" {
		term = "xterm-256color"
	} else if !terminalName.MatchString(term) {
		return 0, nil, fail(400, "malformed", "term is not a terminal type")
	}
	keep := q.Get("keep_held") == "true"

	session, err := s.o.Control.OpenTerminal(c.ctx, domain.ID(c.params[0]), strings.TrimPrefix(requestID(), "req_"))
	if err != nil {
		return 0, nil, err
	}
	c.note("session", session.ID)
	var (
		ref    string
		work   domain.Work
		reason = "operator detached"
	)
	// Cleanup outlives the request: its context ends with the client.
	defer func() {
		cx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), terminalCleanup)
		defer cancel()
		hold := keep
		if ref != "" && s.o.ContainerDetach != nil {
			if e := s.o.ContainerDetach(cx, work, ref); e != nil {
				// The container may still be running on the session, so the
				// Work stays held until an operator has looked.
				hold, reason = true, "attach container not removed: "+e.Error()
			}
		}
		_ = s.o.Control.CloseTerminal(cx, session, reason, hold)
	}()

	spec, containerRef, w, err := s.terminalTarget(c)
	ref, work = containerRef, w
	if err != nil {
		reason = "attach refused"
		return 0, nil, err
	}
	spec.Term, spec.Cols, spec.Rows = term, cols, rows
	proc, err := s.o.StartTerminal(c.ctx, spec)
	if err != nil {
		reason = "terminal did not start"
		return 0, nil, err
	}
	defer proc.Close()

	conn, err := websocket.Accept(c.w, c.r, nil)
	c.written = true
	if err != nil {
		reason = "client did not connect"
		return 0, nil, nil
	}
	s.auditAfter(c.ctx, c.auditRecord(domain.AuditOK))
	c.recorded = true
	conn.SetReadLimit(terminalReadLimit)
	if code, exited := s.relayTerminal(c.ctx, conn, proc); exited {
		reason = "session ended (exit " + strconv.Itoa(code) + ")"
		_ = conn.Close(websocket.StatusNormalClosure, "")
	} else {
		reason = "client disconnected"
		_ = conn.CloseNow()
	}
	return 0, nil, nil
}

// relayTerminal copies the terminal to and from conn until the process
// exits (exited, with its code) or the client goes.
func (s *Server) relayTerminal(parent context.Context, conn *websocket.Conn, proc TerminalProcess) (int, bool) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	output := make(chan struct{})
	go func() {
		defer close(output)
		buf := make([]byte, 32<<10)
		for {
			n, err := proc.Read(buf)
			if n > 0 {
				wctx, done := context.WithTimeout(ctx, terminalWriteWait)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				done()
				if werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer cancel()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				if _, err = proc.Write(data); err != nil {
					return
				}
				continue
			}
			var m TerminalControl
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols >= 1 && m.Rows >= 1 && m.Cols <= terminalMaxSize && m.Rows <= terminalMaxSize {
				_ = proc.Resize(m.Cols, m.Rows)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(terminalPing)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pctx, done := context.WithTimeout(ctx, terminalPingWait)
				err := conn.Ping(pctx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	exited := make(chan int, 1)
	go func() { exited <- proc.Wait() }()

	select {
	case <-ctx.Done():
		return 0, false
	case code := <-exited:
		// What the process wrote last still reaches the operator.
		select {
		case <-output:
		case <-time.After(terminalDrain):
		case <-ctx.Done():
			return code, true
		}
		b, _ := json.Marshal(TerminalControl{Type: "exit", Code: code})
		wctx, done := context.WithTimeout(ctx, terminalWriteWait)
		defer done()
		_ = conn.Write(wctx, websocket.MessageText, b)
		return code, true
	}
}
