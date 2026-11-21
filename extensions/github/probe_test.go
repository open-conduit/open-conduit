package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

func TestProbes(t *testing.T) {
	var ran []string
	p := Probes{Gh: func(_ context.Context, args ...string) ([]byte, error) {
		ran = append(ran, strings.Join(args, " "))
		switch args[0] {
		case "auth":
			return []byte(`{"hosts":{"github.com":[{"host":"github.com","login":"octo","active":true,"tokenSource":"keyring","state":"success","token":"gho_secret","scopes":"repo"}]}}`), nil
		case "repo":
			return []byte(`[{"nameWithOwner":"octo/widgets"},{"nameWithOwner":"octo/gadgets"}]`), nil
		}
		return nil, errors.New("unexpected")
	}}
	probes := map[string]extension.SetupProbe{}
	for _, x := range p.SetupProbes() {
		probes[x.Name] = x
	}
	r, err := probes["accounts"].Run(context.Background(), nil)
	b, _ := json.Marshal(r.Output)
	if err != nil || !strings.Contains(string(b), `"login":"octo","active":true,"token_source":"keyring","state":"success"`) || strings.Contains(string(b), "gho_") {
		t.Fatalf("accounts: %s %v", b, err)
	}
	r, err = probes["repos"].Run(context.Background(), map[string]string{"owner": "octo"})
	b, _ = json.Marshal(r.Output)
	if err != nil || string(b) != `{"repos":["octo/widgets","octo/gadgets"]}` || r.Command != "gh repo list octo" {
		t.Fatalf("repos: %s %v", b, err)
	}
	for _, owner := range []string{"", "--help", "octo; rm -rf", "a/b"} {
		if _, err = probes["repos"].Run(context.Background(), map[string]string{"owner": owner}); !errors.Is(err, extension.ErrProbeInput) {
			t.Errorf("owner %q: %v", owner, err)
		}
	}
	if strings.Join(ran, "|") != "auth status --json hosts|repo list octo --limit 200 --json nameWithOwner" {
		t.Fatalf("ran: %q", ran)
	}
}

// probeConn is a connection as the host grants one to a probe run.
func probeConn(api, auth string, secrets map[string]string) extension.ProbeConnection {
	cfg, _ := json.Marshal(map[string]string{"api_url": api, "auth": auth, "app_id": map[bool]string{true: "7"}[auth == "app"]})
	return extension.ProbeConnection{ID: "gh", Type: Type, Config: cfg, Secret: func(_ context.Context, name string) (string, error) {
		v, ok := secrets[name]
		if !ok {
			return "", ErrCredential
		}
		return v, nil
	}}
}

func runProbe(t *testing.T, p Probes, ctx context.Context, name string, args map[string]string) (extension.ProbeResult, error) {
	t.Helper()
	for _, x := range p.SetupProbes() {
		if x.Name == name {
			return x.Run(ctx, args)
		}
	}
	t.Fatalf("no probe %s", name)
	return extension.ProbeResult{}, nil
}

func TestProbesThroughATokenConnection(t *testing.T) {
	const canary = "ghp_canary_value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+canary {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case "/user/repos":
			_, _ = w.Write([]byte(`[{"full_name":"octo/widgets"},{"full_name":"other/thing"},{"full_name":"OCTO/gadgets"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	gh := func(context.Context, ...string) ([]byte, error) {
		t.Fatal("a run through a connection must not use gh")
		return nil, nil
	}
	p := Probes{Gh: gh, HTTP: srv.Client()}
	ctx := extension.WithProbeConnection(context.Background(), probeConn(srv.URL, "token", map[string]string{SecretToken: canary}))

	r, err := runProbe(t, p, ctx, "accounts", map[string]string{"connection": "gh"})
	b, _ := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"login":"octo"`) || !strings.Contains(string(b), `"token_source":"connection"`) || strings.Contains(string(b), canary) {
		t.Fatalf("accounts: %s %v", b, err)
	}
	r, err = runProbe(t, p, ctx, "repos", map[string]string{"owner": "octo", "connection": "gh"})
	b, _ = json.Marshal(r.Output)
	if err != nil || string(b) != `{"repos":["octo/widgets","OCTO/gadgets"]}` || strings.Contains(r.Command, canary) {
		t.Fatalf("repos: %s %q %v", b, r.Command, err)
	}
}

func TestProbesThroughAConnectionFailClosed(t *testing.T) {
	gh := func(context.Context, ...string) ([]byte, error) {
		t.Fatal("a refused connection must not fall back to gh")
		return nil, nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials ghp_echoed_secret"}`))
	}))
	defer srv.Close()
	p := Probes{Gh: gh, HTTP: srv.Client()}

	// Named, but the host granted no connection for this run.
	if _, err := runProbe(t, p, context.Background(), "repos", map[string]string{"owner": "octo", "connection": "gh"}); err == nil {
		t.Fatal("an ungranted connection ran")
	}
	// Granted a different connection from the one named.
	ctx := extension.WithProbeConnection(context.Background(), probeConn(srv.URL, "token", map[string]string{SecretToken: "x"}))
	if _, err := runProbe(t, p, ctx, "repos", map[string]string{"owner": "octo", "connection": "other"}); err == nil {
		t.Fatal("a mismatched connection ran")
	}
	// GitHub's own error text never reaches the agent.
	_, err := runProbe(t, p, ctx, "accounts", map[string]string{"connection": "gh"})
	if err == nil || strings.Contains(err.Error(), "ghp_") || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestProbeReposThroughAnApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var minted map[string]any
	revoked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/octo/installation":
			_, _ = w.Write([]byte(`{"id":42}`))
		case r.URL.Path == "/app/installations/42/access_tokens":
			_ = json.NewDecoder(r.Body).Decode(&minted)
			_, _ = w.Write([]byte(`{"token":"ghs_inst","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/installation/repositories" && r.Header.Get("Authorization") == "Bearer ghs_inst":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"octo/widgets"}]}`))
		case r.URL.Path == "/installation/token" && r.Method == http.MethodDelete:
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := Probes{HTTP: srv.Client()}
	ctx := extension.WithProbeConnection(context.Background(), probeConn(srv.URL, "app", map[string]string{SecretAppPrivateKey: keyPEM}))
	r, err := runProbe(t, p, ctx, "repos", map[string]string{"owner": "octo", "connection": "gh"})
	b, _ := json.Marshal(r.Output)
	if err != nil || string(b) != `{"repos":["octo/widgets"]}` {
		t.Fatalf("repos: %s %v", b, err)
	}
	perms, _ := json.Marshal(minted["permissions"])
	if string(perms) != `{"metadata":"read"}` {
		t.Fatalf("minted with %s, want metadata read alone", perms)
	}
	if !revoked {
		t.Fatal("the listing's token was not revoked")
	}
}
