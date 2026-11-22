package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

const secretCanary = "canary-secret-value-7f3a"

// A value goes to the daemon and comes back in no response, header or
// cookie, on success or on the daemon's refusal.
func TestSecretValueNeverComesBack(t *testing.T) {
	f, s, h := setup(t)
	f.secrets = []client.Secret{{Name: "github_token", Store: "file", Reference: "file:github_token", Resolves: true, ReferencedBy: []string{"/connections/gh/secret_refs/token"}}}
	form := func() url.Values {
		return url.Values{"csrf": {s.csrf}, "name": {"model_key"}, "value": {secretCanary}}
	}
	if w := do(h, "POST", "/secrets", url.Values{"name": {"model_key"}, "value": {secretCanary}}, s); w.Code != http.StatusForbidden || len(f.stored) != 0 {
		t.Fatalf("without CSRF: %d %v", w.Code, f.stored)
	}
	w := do(h, "POST", "/secrets", form(), s)
	if w.Code != http.StatusSeeOther || !slices.Equal(f.stored, []string{"model_key=" + secretCanary}) {
		t.Fatalf("store: %d %v", w.Code, f.stored)
	}
	assertNoCanary(t, w.Header(), w.Body.String())
	page := do(h, "GET", "/secrets", nil, s)
	assertNoCanary(t, page.Header(), page.Body.String())
	for _, want := range []string{"github_token", "/connections/gh/secret_refs/token", `type="password"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("secrets page lacks %q", want)
		}
	}
	f.adminRefused = true
	w = do(h, "POST", "/secrets", url.Values{"csrf": {s.csrf}, "name": {secretCanary}, "value": {secretCanary}}, s)
	assertNoCanary(t, w.Header(), w.Body.String())
}

// postSecretFile posts a secret form as a browser does, with a file part.
func postSecretFile(t *testing.T, h http.Handler, s *Server, target string, fields map[string]string, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	session := testSession(s)
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("csrf", s.sessionToken(session))
	for k, v := range fields {
		_ = m.WriteField(k, v)
	}
	part, err := m.CreateFormFile("value_file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(content))
	_ = m.Close()
	req := httptest.NewRequest("POST", target, &body)
	req.Host = "open-conduit.localhost:7420"
	req.Header.Set("Content-Type", m.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A multi-line value survives from a file on both forms, and what can't be
// stored faithfully is refused without quoting it.
func TestSecretFromAFileKeepsItsLines(t *testing.T) {
	f, s, h := setup(t)
	pem := "-----BEGIN PRIVATE KEY-----\n" + secretCanary + "\n-----END PRIVATE KEY-----"
	w := postSecretFile(t, h, s, "/secrets", map[string]string{"name": "app_key"}, "key.pem", pem+"\r\n")
	if w.Code != http.StatusSeeOther || !slices.Equal(f.stored, []string{"app_key=" + pem}) {
		t.Fatalf("store: %d %q", w.Code, f.stored)
	}
	assertNoCanary(t, w.Header(), w.Body.String())

	f.stored = nil
	f.actions = map[string][]client.Action{"wrk_1": {{Provider: "setup", ID: "give_key", Label: "App key", Prompt: "Choose the key", Secret: "app_key"}}}
	w = postSecretFile(t, h, s, "/actions/secret", map[string]string{"work": "wrk_1", "provider": "setup", "action": "give_key"}, "key.pem", pem+"\n")
	if !slices.Equal(f.stored, []string{"app_key=" + pem}) || len(f.invoked) != 1 || f.invoked[0].in != "app_key" {
		t.Fatalf("action: %q %+v", f.stored, f.invoked)
	}
	assertNoCanary(t, w.Header(), w.Body.String())

	f.stored = nil
	for name, c := range map[string]struct{ typed, file string }{
		"both":   {"typed", pem},
		"binary": {"", "\xff\xfe" + secretCanary},
		"empty":  {"", "\r\n"},
	} {
		w := postSecretFile(t, h, s, "/secrets", map[string]string{"name": "app_key", "value": c.typed}, "key.pem", c.file)
		if len(f.stored) != 0 {
			t.Fatalf("%s: stored %q", name, f.stored)
		}
		assertNoCanary(t, w.Header(), w.Body.String())
	}

	// A PEM key flattened by a password box is refused, not stored broken.
	flat := strings.ReplaceAll(pem, "\n", "")
	w = do(h, "POST", "/secrets", url.Values{"csrf": {s.csrf}, "name": {"app_key"}, "value": {flat}}, s)
	if len(f.stored) != 0 {
		t.Fatalf("stored a flattened key: %q", f.stored)
	}
	assertNoCanary(t, w.Header(), w.Body.String())

	// Past the cap the form is refused and nothing is stored.
	w = postSecretFile(t, h, s, "/secrets", map[string]string{"name": "app_key"}, "key.pem", strings.Repeat("x", maxSecretForm+1))
	if w.Code != http.StatusRequestEntityTooLarge || len(f.stored) != 0 {
		t.Fatalf("oversize: %d %d stored", w.Code, len(f.stored))
	}
}

func assertNoCanary(t *testing.T, header http.Header, body string) {
	t.Helper()
	if strings.Contains(body, secretCanary) {
		t.Fatal("the value is in the body")
	}
	for k, v := range header {
		if strings.Contains(strings.Join(v, " "), secretCanary) || strings.Contains(strings.Join(v, " "), url.QueryEscape(secretCanary)) {
			t.Fatalf("the value is in header %s", k)
		}
	}
}

// The daemon's role check is the gate: its refusal is the page's state.
func TestAdminPagesShowTheDaemonsRefusal(t *testing.T) {
	f, s, h := setup(t)
	f.adminRefused = true
	w := do(h, "GET", "/secrets", nil, s)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "needs the admin role") || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("secrets page: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	w = do(h, "POST", "/operators", url.Values{"csrf": {s.csrf}, "name": {"sam"}, "role": {"admin"}}, s)
	if w.Code != http.StatusForbidden || len(f.issued) != 0 || strings.Contains(w.Body.String(), "oc-issued-canary") {
		t.Fatalf("issue: %d %v", w.Code, f.issued)
	}
}

// An issued token is drawn once, in the POST's own response, and never put
// in a cookie; the list afterwards doesn't carry it.
func TestIssueShowsTheTokenOnce(t *testing.T) {
	f, s, h := setup(t)
	w := do(h, "POST", "/operators", url.Values{"csrf": {s.csrf}, "name": {"sam"}, "role": {"operate"}, "ttl": {"8h"}}, s)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oc-issued-canary") {
		t.Fatalf("issue: %d", w.Code)
	}
	if strings.Contains(strings.Join(w.Header()["Set-Cookie"], " "), "oc-issued-canary") {
		t.Fatal("the token is in a cookie")
	}
	if got := f.issued[0]; got.Name != "sam" || got.Role != "operate" || got.TTL != "8h" {
		t.Fatalf("issued %+v", got)
	}
	if strings.Contains(do(h, "GET", "/operators", nil, s).Body.String(), "oc-issued-canary") {
		t.Fatal("the token came back")
	}
	do(h, "POST", "/operators/revoke", url.Values{"csrf": {s.csrf}, "token_id": {"tok_1"}}, s)
	if !slices.Equal(f.revoked, []string{"tok_1"}) {
		t.Fatalf("revoked %v", f.revoked)
	}
}

// Secrets are checked only on the POST.
func TestDoctorChecksSecretsOnlyWhenAsked(t *testing.T) {
	f, s, h := setup(t)
	f.doctor = &client.Doctor{State: "ok", Checks: []client.Check{{Name: "secrets", Status: "ok", Code: "resolved"}}}
	if body := do(h, "GET", "/doctor", nil, s).Body.String(); !strings.Contains(body, "Without secrets") {
		t.Fatal("plain doctor")
	}
	if w := do(h, "POST", "/doctor", url.Values{"csrf": {s.csrf}}, s); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Checked with secrets") || !strings.Contains(w.Body.String(), "resolved") {
		t.Fatalf("checked doctor: %d", w.Code)
	}
}

// An action asking for a secret stores the value under the name the daemon
// listed, whatever the form says, and is invoked with the name alone.
func TestSecretActionSendsOnlyTheName(t *testing.T) {
	f, s, h := setup(t)
	f.actions = map[string][]client.Action{"wrk_1": {{Provider: "setup", ID: "give_token", Label: "GitHub token", Prompt: "Paste a token", Secret: "github_token"}}}
	form := url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "provider": {"setup"}, "action": {"give_token"}, "name": {"other"}, "value": {secretCanary}, "back": {"/pages/setup"}}
	w := do(h, "POST", "/actions/secret", form, s)
	if w.Code != http.StatusSeeOther || !slices.Equal(f.stored, []string{"github_token=" + secretCanary}) {
		t.Fatalf("store: %d %v", w.Code, f.stored)
	}
	if len(f.invoked) != 1 || f.invoked[0].in != "github_token" {
		t.Fatalf("invoked %+v", f.invoked)
	}
	assertNoCanary(t, w.Header(), w.Body.String())

	// A stored name isn't replaced from a prompt; an empty value uses it.
	f.stored, f.invoked = nil, nil
	f.secrets = []client.Secret{{Name: "github_token", Store: "file", Resolves: true}}
	do(h, "POST", "/actions/secret", url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "provider": {"setup"}, "action": {"give_token"}, "value": {secretCanary}}, s)
	if len(f.stored) != 0 || len(f.invoked) != 0 {
		t.Fatalf("replaced a stored secret: %v %v", f.stored, f.invoked)
	}
	do(h, "POST", "/actions/secret", url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "provider": {"setup"}, "action": {"give_token"}}, s)
	if len(f.stored) != 0 || len(f.invoked) != 1 || f.invoked[0].in != "github_token" {
		t.Fatalf("reuse: %v %+v", f.stored, f.invoked)
	}

	// An action that doesn't ask for a secret stores nothing.
	f.stored, f.invoked = nil, nil
	f.actions["wrk_1"][0].Secret = ""
	do(h, "POST", "/actions/secret", url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "provider": {"setup"}, "action": {"give_token"}, "value": {secretCanary}}, s)
	if len(f.stored) != 0 || len(f.invoked) != 0 {
		t.Fatalf("not a secret action: %v %v", f.stored, f.invoked)
	}
}

// The page reports what this request did: the store and the action apart.
func TestSecretActionReportsWhatHappened(t *testing.T) {
	flashOf := func(w *httptest.ResponseRecorder) flashNote {
		t.Helper()
		for _, c := range w.Result().Cookies() {
			if c.Name == flashCookie {
				v, _ := url.QueryUnescape(c.Value)
				var n flashNote
				if err := json.Unmarshal([]byte(v), &n); err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
		t.Fatal("no flash")
		return flashNote{}
	}
	f, s, h := setup(t)
	f.actions = map[string][]client.Action{"wrk_1": {{Provider: "setup", ID: "give_token", Label: "GitHub token", Prompt: "Paste a token", Secret: "github_token"}}}
	form := func(value string) url.Values {
		return url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "provider": {"setup"}, "action": {"give_token"}, "value": {value}}
	}

	// Stored, but the action failed: it says so, and how to retry.
	f.invokeErr = &client.Error{Status: 409, Code: "conflict", Message: "moved on"}
	n := flashOf(do(h, "POST", "/actions/secret", form(secretCanary), s))
	if !n.Err || !strings.Contains(n.Text, "github_token is stored") || !strings.Contains(n.Text, "moved on") || !strings.Contains(n.Text, "Leave the value empty") || strings.Contains(n.Text, secretCanary) {
		t.Fatalf("store ok, invoke failed: %+v", n)
	}

	// Reusing a stored secret names no store and no PUT.
	f.invokeErr = nil
	f.secrets = []client.Secret{{Name: "github_token", Store: "file", Resolves: true}}
	n = flashOf(do(h, "POST", "/actions/secret", form(""), s))
	if n.Err || strings.Contains(n.Text, "Stored") || strings.Contains(n.Call, "PUT") || !strings.Contains(n.Call, "/actions/setup/give_token") {
		t.Fatalf("reuse: %+v", n)
	}

	// A real store names both calls.
	f.secrets = nil
	n = flashOf(do(h, "POST", "/actions/secret", form(secretCanary), s))
	if n.Err || !strings.Contains(n.Text, "Stored github_token") || !strings.Contains(n.Call, "PUT /v1/secrets/github_token") || !strings.Contains(n.Call, "/actions/setup/give_token") {
		t.Fatalf("store: %+v", n)
	}
}
