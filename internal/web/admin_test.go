package web

import (
	"net/http"
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
