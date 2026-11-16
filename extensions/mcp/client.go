package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the MCP revision the bridge asks for. A server that
// answers with another revision it supports is spoken to in that one: the
// bridge uses only initialize, tools/list and tools/call, which every
// revision since 2024-11-05 carries the same way.
const ProtocolVersion = "2025-06-18"

// maxMessage bounds one message from a server.
const maxMessage = 16 << 20

// ServerTool is a tool as a server lists it.
type ServerTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// CallResult is a tools/call result.
type CallResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

// rpcError is a JSON-RPC error a server answered.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("server error %d: %s", e.Code, e.Message) }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// transport carries JSON-RPC messages to one server. roundTrip sends a
// request and returns its response; notify sends a notification.
type transport interface {
	roundTrip(ctx context.Context, id int64, req []byte) (message, error)
	notify(ctx context.Context, msg []byte) error
	close() error
}

// session is an initialized connection to a server.
type session struct {
	t      transport
	nextID atomic.Int64
}

func request(id int64, method string, params any) []byte {
	b, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", id, method, params})
	return b
}

func (s *session) call(ctx context.Context, method string, params, out any) error {
	id := s.nextID.Add(1)
	resp, err := s.t.roundTrip(ctx, id, request(id, method, params))
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("the server's %s result is malformed", method)
	}
	return nil
}

// start initializes a session over t.
func start(ctx context.Context, t transport) (*session, error) {
	s := &session{t: t}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "open-conduit-mcp-bridge", "version": "1"}}
	if err := s.call(ctx, "initialize", params, &init); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if init.ProtocolVersion == "" {
		return nil, errors.New("initialize: the server named no protocol version")
	}
	if h, ok := t.(*httpTransport); ok {
		h.version.Store(init.ProtocolVersion)
	}
	if err := t.notify(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return nil, fmt.Errorf("initialized: %w", err)
	}
	return s, nil
}

// maxPages bounds a tools/list walk.
const maxPages = 50

// tools lists every tool the server offers now.
func (s *session) tools(ctx context.Context) ([]ServerTool, error) {
	var all []ServerTool
	cursor := ""
	for range maxPages {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var page struct {
			Tools      []ServerTool `json:"tools"`
			NextCursor string       `json:"nextCursor,omitempty"`
		}
		if err := s.call(ctx, "tools/list", params, &page); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
	return nil, errors.New("tools/list: the server's list doesn't end")
}

func (s *session) callTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage(`{}`)
	}
	var out CallResult
	err := s.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out)
	return out, err
}

func (s *session) close() error { return s.t.close() }

// stdioTransport speaks newline-delimited JSON-RPC over a process's stdin
// and stdout. A reader goroutine hands each response to the request
// waiting for its ID and answers the server's own requests: ping, and
// method-not-found for the rest (the bridge offers no client capabilities).
type stdioTransport struct {
	w       io.WriteCloser
	wmu     sync.Mutex
	mu      sync.Mutex
	waiting map[int64]chan message
	done    chan struct{}
	err     error
	stop    func() error
}

func newStdioTransport(stdin io.WriteCloser, stdout io.Reader, stop func() error) *stdioTransport {
	t := &stdioTransport{w: stdin, waiting: map[int64]chan message{}, done: make(chan struct{}), stop: stop}
	go t.read(stdout)
	return t
}

func (t *stdioTransport) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			t.answer(m)
		case m.Method == "" && len(m.ID) > 0:
			id, err := strconv.ParseInt(string(m.ID), 10, 64)
			if err != nil {
				continue
			}
			t.mu.Lock()
			ch := t.waiting[id]
			delete(t.waiting, id)
			t.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	t.mu.Lock()
	t.err = errors.New("the server closed its output")
	if err := sc.Err(); err != nil {
		t.err = fmt.Errorf("reading the server's output: %w", err)
	}
	t.mu.Unlock()
	close(t.done)
}

// answer replies to a request the server sent.
func (t *stdioTransport) answer(m message) {
	var reply []byte
	if m.Method == "ping" {
		reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
	} else {
		reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "the client offers no " + m.Method}})
	}
	_ = t.write(reply)
}

func (t *stdioTransport) write(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err := t.w.Write(append(b, '\n'))
	return err
}

func (t *stdioTransport) roundTrip(ctx context.Context, id int64, req []byte) (message, error) {
	ch := make(chan message, 1)
	t.mu.Lock()
	if t.err != nil {
		err := t.err
		t.mu.Unlock()
		return message{}, err
	}
	t.waiting[id] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.waiting, id)
		t.mu.Unlock()
	}()
	if err := t.write(req); err != nil {
		return message{}, fmt.Errorf("writing to the server: %w", err)
	}
	select {
	case m := <-ch:
		return m, nil
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return message{}, t.err
	case <-ctx.Done():
		cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
		_ = t.write(cancel)
		return message{}, ctx.Err()
	}
}

func (t *stdioTransport) notify(_ context.Context, msg []byte) error { return t.write(msg) }

func (t *stdioTransport) close() error {
	_ = t.w.Close()
	return t.stop()
}

// httpTransport speaks MCP's streamable HTTP transport: each message is a
// POST, answered with JSON or with an event stream that carries the
// response; the session ID the server assigns at initialize goes with
// every later request.
type httpTransport struct {
	url     string
	headers http.Header
	client  *http.Client
	session atomic.Value // string
	version atomic.Value // string
}

func (t *httpTransport) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range t.headers {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s, _ := t.session.Load().(string); s != "" {
		req.Header.Set("Mcp-Session-Id", s)
	}
	if v, _ := t.version.Load().(string); v != "" {
		req.Header.Set("MCP-Protocol-Version", v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		// The URL and headers stay out of the error: they may carry what
		// the operator would keep private.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("the server couldn't be reached")
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.session.Store(s)
	}
	return resp, nil
}

func (t *httpTransport) roundTrip(ctx context.Context, id int64, req []byte) (message, error) {
	resp, err := t.post(ctx, req)
	if err != nil {
		return message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMessage))
		return message{}, fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body := io.LimitReader(resp.Body, maxMessage)
	want := strconv.FormatInt(id, 10)
	if media == "text/event-stream" {
		return readEvents(body, want)
	}
	var m message
	if err := json.NewDecoder(body).Decode(&m); err != nil || string(m.ID) != want {
		return message{}, errors.New("the server's response is malformed")
	}
	return m, nil
}

// readEvents reads an event stream until the response with ID want.
func readEvents(r io.Reader, want string) (message, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var data strings.Builder
	flush := func() (message, bool) {
		defer data.Reset()
		var m message
		if data.Len() == 0 || json.Unmarshal([]byte(data.String()), &m) != nil {
			return m, false
		}
		return m, m.Method == "" && string(m.ID) == want
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if m, ok := flush(); ok {
				return m, nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if m, ok := flush(); ok {
		return m, nil
	}
	return message{}, errors.New("the server's event stream ended without a response")
}

func (t *httpTransport) notify(ctx context.Context, msg []byte) error {
	resp, err := t.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMessage))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// close ends the server's session, when it assigned one.
func (t *httpTransport) close() error {
	s, _ := t.session.Load().(string)
	if s == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return err
	}
	for k, v := range t.headers {
		req.Header[k] = v
	}
	req.Header.Set("Mcp-Session-Id", s)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil
	}
	return resp.Body.Close()
}
