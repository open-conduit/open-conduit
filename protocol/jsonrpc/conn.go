// Package jsonrpc is the protocol version 1 transport (07-02): JSON-RPC 2.0
// over a pair of byte streams, one JSON object per line, standard library
// only. The connection is symmetric: either side sends requests, which is
// how an extension calls the host (KV, artifacts, delivery) while serving a
// host request.
//
// Hardened from the 07-01 prototype:
//   - the reader never blocks: over MaxInflight concurrently served requests
//     a request is refused with "overloaded", because the reader also carries
//     the responses nested callbacks wait for;
//   - a line longer than protocol.MaxMessageBytes, invalid JSON or a
//     truncated final line (a partial response before a crash) is a protocol
//     error that closes the connection, failing every pending call with
//     protocol.ErrTransport, never a hang or an authored error;
//   - authored errors cross as -32000 with data.code and decode to the
//     public sentinels (protocol.Decode); unauthored errors cross as
//     "internal" with no detail;
//   - "$/cancel" cancels the peer's handler context; the canceller returns
//     context.Canceled at once and discards a late response.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/protocol"
)

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeCancelled      = -32800 // LSP RequestCancelled
	codeApplication    = -32000
	// CancelMethod is the cancellation notification either side may send.
	CancelMethod = "$/cancel"
)

// Handler serves one inbound request or notification.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type wireError struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Data    *errData `json:"data,omitempty"`
}

type errData struct {
	Code    string     `json:"code"`
	ResetAt *time.Time `json:"reset_at,omitempty"`
}

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *wireError       `json:"error,omitempty"`
}

// Options bound a connection. Zero values take the protocol limits.
type Options struct {
	MaxMessage  int
	MaxInflight int
	// OnProtocolError, when set, receives the reason a malformed stream
	// closed the connection (for the side's own diagnostic log).
	OnProtocolError func(error)
}

// Conn is one JSON-RPC peer.
type Conn struct {
	handler Handler
	opts    Options
	wmu     sync.Mutex
	w       *bufio.Writer
	closer  io.Closer

	mu      sync.Mutex
	next    int64
	pending map[string]chan message
	cancels map[string]context.CancelFunc
	err     error
	done    chan struct{}
	slots   chan struct{}
}

// NewConn starts reading r. A read failure, a protocol error or Close ends
// the connection. If w is also an io.Closer, the end closes it, which tells
// a peer process reading it to exit.
func NewConn(r io.Reader, w io.Writer, h Handler, o Options) *Conn {
	if o.MaxMessage == 0 {
		o.MaxMessage = protocol.MaxMessageBytes
	}
	if o.MaxInflight == 0 {
		o.MaxInflight = protocol.MaxInflight
	}
	c := &Conn{handler: h, opts: o, w: bufio.NewWriter(w), pending: map[string]chan message{}, cancels: map[string]context.CancelFunc{}, done: make(chan struct{}), slots: make(chan struct{}, o.MaxInflight)}
	if cl, ok := w.(io.Closer); ok {
		c.closer = cl
	}
	go c.read(r)
	return c
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (nil while open).
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection; pending calls fail with protocol.ErrTransport.
func (c *Conn) Close() error {
	c.shutdown(protocol.ErrTransport)
	return nil
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	for _, cancel := range c.cancels {
		cancel()
	}
	close(c.done)
	c.mu.Unlock()
	if c.closer != nil {
		_ = c.closer.Close()
	}
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) >= c.opts.MaxMessage {
		return protocol.Errorf(protocol.CodeInvalid, "message exceeds the protocol bound")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err = c.w.Write(append(data, '\n')); err == nil {
		err = c.w.Flush()
	}
	if err != nil {
		c.shutdown(protocol.ErrTransport)
		return protocol.ErrTransport
	}
	return nil
}

func (c *Conn) protocolError(err error) {
	if c.opts.OnProtocolError != nil {
		c.opts.OnProtocolError(err)
	}
	c.shutdown(protocol.ErrTransport)
}

func (c *Conn) read(r io.Reader) {
	s := bufio.NewScanner(r)
	// The bound is the larger of max and the initial buffer's capacity.
	s.Buffer(make([]byte, 0, min(64<<10, c.opts.MaxMessage)), c.opts.MaxMessage)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" {
			// Includes a truncated final line: a partial response before
			// the peer died is a protocol error, not a result.
			c.protocolError(errors.New("malformed message"))
			return
		}
		switch {
		case m.Method == CancelMethod:
			var p struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(m.Params, &p)
			c.mu.Lock()
			if cancel := c.cancels[string(p.ID)]; cancel != nil {
				cancel()
			}
			c.mu.Unlock()
		case m.Method != "" && m.ID != nil:
			// Never block the reader: it also delivers the responses that
			// in-flight handlers wait for (nested callbacks), so blocking
			// here deadlocks. Over the bound, refuse explicitly instead.
			select {
			case c.slots <- struct{}{}:
				go c.serve(m)
			default:
				_ = c.write(message{ID: m.ID, Error: &wireError{Code: codeApplication, Message: "too many requests in flight", Data: &errData{Code: protocol.CodeOverloaded}}})
			}
		case m.Method != "":
			// Other notifications are served without a reply.
			go func() { _, _ = c.handler(context.Background(), m.Method, m.Params) }()
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[string(*m.ID)]
			delete(c.pending, string(*m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		default:
			c.protocolError(errors.New("message is neither request nor response"))
			return
		}
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, bufio.ErrTooLong) {
			c.protocolError(errors.New("message exceeds the protocol bound"))
			return
		}
	}
	c.shutdown(protocol.ErrTransport)
}

