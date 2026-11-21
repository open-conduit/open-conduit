package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// TerminalOptions are a terminal's initial size and type, and whether the
// Work stays held after it ends.
type TerminalOptions struct {
	Cols, Rows uint16
	Term       string
	KeepHeld   bool
}

// Terminal is an open terminal on a Work's session. Read returns the
// session's output and io.EOF when it has ended; ExitCode is then its code.
type Terminal struct {
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	pending []byte
	code    int
	exited  bool
}

// Remote reports whether the client reaches the daemon over the network.
func (c *Client) Remote() bool { return strings.HasPrefix(c.base, "https://") }

// OpenTerminal opens a terminal on held-or-holdable Work's session, streamed
// through the daemon. It holds the Work until the terminal ends.
func (c *Client) OpenTerminal(ctx context.Context, id string, o TerminalOptions) (*Terminal, error) {
	q := url.Values{}
	if o.Cols > 0 && o.Rows > 0 {
		q.Set("cols", strconv.Itoa(int(o.Cols)))
		q.Set("rows", strconv.Itoa(int(o.Rows)))
	}
	if o.Term != "" {
		q.Set("term", o.Term)
	}
	if o.KeepHeld {
		q.Set("keep_held", "true")
	}
	u := strings.Replace(c.base, "http", "ws", 1) + "work/" + esc(id) + "/terminal"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	header := http.Header{}
	probe, err := http.NewRequest(http.MethodGet, c.base, nil)
	if err != nil {
		return nil, err
	}
	authorize(probe, c.token)
	header.Set("Authorization", probe.Header.Get("Authorization"))
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.streaming(), HTTPHeader: header})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, terminalRefusal(resp)
		}
		return nil, transportError(ctx, err)
	}
	conn.SetReadLimit(1 << 20)
	life, cancel := context.WithCancel(context.Background())
	return &Terminal{conn: conn, ctx: life, cancel: cancel}, nil
}

func terminalRefusal(resp *http.Response) error {
	var env struct {
		Error Error `json:"error"`
	}
	var data []byte
	if resp.Body != nil {
		data, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	if json.Unmarshal(data, &env) != nil || env.Error.Code == "" {
		return &Error{Status: resp.StatusCode, Code: "http_error", Message: http.StatusText(resp.StatusCode)}
	}
	env.Error.Status = resp.StatusCode
	return &env.Error
}

// streaming is an HTTP client for an upgrade: HTTP/1.1 only, since a
// WebSocket can't start on an HTTP/2 connection, and on a connection of its
// own.
func (c *Client) streaming() *http.Client {
	t, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return c.http
	}
	s := t.Clone()
	s.ForceAttemptHTTP2 = false
	s.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if s.TLSClientConfig != nil {
		s.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{Transport: s, CheckRedirect: c.http.CheckRedirect}
}

// Read returns the session's output.
func (t *Terminal) Read(p []byte) (int, error) {
	for {
		t.mu.Lock()
		if len(t.pending) > 0 {
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			t.mu.Unlock()
			return n, nil
		}
		t.mu.Unlock()
		kind, data, err := t.conn.Read(t.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, context.Canceled) {
				return 0, io.EOF
			}
			return 0, err
		}
		if kind == websocket.MessageBinary {
			t.mu.Lock()
			t.pending = data
			t.mu.Unlock()
			continue
		}
		var m struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		}
		if json.Unmarshal(data, &m) == nil && m.Type == "exit" {
			t.mu.Lock()
			t.code, t.exited = m.Code, true
			t.mu.Unlock()
		}
	}
}

// Write sends input to the session.
func (t *Terminal) Write(p []byte) (int, error) {
	if err := t.conn.Write(t.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize tells the session the terminal's new size.
func (t *Terminal) Resize(cols, rows uint16) error {
	b, _ := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	return t.conn.Write(t.ctx, websocket.MessageText, b)
}

// ExitCode is the session's exit code, once it has ended by itself.
func (t *Terminal) ExitCode() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.code, t.exited
}

// Abort drops the connection with no closing handshake, as a client that
// dies does.
func (t *Terminal) Abort() error {
	defer t.cancel()
	return t.conn.CloseNow()
}

// Detach leaves the session: the daemon ends it and releases the hold.
func (t *Terminal) Detach() error {
	defer t.cancel()
	return t.conn.Close(websocket.StatusNormalClosure, "")
}
