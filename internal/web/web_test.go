package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

type invoked struct {
	target           client.ActionTarget
	provider, id, in string
}

type fake struct {
	runs    []client.Run
	work    map[string]client.Work
	waits   []client.Wait
	actions map[string][]client.Action
	panels  map[string][]client.Panel
	scopes  []client.Scope
	config  *client.Config
	live    map[string]client.LiveRun
	content map[string]string

	events  []client.Event
	health  *client.Health
	trace   *client.RouteTrace
	samples map[string][]client.RouteSample

	healthErr error
	invoked   []invoked
	controls  []string
	calls     []string
	traced    []string
	withCalls int
}

func (f *fake) Runs(_ context.Context, q url.Values) (client.Page[client.Run], error) {
	var out []client.Run
	for _, r := range f.runs {
		if (q.Get("state") == "" || q.Get("state") == r.State) && (q.Get("work_id") == "" || q.Get("work_id") == r.WorkID) {
			out = append(out, r)
		}
	}
	return client.Page[client.Run]{Items: out}, nil
}
func (f *fake) Run(_ context.Context, id string) (client.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return client.Run{}, &client.Error{Status: 404, Code: "not_found", Message: "no run"}
}
func (f *fake) ArtifactContent(_ context.Context, id, _ string, w io.Writer) error {
	_, err := io.WriteString(w, f.content[id])
	return err
}
func (f *fake) RunLive(_ context.Context, id string, _ int64) (client.LiveRun, error) {
	if l, ok := f.live[id]; ok {
		return l, nil
	}
	return client.LiveRun{}, &client.Error{Status: 409, Code: "not_live"}
}
func (f *fake) Work(_ context.Context, id string) (client.Work, error) {
	if w, ok := f.work[id]; ok {
		return w, nil
	}
	return client.Work{}, &client.Error{Status: 404, Code: "not_found"}
}
func (f *fake) Waits(context.Context, url.Values) (client.Page[client.Wait], error) {
	return client.Page[client.Wait]{Items: f.waits}, nil
}
func (f *fake) Scopes(context.Context, url.Values) (client.Page[client.Scope], error) {
	return client.Page[client.Scope]{Items: f.scopes}, nil
}
func (f *fake) Config(context.Context) (client.Config, error) {
	if f.config == nil {
		return client.Config{}, &client.Error{Status: 404, Code: "not_found"}
	}
	return *f.config, nil
}
func (f *fake) control(name, id, revision string) (client.ControlResult, error) {
	f.controls = append(f.controls, name+" "+id+"@"+revision)
	return client.ControlResult{EventID: "evt_" + name}, nil
}
func (f *fake) CancelExecution(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return f.control("cancel-execution", id, rev)
}
func (f *fake) CancelWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return f.control("cancel", id, rev)
}
func (f *fake) RetryWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return f.control("retry", id, rev)
}
func (f *fake) HoldWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return f.control("hold", id, rev)
}
func (f *fake) ReleaseWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return f.control("release", id, rev)
}
func (f *fake) Actions(_ context.Context, t client.ActionTarget) ([]client.Action, error) {
	return f.actions[t.WorkID+t.ScopeID], nil
}
func (f *fake) Panels(_ context.Context, t client.ActionTarget) ([]client.Panel, error) {
	if f.panels == nil {
		return nil, &client.Error{Status: 404, Code: "not_found"}
	}
	return f.panels[t.WorkID+t.ScopeID], nil
}
func (f *fake) InvokeAction(_ context.Context, t client.ActionTarget, provider, id, input, _ string) (client.ActionResult, error) {
	f.invoked = append(f.invoked, invoked{t, provider, id, input})
	return client.ActionResult{Event: client.Event{ID: "evt_answer"}, Message: "answered"}, nil
}

func run(id, work, state, created string) client.Run {
	r := client.Run{WorkKind: "github.issue", WorkState: "active"}
	r.ID, r.WorkID, r.State, r.Revision, r.CreatedAt = id, work, state, "3", created
	return r
}

