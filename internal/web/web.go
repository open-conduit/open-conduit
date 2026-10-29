// Package web is the operator's browser view over the unified run history,
// the web counterpart of the TUI. Like the TUI it uses only the public
// management client: it lists Work and turns, shows a run with its Work's
// context (annotations, Waits, hold) and its Work's turns as a timeline,
// tails logs and transcripts, and offers the operator controls and the
// actions extensions offer (claim, answer, …) by name, with no executor,
// work-kind or domain branches. A transcript in the Messages API's shape
// reads as a chat whichever executor wrote it (see conversation.go).
//
// It runs as its own process beside the daemon, so the daemon keeps serving
// management only on its Unix socket. The process holds the operator token
// and never hands it to the browser. It listens only on loopback, answers
// only to the names it was told it is served on (so a rebound DNS name
// cannot reach it), lets a browser in only through the login URL it prints
// at start, and requires a per-process CSRF token on every change.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// DefaultAddr and DefaultHost are where the view is served unless told
// otherwise. Browsers resolve every *.localhost name to loopback without
// any setup (RFC 6761).
const (
	DefaultAddr = "127.0.0.1:7420"
	DefaultHost = "open-conduit.localhost"
)

// Source is the subset of the management client the view needs.
type Source interface {
	Runs(context.Context, url.Values) (client.Page[client.Run], error)
	Run(context.Context, string) (client.Run, error)
	ArtifactContent(ctx context.Context, id, byteRange string, w io.Writer) error
	RunLive(ctx context.Context, id string, tail int64) (client.LiveRun, error)
	Work(ctx context.Context, id string) (client.Work, error)
	Waits(ctx context.Context, q url.Values) (client.Page[client.Wait], error)
	Scopes(ctx context.Context, q url.Values) (client.Page[client.Scope], error)
	Config(ctx context.Context) (client.Config, error)
	CancelExecution(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	CancelWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	RetryWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	HoldWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	ReleaseWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	Actions(ctx context.Context, t client.ActionTarget) ([]client.Action, error)
	Panels(ctx context.Context, t client.ActionTarget) ([]client.Panel, error)
	InvokeAction(ctx context.Context, t client.ActionTarget, provider, id, input, key string) (client.ActionResult, error)
	Health(ctx context.Context) (client.Health, error)
	Capacity(ctx context.Context) (client.Capacity, error)
	Works(ctx context.Context, q url.Values) (client.Page[client.Work], error)
	WorksWithActions(ctx context.Context, q url.Values) (client.Page[client.Work], error)
	Events(ctx context.Context, q url.Values) (client.Page[client.Event], error)
	EventsWithRoutes(ctx context.Context, q url.Values) (client.Page[client.Event], error)
	EventRoute(ctx context.Context, id string) (client.EventRoute, error)
	TraceRoute(ctx context.Context, scope string, e client.TraceEvent) (client.RouteTrace, error)
	RouteSamples(ctx context.Context, scope string) ([]client.RouteSample, error)
	RebindWork(ctx context.Context, id, revision, binding, reason, key string) (client.ControlResult, error)
	AttachWork(ctx context.Context, id, key string) (client.AttachSpec, error)
	ReconcileScope(ctx context.Context, id, revision, key string) (client.ReconcileResult, error)
	Wake(ctx context.Context, id string, r client.WakeRequest, key string) (client.WakeResult, error)
	Doctor(ctx context.Context, checkSecrets bool) (client.Doctor, error)
}

var _ Source = (*client.Client)(nil)

// Options configure a Server.
type Options struct {
	// Addr is the loopback host:port to listen on (DefaultAddr when empty).
	Addr string
	// Host is the name the view is served on (DefaultHost when empty).
	// Requests naming any other host than it, localhost or the listening
	// IP are refused.
	Host string
	// NewKey returns a fresh Idempotency-Key for each change.
	NewKey func() string
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

const (
	sessionCookie = "oc_session"
	flashCookie   = "oc_flash"
	callTimeout   = 15 * time.Second
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server serves the view. Its login token, session and CSRF secrets live
// only as long as the process.
type Server struct {
	src    Source
	opts   Options
	login  string
	sess   string
	csrf   string
	pages  map[string]*template.Template
	static http.Handler

	mu     sync.Mutex
	labels map[string]string

	// wmu guards the short-lived read of every Work (cachedWorld).
	wmu    sync.Mutex
	wcache world
	wat    time.Time

	// receipts are this process's interventions, newest first (actions.go).
	receipts receiptLog
}

// New makes a Server over src.
func New(src Source, opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	if opts.Host == "" {
		opts.Host = DefaultHost
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewKey == nil {
		opts.NewKey = func() string { return "web-" + secret() }
	}
	if err := checkLoopback(opts.Addr); err != nil {
		return nil, err
	}
	if strings.ContainsAny(opts.Host, ":/ ") {
		return nil, fmt.Errorf("host %q must be a bare name, without a port or scheme", opts.Host)
	}
	s := &Server{src: src, opts: opts, login: secret(), sess: secret(), csrf: secret(), labels: map[string]string{}}
	if err := s.parse(); err != nil {
		return nil, err
	}
	sub, _ := fs.Sub(assets, "static")
	s.static = http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return s, nil
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// checkLoopback refuses an address that is not on this machine only.
func checkLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("address %q must be host:port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("address %q is not loopback: the web view serves this machine only", addr)
}

// LoginURLs are the URLs that sign a browser in: by name, then by address
// for a browser that does not resolve *.localhost.
func (s *Server) LoginURLs(addr string) []string {
	_, port, _ := net.SplitHostPort(addr)
	q := "/login?token=" + s.login
	return []string{"http://" + net.JoinHostPort(s.opts.Host, port) + q, "http://" + addr + q}
}

// Run listens on the configured address and serves until ctx ends. It
// writes the login URLs to out once it is listening.
func (s *Server) Run(ctx context.Context, out io.Writer) error {
	l, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, l, out)
}

// Serve serves on l until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener, out io.Writer) error {
	addr := l.Addr().String()
	if err := checkLoopback(addr); err != nil {
		l.Close()
		return err
	}
	urls := s.LoginURLs(addr)
	fmt.Fprintf(out, "Open Conduit web view on http://%s\nOpen this to sign in:\n  %s\nor, if your browser does not resolve %s:\n  %s\n", net.JoinHostPort(s.opts.Host, portOf(addr)), urls[0], s.opts.Host, urls[1])
	srv := &http.Server{Handler: s.Handler(addr), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// Handler is the view's HTTP handler for a listener on addr.
func (s *Server) Handler(addr string) http.Handler {
	port := portOf(addr)
	allowed := map[string]bool{
		net.JoinHostPort(s.opts.Host, port): true,
		net.JoinHostPort("localhost", port): true,
		addr:                                true,
	}
	if port == "80" {
		// Browsers leave the default port out of Host.
		allowed[s.opts.Host], allowed["localhost"] = true, true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /{$}", s.authed(s.runsPage))
	// Work and run IDs contain slashes, so IDs travel in the query and form.
	mux.HandleFunc("GET /work", s.authed(s.workPage))
	mux.HandleFunc("GET /run", s.authed(s.runRedirect))
	mux.HandleFunc("GET /run/conversation", s.authed(s.conversationPart))
	mux.HandleFunc("GET /output", s.authed(s.outputPage))
	mux.HandleFunc("GET /attention", s.authed(s.attentionPage))
	mux.HandleFunc("GET /config", s.authed(s.configPage))
	mux.HandleFunc("GET /scopes", s.authed(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/config", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("GET /setup", s.authed(s.setupPage))
	mux.HandleFunc("GET /palette.json", s.authed(s.palette))
	mux.HandleFunc("GET /work/confirm", s.authed(s.confirmPage))
	mux.HandleFunc("POST /work/bulk/{control}", s.authed(s.changed(s.bulkControl)))
	mux.HandleFunc("POST /work/{control}", s.authed(s.changed(s.workControl)))
	mux.HandleFunc("POST /run/cancel", s.authed(s.changed(s.cancelRun)))
	mux.HandleFunc("POST /actions", s.authed(s.changed(s.invokeAction)))
	mux.HandleFunc("POST /scopes/reconcile", s.authed(s.changed(s.reconcileScope)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: under no-referrer a browser sends
		// "Origin: null" with our own form posts, and changed refuses them.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if !allowed[strings.ToLower(r.Host)] {
			// A name that is not ours: a rebound DNS name or a typo.
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// loginPage trades the printed token for a session cookie on this name.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !same(r.URL.Query().Get("token"), s.login) {
		http.Error(w, "sign in with the URL open-conduit web printed", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sess, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !same(c.Value, s.sess) {
			http.Error(w, "not signed in: open the login URL open-conduit web printed", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// changed guards a change: the form's CSRF token, and an Origin, when the
// browser sends one, naming the host the request came to.
func (s *Server) changed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		if !same(r.PostFormValue("csrf"), s.csrf) {
			http.Error(w, "stale or missing form token: reload the page", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// flashNote is a change's outcome shown on the next page as a toast: what
// happened, the call made and the event the daemon accepted.
type flashNote struct {
	Text  string `json:"t"`
	Call  string `json:"c,omitempty"`
	Event string `json:"e,omitempty"`
	Err   bool   `json:"x,omitempty"`
}

// flash shows text on the next page this browser loads.
func flash(w http.ResponseWriter, text string, failed bool) {
	flashWith(w, flashNote{Text: text, Err: failed})
}

func flashWith(w http.ResponseWriter, n flashNote) {
	b, _ := json.Marshal(n)
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(string(b)), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 60})
}

// takeFlash reads and clears the pending flash.
func takeFlash(w http.ResponseWriter, r *http.Request) (*flashNote, bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil, false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	v, _ := url.QueryUnescape(c.Value)
	var n flashNote
	if json.Unmarshal([]byte(v), &n) != nil || n.Text == "" {
		return nil, false
	}
	return &n, n.Err
}
