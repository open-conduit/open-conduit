package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/web"
)

// webTerminal is a signed-in browser on a web view of h's daemon.
type webTerminal struct {
	base, csrf string
	http       *http.Client
}

func newWebTerminal(t *testing.T, h *managementHarness, id string) webTerminal {
	t.Helper()
	srv, err := web.New(h.operatorClient(), web.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l, io.Discard) }()
	t.Cleanup(func() { cancel(); <-done })
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	login := srv.LoginURLs(l.Addr().String())[1]
	resp, err := browser.Get(login)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	base := "http://" + l.Addr().String()
	resp, err = browser.Get(base + "/work/terminal?" + url.Values{"id": {id}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	m := regexp.MustCompile(`data-csrf="([0-9a-f]+)"`).FindSubmatch(page)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("terminal page: %d\n%s", resp.StatusCode, page)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "script-src") {
		t.Fatalf("the page's policy: %s", csp)
	}
	return webTerminal{base: base, csrf: string(m[1]), http: browser}
}

func (b webTerminal) dial(id, origin string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	u := "ws" + strings.TrimPrefix(b.base, "http") + "/work/terminal/stream?" + url.Values{"id": {id}, "cols": {"100"}, "rows": {"30"}}.Encode()
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPClient: b.http, HTTPHeader: header, Subprotocols: protocols})
}

// The web view's terminal reaches the session through the daemon, refuses
// a page on another origin or without the session's token, and releases the
// hold when the browser goes.
func TestWebTerminal(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "web-terminal")
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"sh", "-c", `stty -echo; printf 'ready\n'; read line; printf 'got:%s\n' "$line"; read again`}}, nil
	}
	b := newWebTerminal(t, h, id)
	c := h.operatorClient()
	good := []string{"open-conduit.terminal", "csrf." + b.csrf}
	for name, try := range map[string]struct {
		origin string
		protos []string
	}{
		"another origin":     {"http://evil.example", good},
		"another local port": {"http://127.0.0.1:1", good},
		"no origin":          {"", good},
		"no token":           {b.base, []string{"open-conduit.terminal"}},
		"another token":      {b.base, []string{"open-conduit.terminal", "csrf." + strings.Repeat("0", 64)}},
	} {
		if conn, resp, err := b.dial(id, try.origin, try.protos...); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			if conn != nil {
				conn.CloseNow()
			}
			t.Fatalf("%s: %v %+v", name, err, resp)
		}
	}
	if w, err := c.Work(context.Background(), id); err != nil || w.Hold != nil {
		t.Fatalf("a refused handshake acted: %+v %v", w.Hold, err)
	}

	conn, _, err := b.dial(id, b.base, good...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read := func(want string) {
		t.Helper()
		var got strings.Builder
		for !strings.Contains(got.String(), want) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			kind, data, err := conn.Read(ctx)
			cancel()
			if err != nil {
				t.Fatalf("reading for %q after %q: %v", want, got.String(), err)
			}
			if kind == websocket.MessageBinary {
				got.Write(data)
			}
		}
	}
	read("ready")
	heldUntil(t, c, id, true)
	if err = conn.Write(context.Background(), websocket.MessageBinary, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	read("got:hello")
	conn.CloseNow()
	heldUntil(t, c, id, false)
}

// A refusal reaches the page as a message it can show.
func TestWebTerminalShowsARefusal(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "web-terminal-refused")
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{}, management.ErrNothingToAttach
	}
	b := newWebTerminal(t, h, id)
	conn, _, err := b.dial(id, b.base, "open-conduit.terminal", "csrf."+b.csrf)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("%v %v", kind, err)
	}
	var m struct{ Type, Message string }
	if err = json.Unmarshal(data, &m); err != nil || m.Type != "error" || !strings.Contains(m.Message, "nothing to attach") {
		t.Fatalf("%s %v", data, err)
	}
	heldUntil(t, h.operatorClient(), id, false)
}

// A client acting for someone opens the terminal as them: the daemon
// checks their role and records them, never the client's own credential.
func TestTerminalActsForTheSubject(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "terminal-subject")
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"sh", "-c", `printf 'ready\n'`}}, nil
	}
	path := filepath.Join(h.state, "ui.token")
	h.mustCLI(nil, "auth", "add", "ui", "--role", "operate", "--act-for", "operator", "--out", path)
	h.mustCLI(nil, "auth", "add", "olly", "--role", "operate", "--out", filepath.Join(h.state, "olly.token"))
	h.mustCLI(nil, "auth", "add", "vera", "--role", "view", "--out", filepath.Join(h.state, "vera.token"))
	f, err := client.ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ui, err := client.New(h.endpoint, f.Token)
	if err != nil {
		t.Fatal(err)
	}
	var api *client.Error
	if _, err = ui.OpenTerminal(client.WithSubject(context.Background(), "operator:vera"), id, client.TerminalOptions{}); !errors.As(err, &api) || api.Status != 403 {
		t.Fatalf("a viewer through an operating client: %v", err)
	}
	term, err := ui.OpenTerminal(client.WithSubject(context.Background(), "operator:olly"), id, client.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Detach()
	readUntil(t, bufio.NewReader(term), "ready")
	c := h.operatorClient()
	heldUntil(t, c, id, false)
	events, err := c.Events(context.Background(), url.Values{"work_id": {id}, "type": {"core.manual.terminal_opened"}})
	if err != nil || len(events.Items) != 1 || events.Items[0].Actor != "operator:olly" || events.Items[0].Via != "operator:ui" {
		t.Fatalf("the terminal on record: %+v %v", events.Items, err)
	}
}