func newFake() *fake {
	parked := run("exe_2", "wrk_1", "parked", "2026-10-28T01:00:00Z")
	parked.Attempt = 2
	done := run("exe_1", "wrk_1", "completed", "2026-10-28T00:00:00Z")
	done.Attempt = 1
	done.Artifacts = []client.Artifact{{ID: "art_log", MediaType: "text/plain", Size: 5}}
	return &fake{
		runs: []client.Run{done, parked, run("exe_3", "wrk_2", "running", "2026-10-28T02:00:00Z")},
		work: map[string]client.Work{
			"wrk_1": {ID: "wrk_1", State: "active", Revision: "7", Annotations: []client.Annotation{{Source: "github", Name: "issue", Value: "open-conduit/open-conduit#88"}}},
			"wrk_2": {ID: "wrk_2", State: "active", Revision: "2", Hold: &client.Hold{Reason: "busy"}, Annotations: []client.Annotation{{Source: "human", Name: "question", Value: "<script>alert(1)</script>"}}},
		},
		actions: map[string][]client.Action{
			"wrk_1":   {{Provider: "human", ID: "answer", Label: "Answer the agent's question", Prompt: "your answer"}},
			"scope_a": {{Provider: "github", ID: "claim", Label: "Claim an issue", Prompt: "issue number"}},
		},
		scopes:  []client.Scope{{ID: "scope_a", Name: "Open Conduit", Enabled: true}},
		live:    map[string]client.LiveRun{"exe_3": {RunID: "exe_3", Outputs: []client.LiveOutput{{Name: "stdout", MediaType: "text/plain", Content: "working…"}}}},
		content: map[string]string{"art_log": "done\n"},
	}
}

const addr = "127.0.0.1:7420"

func setup(t *testing.T) (*fake, *Server, http.Handler) {
	t.Helper()
	f := newFake()
	s, err := New(f, Options{Addr: addr, NewKey: func() string { return "k" }, Now: func() time.Time { return time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return f, s, s.Handler(addr)
}

func do(h http.Handler, method, target string, form url.Values, signedIn *Server) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = "open-conduit.localhost:7420"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if signedIn != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: signedIn.sess})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestNewRefusesNonLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:7420", "192.168.1.4:80", ":7420", "example.com:80", "7420"} {
		if _, err := New(newFake(), Options{Addr: a}); err == nil {
			t.Errorf("%s: accepted", a)
		}
	}
	for _, a := range []string{"127.0.0.1:0", "localhost:7420", "[::1]:7420"} {
		if _, err := New(newFake(), Options{Addr: a}); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err := New(newFake(), Options{Host: "evil.com:80"}); err == nil {
		t.Error("host with a port accepted")
	}
}

func TestSignInAndHostCheck(t *testing.T) {
	_, s, h := setup(t)
	if w := do(h, "GET", "/", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", w.Code)
	}
	if w := do(h, "GET", "/login?token=wrong", nil, nil); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	w := do(h, "GET", "/login?token="+s.login, nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Value != s.sess || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", c)
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("headers: %v", w.Header())
	}
	// A rebound DNS name reaches the listener but is refused, signed in or not.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "attacker.example:7420"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", rec.Code)
	}
	for _, host := range []string{"localhost:7420", addr} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sess})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", host, rec.Code)
		}
	}
	if urls := s.LoginURLs(addr); urls[0] != "http://open-conduit.localhost:7420/login?token="+s.login || urls[1] != "http://"+addr+"/login?token="+s.login {
		t.Fatalf("login urls: %v", urls)
	}
}

