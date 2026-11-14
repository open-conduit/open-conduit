package integration_test

// The toolbelt's git transport: git in a run reaches its repository through
// `os git-remote` and the host, which signs each request in with what the
// operation serving git.credential answers. Git clones, fetches and pushes
// with nothing but the run's config, while nothing in the run (git's own
// credential machinery, `os git-credential`, a hand-made toolbelt request)
// gets the token. The repository is git's own smart HTTP (git http-backend)
// over TLS on loopback; no real service is called.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
)

// gitTransportToken is what the repository's host accepts; it must never
// reach the run.
const gitTransportToken = "ghs_never_in_the_run"

type gitTransportLab struct {
	t                  *testing.T
	h                  *managementHarness
	root, bare, osDir  string
	host               string
	ctx                extension.ExecutionContext
	mu                 sync.Mutex
	signedIn, anon     int
	credentialCalls    int
	gate               func() *extension.WaitPlan
	upstreamAuthorized []string
}

func newGitTransportLab(t *testing.T) *gitTransportLab {
	t.Helper()
	osBin := builtToolbelt()
	if osBin == "" {
		t.Skip("cmd/os can't be built here")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	l := &gitTransportLab{t: t, root: t.TempDir(), osDir: filepath.Dir(osBin)}
	// The repository, served by git's own smart HTTP, which takes only the
	// token: anything else is asked to sign in.
	l.bare = filepath.Join(l.root, "srv")
	repo := filepath.Join(l.bare, "octo", "widgets.git")
	other := filepath.Join(l.bare, "octo", "other.git")
	seed := filepath.Join(l.root, "seed")
	for _, args := range [][]string{
		{"init", "-q", "--bare", repo}, {"-C", repo, "config", "http.receivepack", "true"},
		{"init", "-q", "--bare", other},
		{"init", "-q", "-b", "main", seed},
		{"-C", seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", seed, "push", "-q", repo, "main"},
		{"-C", seed, "push", "-q", other, "main"},
	} {
		l.git(nil, args...)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + l.bare, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		l.mu.Lock()
		if ok && user == "x-access-token" && pass == gitTransportToken {
			l.signedIn++
			l.upstreamAuthorized = append(l.upstreamAuthorized, r.Method+" "+r.URL.Path)
		} else {
			l.anon++
		}
		l.mu.Unlock()
		if !ok || pass != gitTransportToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Go's CGI host takes no chunked body, which git's hosts do (git
		// itself sends a large push chunked): hand it the body whole.
		if len(r.TransferEncoding) > 0 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	l.host = u.Host

	cfg := strings.ReplaceAll(managementConfig, `"declare_outcome", "create_work", "wait"]`, `"declare_outcome", "create_work", "wait", "codehost.git.credential"]`)
	l.h = newManagementHarnessWith(t, cfg, func(r *config.Registry) { r.Operations = append(r.Operations, "codehost.git.credential") })
	l.h.gitUpstream = srv.Client().Transport
	// The code host's extension: a token for widgets on this host only.
	// A gate of another extension's, as the human extension's is.
	if err := l.h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "watcher",
		Gate: func(_ context.Context, _ extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.gate == nil || c.Capability != extension.CapabilityGitCredential {
				return nil, nil
			}
			return l.gate(), nil
		}}); err != nil {
		t.Fatal(err)
	}
	if err := l.h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "codehost",
		Operations: []extension.Operation{{Name: "codehost.git.credential", Serves: extension.CapabilityGitCredential, Handler: func(_ context.Context, _ extension.Authority, in domain.Payload) (domain.Payload, error) {
			var req struct{ Protocol, Host, Path string }
			_ = json.Unmarshal(in.Data, &req)
			l.mu.Lock()
			l.credentialCalls++
			l.mu.Unlock()
			if req.Protocol != "https" || req.Host != l.host || strings.TrimSuffix(req.Path, ".git") != "octo/widgets" {
				return domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}, nil
			}
			return domain.Payload{Schema: "fixture.v1", Data: []byte(`{"username":"x-access-token","password":"` + gitTransportToken + `"}`)}, nil
		}}}}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan extension.ExecutionContext, 1)
	l.h.executor.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		seen <- r.Context
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	var root client.Event
	l.h.mustCLI(&root, "events", "emit", "--scope", "ops", "--type", "operator.requested", "--delivery-id", "git-transport")
	if _, err := l.h.scheduler.Start(context.Background(), domain.ID(root.RoutedWorkID)); err != nil {
		t.Fatal(err)
	}
	l.ctx = <-seen
	return l
}

