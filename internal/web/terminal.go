package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/open-conduit/open-conduit/client"
)

// The terminal page relays a browser's WebSocket to the daemon's terminal
// (client.OpenTerminal). The view adds no authority of its own: the daemon
// checks the role, holds the Work and records the session, as whoever this
// request acts for.

const (
	terminalProtocol = "open-conduit.terminal"
	terminalCSRF     = "csrf."
	terminalPing     = 20 * time.Second
	terminalPingWait = 15 * time.Second
	terminalWrite    = 30 * time.Second
	terminalOpen     = 2 * time.Minute
	terminalMaxInput = 64 << 10
	terminalMaxSize  = 1000
)

type terminalData struct{ ID, Name string }

func (s *Server) terminalPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id := r.URL.Query().Get("id")
	work, err := s.src.Work(ctx, id)
	if err != nil {
		http.Error(w, "no such Work: "+errText(err), http.StatusNotFound)
		return
	}
	name := workName(newWork(work, nil, nil, s.opts.Now()))
	if name == "" {
		name = work.ID
	}
	// xterm.js sizes its grid with a style element it writes itself, so
	// this page alone allows inline styles. Scripts stay same-origin.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	s.render(w, r, "terminal", &page{Title: "Terminal on " + name, Nav: "runs"}, terminalData{ID: work.ID, Name: name})
}

// terminalAllowed is the cross-site check a WebSocket needs, since a browser
// lets any page open one with this site's cookies: an Origin naming this
// host, always sent by browsers on a handshake, and the session's form
// token, offered as a subprotocol so it stays out of URLs and logs.
func (s *Server) terminalAllowed(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if sessionNonce(r.Context()) == "" {
		return false
	}
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(line, ",") {
			if token, ok := strings.CutPrefix(strings.TrimSpace(p), terminalCSRF); ok && same(token, s.formToken(r)) {
				return true
			}
		}
	}
	return false
}

func terminalSize(raw string) uint16 {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > terminalMaxSize {
		return 0
	}
	return uint16(n)
}

func (s *Server) terminalStream(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAllowed(r) {
		http.Error(w, "cross-site or stale terminal request refused: reload the page", http.StatusForbidden)
		return
	}
	browser, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
	if err != nil {
		return
	}
	defer browser.CloseNow()
	browser.SetReadLimit(terminalMaxInput)
	q := r.URL.Query()
	opts := client.TerminalOptions{Cols: terminalSize(q.Get("cols")), Rows: terminalSize(q.Get("rows")), Term: "xterm-256color"}
	// The upgrade is answered first, because a browser can't read the body
	// of a refused handshake and the operator needs to see why.
	opening, cancel := context.WithTimeout(r.Context(), terminalOpen)
	term, err := s.src.OpenTerminal(opening, q.Get("id"), opts)
	cancel()
	if err != nil {
		terminalControl(r.Context(), browser, map[string]any{"type": "error", "message": terminalRefusal(err)})
		_ = browser.Close(websocket.StatusNormalClosure, "")
		return
	}
	// However this ends, the daemon hears it and releases the hold.
	defer term.Detach()
	if relayTerminal(r.Context(), browser, term) {
		_ = browser.Close(websocket.StatusNormalClosure, "")
	}
}

func terminalRefusal(err error) string {
	var api *client.Error
	if errors.As(err, &api) && api.Message != "" {
		return "The terminal was refused: " + api.Message
	}
	return "The terminal could not be opened: " + errText(err)
}

func terminalControl(ctx context.Context, c *websocket.Conn, m map[string]any) {
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, terminalWrite)
	defer cancel()
	_ = c.Write(wctx, websocket.MessageText, b)
}

// relayTerminal copies between the browser and the daemon until either
// goes. It reports whether the session ended by itself.
func relayTerminal(parent context.Context, browser *websocket.Conn, term *client.Terminal) bool {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ended := make(chan bool, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				wctx, done := context.WithTimeout(ctx, terminalWrite)
				werr := browser.Write(wctx, websocket.MessageBinary, buf[:n])
				done()
				if werr != nil {
					ended <- false
					return
				}
			}
			if errors.Is(err, io.EOF) {
				if code, ok := term.ExitCode(); ok {
					terminalControl(ctx, browser, map[string]any{"type": "exit", "code": code})
				}
				ended <- true
				return
			}
			if err != nil {
				terminalControl(ctx, browser, map[string]any{"type": "error", "message": "The connection to the daemon was lost."})
				ended <- true
				return
			}
		}
	}()
	go func() {
		defer cancel()
		for {
			kind, data, err := browser.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				if _, err = term.Write(data); err != nil {
					return
				}
				continue
			}
			var m struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols >= 1 && m.Rows >= 1 && m.Cols <= terminalMaxSize && m.Rows <= terminalMaxSize {
				_ = term.Resize(uint16(m.Cols), uint16(m.Rows))
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
				err := browser.Ping(pctx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	select {
	case by := <-ended:
		return by
	case <-ctx.Done():
		return false
	}
}