func TestChangesNeedCSRFAndSameOrigin(t *testing.T) {
	f, s, h := setup(t)
	form := url.Values{"work": {"wrk_1"}, "provider": {"human"}, "action": {"answer"}, "label": {"Answer"}, "input": {" option 2 "}, "back": {"/run?id=exe_2"}}
	if w := do(h, "POST", "/actions", form, s); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
	form.Set("csrf", s.csrf)
	if w := do(h, "POST", "/actions", form, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("not signed in: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/actions", strings.NewReader(form.Encode()))
	req.Host = "open-conduit.localhost:7420"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://attacker.example")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(f.invoked) != 0 {
		t.Fatalf("cross origin: %d %v", rec.Code, f.invoked)
	}

	// The view's own pages send their origin with a post.
	req = httptest.NewRequest("POST", "/actions", strings.NewReader(form.Encode()))
	req.Host = "open-conduit.localhost:7420"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://open-conduit.localhost:7420")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sess})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("same origin: %d %s", rec.Code, rec.Body)
	}
	f.invoked = nil

	w := do(h, "POST", "/actions", form, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/run?id=exe_2" {
		t.Fatalf("invoke: %d %s", w.Code, w.Header().Get("Location"))
	}
	if len(f.invoked) != 1 || f.invoked[0] != (invoked{client.ActionTarget{WorkID: "wrk_1"}, "human", "answer", "option 2"}) {
		t.Fatalf("invoked: %+v", f.invoked)
	}
	// The outcome shows once on the page it returns to.
	req = httptest.NewRequest("GET", "/work?id=wrk_1", nil)
	req.Host = "open-conduit.localhost:7420"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.sess})
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "answered (event evt_answer)") {
		t.Fatal("flash not shown")
	}

	// A scope action, and a back that would leave the view goes home.
	w = do(h, "POST", "/actions", url.Values{"csrf": {s.csrf}, "scope": {"scope_a"}, "provider": {"github"}, "action": {"claim"}, "input": {"12"}, "back": {"//attacker.example/"}}, s)
	if w.Header().Get("Location") != "/" || f.invoked[1].target.ScopeID != "scope_a" {
		t.Fatalf("scope action: %s %+v", w.Header().Get("Location"), f.invoked)
	}
	if w := do(h, "POST", "/actions", url.Values{"csrf": {s.csrf}, "provider": {"x"}}, s); w.Code != http.StatusBadRequest {
		t.Fatalf("no target: %d", w.Code)
	}
}

func TestControlsUseCurrentRevision(t *testing.T) {
	f, s, h := setup(t)
	for _, c := range []string{"retry", "hold", "release", "cancel"} {
		if w := do(h, "POST", "/work/"+c, url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}}, s); w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", c, w.Code)
		}
	}
	do(h, "POST", "/run/cancel", url.Values{"csrf": {s.csrf}, "run": {"exe_2"}}, s)
	want := []string{"retry wrk_1@7", "hold wrk_1@7", "release wrk_1@7", "cancel wrk_1@7", "cancel-execution exe_2@3"}
	if strings.Join(f.controls, ",") != strings.Join(want, ",") {
		t.Fatalf("controls: %v", f.controls)
	}
	if w := do(h, "POST", "/work/delete", url.Values{"csrf": {s.csrf}}, s); w.Code != 404 {
		t.Fatalf("unknown control: %d", w.Code)
	}
}

func TestOutputLiveAndArtifact(t *testing.T) {
	_, s, h := setup(t)
	var o output
	if err := json.Unmarshal(do(h, "GET", "/output?run=exe_3&format=json", nil, s).Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if !o.Live || o.Text != "working…" {
		t.Fatalf("live: %+v", o)
	}
	body := do(h, "GET", "/output?run=exe_3", nil, s).Body.String()
	if !strings.Contains(body, `data-follow="/output?run=exe_3&amp;pick=log&amp;format=json"`) {
		t.Fatalf("live page should follow:\n%s", body)
	}
	body = do(h, "GET", "/output?run=exe_1&pick=log", nil, s).Body.String()
	if !strings.Contains(body, "done\n</pre>") || strings.Contains(body, "data-follow") {
		t.Fatalf("artifact:\n%s", body)
	}
	if body := do(h, "GET", "/output?run=exe_1&pick=transcript", nil, s).Body.String(); !strings.Contains(body, "run has no transcript") {
		t.Fatal("missing transcript should say so")
	}
}

func TestServePrintsLoginAndStops(t *testing.T) {
	s, err := New(newFake(), Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l, &out) }()
	login := "http://" + l.Addr().String() + "/login?token=" + s.login
	jar := &cookieJar{}
	c := &http.Client{Jar: jar}
	var res *http.Response
	for i := 0; i < 50; i++ {
		if res, err = c.Get(login); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Request.URL.Path != "/" {
		t.Fatalf("login then list: %d %s", res.StatusCode, res.Request.URL)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), login) || !strings.Contains(out.String(), "open-conduit.localhost:") {
		t.Fatalf("output: %s", out.String())
	}
}

// cookieJar is a minimal single-host jar.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) { j.cookies = append(j.cookies, cs...) }
func (j *cookieJar) Cookies(*url.URL) []*http.Cookie          { return j.cookies }