func (c *Conn) serve(m message) {
	defer func() { <-c.slots }()
	ctx, cancel := context.WithCancel(context.Background())
	key := string(*m.ID)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		cancel()
		return
	}
	c.cancels[key] = cancel
	c.mu.Unlock()
	result, err := c.handler(ctx, m.Method, m.Params)
	c.mu.Lock()
	delete(c.cancels, key)
	c.mu.Unlock()
	cancelled := ctx.Err() != nil
	cancel()
	reply := message{ID: m.ID}
	if err == nil {
		if reply.Result, err = json.Marshal(result); err != nil {
			reply.Error = &wireError{Code: codeInternal, Message: "unencodable result", Data: &errData{Code: protocol.CodeInternal}}
		}
	} else {
		reply.Error = encodeError(err, cancelled)
	}
	if werr := c.write(reply); werr != nil && reply.Error == nil {
		// An oversized result is refused rather than closing the stream.
		_ = c.write(message{ID: m.ID, Error: encodeError(werr, false)})
	}
}

func encodeError(err error, cancelled bool) *wireError {
	if cancelled || errors.Is(err, context.Canceled) {
		return &wireError{Code: codeCancelled, Message: "request cancelled", Data: &errData{Code: protocol.CodeCancelled}}
	}
	e := protocol.Encode(err)
	code := codeApplication
	switch e.Code {
	case protocol.CodeMethodNotFound:
		code = codeMethodNotFound
	case protocol.CodeInternal:
		code = codeInternal
	}
	return &wireError{Code: code, Message: e.Message, Data: &errData{Code: e.Code, ResetAt: e.ResetAt}}
}

// Call sends a request and decodes its result into out (which may be nil).
// It waits for the response, ctx or the end of the connection. Cancelling
// ctx sends $/cancel and returns ctx.Err() at once; a late response is
// discarded.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return protocol.Errorf(protocol.CodeInvalid, "unencodable params")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.next++
	id := json.RawMessage(strconv.FormatInt(c.next, 10))
	ch := make(chan message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(message{ID: &id, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return decodeError(m.Error)
		}
		if out != nil {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return protocol.Errorf(protocol.CodeInvalid, "malformed result")
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		params, _ := json.Marshal(map[string]json.RawMessage{"id": id})
		_ = c.write(message{Method: CancelMethod, Params: params})
		return ctx.Err()
	case <-c.done:
		return protocol.ErrTransport
	}
}

// Notify sends a notification (no response).
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: raw})
}

func decodeError(e *wireError) error {
	switch {
	case e.Code == codeCancelled:
		return context.Canceled
	case e.Data != nil && e.Data.Code != "":
		return protocol.Decode(e.Data.Code, e.Message, e.Data.ResetAt)
	case e.Code == codeMethodNotFound:
		return protocol.Errorf(protocol.CodeMethodNotFound, e.Message)
	case e.Code == codeInvalidParams || e.Code == codeInvalidRequest || e.Code == codeParse:
		return protocol.Errorf(protocol.CodeInvalid, e.Message)
	}
	return protocol.Errorf(protocol.CodeInternal, "internal error")
}

// Params decodes request params strictly enough for a handler: malformed
// params are an authored "invalid" refusal.
func Params(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return protocol.Errorf(protocol.CodeInvalid, "malformed params")
	}
	return nil
}