// env is the run's: the toolbelt client, `os` on PATH, and the layer an
// extension serving git.credential for the host returns, over a git that
// knows nothing of the operator's config.
func (l *gitTransportLab) env() []string {
	vars := map[string]string{
		"PATH": l.osDir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME": l.root,
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_SSL_NO_VERIFY": "1",
		"OS_ENDPOINT": l.ctx.ToolbeltEndpoint, "OS_TOKEN_FILE": l.ctx.ToolbeltTokenFile,
		"GIT_AUTHOR_NAME": "bot", "GIT_AUTHOR_EMAIL": "bot@example.test", "GIT_COMMITTER_NAME": "bot", "GIT_COMMITTER_EMAIL": "bot@example.test",
	}
	extension.ApplyEnv(vars, extension.GitTransportLayer(l.host))
	var out []string
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	return out
}

// git runs git with env (the test's own when nil), failing the test if it
// fails.
func (l *gitTransportLab) git(env []string, args ...string) string {
	l.t.Helper()
	out, err := l.try(env, "", args...)
	if err != nil {
		l.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (l *gitTransportLab) try(env []string, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if env == nil {
		env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (l *gitTransportLab) remote(path string) string { return "https://" + l.host + "/" + path }

func (l *gitTransportLab) branch(name string) string {
	out, _ := exec.Command("git", "--git-dir", filepath.Join(l.bare, "octo", "widgets.git"), "rev-parse", "--verify", "-q", "refs/heads/"+name).Output()
	return strings.TrimSpace(string(out))
}

// TestGitTransportSignsGitInOnTheHost: git clones, fetches, pushes and
// deletes a branch through the transport, every request reaching the
// repository signed in; the token is in none of what git, `os` or the
// run's files hold.
func TestGitTransportSignsGitInOnTheHost(t *testing.T) {
	l := newGitTransportLab(t)
	env := l.env()
	ws := filepath.Join(l.root, "ws")
	l.git(env, "clone", "-q", l.remote("octo/widgets.git"), ws)
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.git(env, "-C", ws, "add", "f")
	l.git(env, "-C", ws, "commit", "-qm", "f")
	l.git(env, "-C", ws, "push", "-q", "origin", "HEAD:refs/heads/feature")
	head := strings.TrimSpace(l.git(env, "-C", ws, "rev-parse", "HEAD"))
	if got := l.branch("feature"); got != head {
		t.Fatalf("pushed feature = %q, want %q", got, head)
	}
	l.git(env, "-C", ws, "fetch", "-q", "origin")
	if out := l.git(env, "-C", ws, "ls-remote", "origin"); !strings.Contains(out, head+"\trefs/heads/feature") {
		t.Fatalf("ls-remote: %s", out)
	}
	// A push that only deletes sends no pack.
	l.git(env, "-C", ws, "push", "-q", "origin", ":refs/heads/feature")
	if got := l.branch("feature"); got != "" {
		t.Fatalf("feature still there after its delete: %q", got)
	}
	// Nothing to push is no request at all.
	l.git(env, "-C", ws, "push", "-q", "origin", "refs/remotes/origin/main:refs/heads/main")
	l.mu.Lock()
	signedIn, anon := l.signedIn, l.anon
	l.mu.Unlock()
	if signedIn == 0 || anon != 0 {
		t.Fatalf("repository saw %d signed-in and %d anonymous requests, want only signed-in ones", signedIn, anon)
	}

	// Nothing in the run gets the token: git's own credential machinery
	// (whose helpers the layer leaves alone), the retired `os
	// git-credential`, and a hand-made toolbelt request naming the
	// capability.
	fill, _ := l.try(env, "protocol=https\nhost="+l.host+"\npath=octo/widgets.git\n\n", "credential", "fill")
	cred := exec.Command(filepath.Join(l.osDir, "os"), "git-credential", "get")
	cred.Env, cred.Stdin = env, strings.NewReader("protocol=https\nhost="+l.host+"\n\n")
	helper, _ := cred.CombinedOutput()
	token, err := os.ReadFile(l.ctx.ToolbeltTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawToolbeltCall(t, l.ctx.ToolbeltEndpoint, strings.TrimSpace(string(token)), `{"operation":"codehost.git.credential","capability":"git.credential","input":{"data":{"protocol":"https","host":"`+l.host+`","path":"octo/widgets.git"}}}`)
	own := rawToolbeltCall(t, l.ctx.ToolbeltEndpoint, strings.TrimSpace(string(token)), `{"operation":"codehost.git.credential","input":{"data":{"protocol":"https","host":"`+l.host+`","path":"octo/widgets.git"}}}`)
	for what, out := range map[string]string{"git credential fill": fill, "os git-credential get": string(helper), "a toolbelt call naming the capability": raw, "the agent's own call": own} {
		if strings.Contains(out, gitTransportToken) {
			t.Fatalf("%s got the token: %s", what, out)
		}
	}
	for what, out := range map[string]string{"a toolbelt call naming the capability": raw, "the agent's own call": own} {
		if !strings.Contains(out, extension.CodeCapabilityOnly) {
			t.Fatalf("%s wasn't refused %s: %s", what, extension.CodeCapabilityOnly, out)
		}
	}
	files, _ := exec.Command("grep", "-rl", gitTransportToken, ws, l.root+"/.gitconfig").CombinedOutput()
	if strings.Contains(string(files), ws) {
		t.Fatalf("the token is in the workspace: %s", files)
	}
}

// TestGitTransportReachesOtherRepositoriesDirectly: a repository no
// operation gives a credential for is reached by git's own https helper,
// unsigned, as git would without the transport: the host forwards nothing
// it can't sign in, so it is no proxy.
func TestGitTransportReachesOtherRepositoriesDirectly(t *testing.T) {
	l := newGitTransportLab(t)
	out, err := l.try(l.env(), "", "ls-remote", l.remote("octo/other.git"))
	if err == nil {
		t.Fatalf("an unsigned ls-remote of a private repository went through: %s", out)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.anon == 0 || l.signedIn != 0 || l.credentialCalls != 1 {
		t.Fatalf("repository saw %d anonymous and %d signed-in requests, %d credential calls; want git's own unsigned request after one ask", l.anon, l.signedIn, l.credentialCalls)
	}
}

// TestGitTransportGatedSignIn: a gate on the sign-in holds git's command,
// which fails with what the gate said, and asks once per command: the
// credential is asked for when the command opens its session, not for
// each of its requests.
func TestGitTransportGatedSignIn(t *testing.T) {
	l := newGitTransportLab(t)
	env := l.env()
	ws := filepath.Join(l.root, "ws")
	l.git(env, "clone", "-q", l.remote("octo/widgets.git"), ws)
	l.mu.Lock()
	asked := l.credentialCalls
	l.mu.Unlock()
	if asked != 1 {
		t.Fatalf("one clone asked for the credential %d times, want once", asked)
	}
	l.mu.Lock()
	l.gate = func() *extension.WaitPlan {
		return &extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.reported.v1"}}, Description: "a person's approval of git's sign-in"}, Output: domain.Payload{Schema: "fixture.v1", Data: []byte(`{"next":"git's sign-in needs a person's approval. End your turn now"}`)}}
	}
	l.mu.Unlock()
	out, err := l.try(env, "", "-C", ws, "fetch", "origin")
	if err == nil || !strings.Contains(out, "os: no git sign-in yet: git's sign-in needs a person's approval. End your turn now") {
		t.Fatalf("a held sign-in: %v\n%s", err, out)
	}
	if strings.Contains(out, gitTransportToken) {
		t.Fatalf("the held sign-in printed the token: %s", out)
	}
}

// rawToolbeltCall posts body to the execution's toolbelt as anything in the
// run could, and returns the answer.
func rawToolbeltCall(t *testing.T, endpoint, token, body string) string {
	t.Helper()
	socket := strings.TrimPrefix(endpoint, "unix://")
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	req, _ := http.NewRequest("POST", "http://localhost/v1/toolbelt/call", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, buf.String())
}
