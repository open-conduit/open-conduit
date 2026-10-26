// Package jsonrpc is the prototype JSON-RPC 2.0 transport over a pair of byte
// streams (the extension's stdin and stdout), using only the standard
// library. Framing is one JSON object per line. The connection is symmetric:
// either side may send requests, which is how an extension calls back into
// the host (KV, artifacts, push) while it serves a host request.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/alsiola/open-conduit/prototypes/transport/scenario"
)

const (
	// MaxMessage bounds one framed message; a larger line is a protocol
	// error that closes the connection rather than an unbounded buffer.
	MaxMessage = 4 << 20
	// MaxInflight bounds concurrently served inbound requests. Over it a
	// request is refused with the "overloaded" code (see read). Per-call
	// acknowledgement is the flow control for streams such as artifacts.
	MaxInflight = 64

	codeParse          = -32700
	codeMethodNotFound = -32601
	codeInternal       = -32603
	codeCancelled      = -32800 // LSP RequestCancelled
	codeApplication    = -32000
	cancelMethod       = "$/cancel"
)

// Handler serves one inbound request.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type wireError struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Data    *errData `json:"data,omitempty"`
}

// errData carries the stable string code of an application error.
type errData struct {
	Code string `json:"code"`
}

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *wireError       `json:"error,omitempty"`
}

// Conn is one JSON-RPC peer.
type Conn struct {
	handler Handler
	wmu     sync.Mutex
	w       *bufio.Writer

	mu      sync.Mutex
	next    int64
	pending map[string]chan message
	cancels map[string]context.CancelFunc
	err     error
	done    chan struct{}
	slots   chan struct{}
}

// NewConn starts reading r; Close or a read failure ends the connection.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	c := &Conn{handler: h, w: bufio.NewWriter(w), pending: map[string]chan message{}, cancels: map[string]context.CancelFunc{}, done: make(chan struct{}), slots: make(chan struct{}, MaxInflight)}
	go c.read(r)
	return c
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	for _, cancel := range c.cancels {
		cancel()
	}
	close(c.done)
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err = c.w.Write(append(data, '\n')); err == nil {
		err = c.w.Flush()
	}
	if err != nil {
		c.shutdown(scenario.ErrTransportClosed)
	}
	return err
}

func (c *Conn) read(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), MaxMessage)
	for s.Scan() {
		var m message
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			_ = c.write(message{Error: &wireError{Code: codeParse, Message: "parse error"}})
			continue
		}
		switch {
		case m.Method == cancelMethod:
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
				_ = c.write(message{ID: m.ID, Error: &wireError{Code: codeApplication, Message: "too many requests in flight", Data: &errData{Code: "overloaded"}}})
			}
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[string(*m.ID)]
			delete(c.pending, string(*m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	c.shutdown(scenario.ErrTransportClosed)
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
	var se *scenario.Error
	switch {
	case err == nil:
		reply.Result, err = json.Marshal(result)
		if err != nil {
			reply.Error = &wireError{Code: codeInternal, Message: "unencodable result"}
		}
	case cancelled:
		reply.Error = &wireError{Code: codeCancelled, Message: "request cancelled"}
	case errors.As(err, &se) && se.Code == "method_not_found":
		reply.Error = &wireError{Code: codeMethodNotFound, Message: se.Message, Data: &errData{Code: se.Code}}
	case errors.As(err, &se):
		reply.Error = &wireError{Code: codeApplication, Message: se.Message, Data: &errData{Code: se.Code}}
	default:
		// Unauthored errors never cross the boundary verbatim.
		reply.Error = &wireError{Code: codeInternal, Message: "internal error"}
	}
	_ = c.write(reply)
}

// Call sends a request and waits for its response, the context or the end of
// the connection. Cancelling ctx sends $/cancel and returns at once; a late
// response is discarded.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.next++
	id := json.RawMessage(strconv.FormatInt(c.next, 10))
	ch := make(chan message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(message{ID: &id, Method: method, Params: raw}); err != nil {
		return nil, scenario.ErrTransportClosed
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, decodeError(m.Error)
		}
		return m.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		params, _ := json.Marshal(map[string]json.RawMessage{"id": id})
		_ = c.write(message{Method: cancelMethod, Params: params})
		return nil, ctx.Err()
	case <-c.done:
		return nil, scenario.ErrTransportClosed
	}
}

func decodeError(e *wireError) error {
	switch {
	case e.Data != nil && e.Data.Code != "":
		return &scenario.Error{Code: e.Data.Code, Message: e.Message}
	case e.Code == codeCancelled:
		return context.Canceled
	}
	return &scenario.Error{Code: "internal", Message: e.Message}
}
