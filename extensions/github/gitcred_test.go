package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/storage"
)

// workStore serves one Work to the tools' binding.
type workStore struct{ w domain.Work }

type workTx struct {
	storage.Tx
	w domain.Work
}

func (s workStore) Transact(_ context.Context, _ storage.Guard, f func(storage.Tx) error) error {
	return f(workTx{w: s.w})
}

func (t workTx) Work(id domain.ID) (domain.Work, error) {
	if id != t.w.ID {
		return domain.Work{}, storage.ErrNotFound
	}
	return t.w, nil
}

// apiOnly sends every request for api.github.com to the fake, so the
// client has github.com's real hosts.
type apiOnly struct{ to *url.URL }

func (a apiOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = a.to.Scheme, a.to.Host
	return http.DefaultTransport.RoundTrip(r)
}

// gitCredLab is a GitHub App connection, the tools over it and a Work on
// issue 3 of octo/widgets.
func gitCredLab(t *testing.T, auth string) (*githubtest.Server, *gh.Tools, credCall) {
	t.Helper()
	return gitCredLabWith(t, auth, nil)
}

// gitCredLabWith is gitCredLab with the further connections (ID to
// config) the scope's bindings may name, all signing in with the same key.
func gitCredLabWith(t *testing.T, auth string, conns map[string]string) (*githubtest.Server, *gh.Tools, credCall) {
	t.Helper()
	fake := githubtest.New(repo, "pat")
	t.Cleanup(fake.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddApp("42", &key.PublicKey)
	fake.AddApp("43", &key.PublicKey)
	fake.AddApp("44", &key.PublicKey)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cfg, err := gh.ParseConnection([]byte(auth))
	if err != nil {
		t.Fatal(err)
	}
	to, _ := url.Parse(fake.URL)
	secrets := func(_ context.Context, name string) (string, error) {
		if name == gh.SecretToken {
			return "pat", nil
		}
		return pemText, nil
	}
	client, err := gh.NewClient(cfg, secrets, gh.ClientOptions{HTTP: &http.Client{Transport: apiOnly{to}}, Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	work := domain.Work{ID: "work/3", ScopeID: "eng", CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: gh.IssueKey(3, "")}}}
	writers := gh.NewWriters(func(scope domain.ID, id string) (json.RawMessage, gh.SecretFunc, bool) {
		c, ok := conns[id]
		return json.RawMessage(c), secrets, ok && scope == "eng"
	}, fake.Now)
	tools, err := gh.NewTools(gh.ToolsOptions{Store: workStore{work}, Sources: func() []gh.Source { return []gh.Source{{Settings: s, Connection: "github", Client: client}} }, Now: fake.Now, Writers: writers})
	if err != nil {
		t.Fatal(err)
	}
	var op extension.Operation
	for _, o := range tools.Registration().Operations {
		if o.Name == gh.OpGitCredential {
			op = o
		}
	}
	call := credCall{op: op, work: work.ID}
	return fake, tools, call
}

// credCall calls github.git.credential as an execution of the Work, whose
// binding names connection.
type credCall struct {
	op         extension.Operation
	work       domain.ID
	connection string
}

func (c credCall) as(connection string) credCall {
	c.connection = connection
	return c
}

func (c credCall) run(ctx context.Context, exec domain.ID, in string) (domain.Payload, error) {
	return c.op.Handler(ctx, extension.Authority{ScopeID: "eng", WorkID: c.work, ExecutionID: exec, Connection: c.connection}, domain.Payload{Data: json.RawMessage(in)})
}

func (c credCall) do(exec domain.ID, in string) (domain.Payload, error) {
	return c.run(context.Background(), exec, in)
}

func (c credCall) withRequestID(exec domain.ID, in string) (domain.Payload, error) {
	return c.run(extension.WithCallRequestID(context.Background(), "retry-1"), exec, in)
}

type credential struct {
	Username, Password string
	ExpiresAt          string `json:"expires_at"`
}

func credentialOf(t *testing.T, p domain.Payload) credential {
	t.Helper()
	var c credential
	if err := json.Unmarshal(p.Data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGitCredential: git's credential for the Work's repository is an App
// installation token minted for that one repository with contents: write,
// handed out again while it lasts, and only for https on github.com. The
// host revokes every token an execution was given once its segment ends,
// or in the sweep if it missed that end.
func TestGitCredential(t *testing.T) {
	fake, tools, call := gitCredLab(t, `{"auth":"app","app_id":"42"}`)
	out, err := call.do("x/1", `{"protocol":"https","host":"github.com","path":"octo/widgets.git"}`)
	if err != nil {
		t.Fatal(err)
	}
	c := credentialOf(t, out)
	if c.Username != "x-access-token" || !strings.HasPrefix(c.Password, "ghs_") || c.ExpiresAt == "" {
		t.Fatalf("credential %+v", c)
	}
	scope, ok := fake.Scope(c.Password)
	if !ok || !slices.Equal(scope.Repositories, []string{"widgets"}) || len(scope.Permissions) != 1 || scope.Permissions["contents"] != "write" {
		t.Fatalf("minted for %+v (valid %v), want widgets with contents: write only", scope, ok)
	}
	// Asked again (git's fetch, then its push), the same token.
	again, _ := call.do("x/1", `{"protocol":"https","host":"github.com"}`)
	if credentialOf(t, again).Password != c.Password || fake.InstallationTokensIssued() != 1 {
		t.Fatalf("minted %d tokens for one execution", fake.InstallationTokensIssued())
	}
	// Another execution gets its own.
	other, _ := call.do("x/2", `{"protocol":"https","host":"github.com"}`)
	if o := credentialOf(t, other).Password; o == "" || o == c.Password {
		t.Fatalf("x/2 got %q", o)
	}
	// Any other URL gets no credential: plain http, another host, another
	// repository's path.
	for _, in := range []string{
		`{"protocol":"http","host":"github.com"}`,
		`{"protocol":"https","host":"gist.github.com"}`,
		`{"protocol":"https","host":"evil.example","path":"octo/widgets.git"}`,
		`{"protocol":"https","host":"github.com","path":"octo/other.git"}`,
	} {
		out, err := call.do("x/1", in)
		if err != nil || credentialOf(t, out) != (credential{}) {
			t.Errorf("%s: %s %v, want no credential", in, out.Data, err)
		}
	}
	if _, err := call.do("x/1", `{"protocol":"https","host":"github.com","user":"me"}`); err == nil {
		t.Error("unknown input accepted")
	}
	// A call with a request ID would keep the token for replay.
	var op *extension.OperationError
	if _, err := call.withRequestID("x/1", `{"protocol":"https","host":"github.com"}`); !errors.As(err, &op) || op.Code != "request_id_refused" {
		t.Errorf("a call with a request ID: %v", err)
	}
	if err := tools.RevokeGitTokens(context.Background(), "x/1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Scope(c.Password); ok {
		t.Fatal("the execution's token still works once its segment ended")
	}
	if _, ok := fake.Scope(credentialOf(t, other).Password); !ok {
		t.Fatal("revoking x/1's tokens revoked x/2's")
	}
	// The sweep revokes what's left of executions that aren't running.
	if err := tools.SweepGitTokens(context.Background(), func(ids []domain.ID) (map[domain.ID]bool, error) {
		return map[domain.ID]bool{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Scope(credentialOf(t, other).Password); ok {
		t.Fatal("the sweep left an ended execution's token working")
	}
	// The next segment of x/1 gets a new token.
	next, _ := call.do("x/1", `{"protocol":"https","host":"github.com"}`)
	if p := credentialOf(t, next).Password; p == "" || p == c.Password {
		t.Fatalf("next segment got %q", p)
	}
	// A sweep that can't tell what is running revokes nothing.
	if err := tools.SweepGitTokens(context.Background(), func([]domain.ID) (map[domain.ID]bool, error) { return nil, errors.New("down") }); err == nil {
		t.Fatal("a failed listing was taken as nothing running")
	}
	if _, ok := fake.Scope(credentialOf(t, next).Password); !ok {
		t.Fatal("a failed listing revoked a token")
	}
}

// TestGitCredentialStaticToken: a connection signed in with a token has
// nothing to mint, and says so.
func TestGitCredentialStaticToken(t *testing.T) {
	fake, _, call := gitCredLab(t, `{}`)
	_, err := call.do("x/1", `{"protocol":"https","host":"github.com"}`)
	var op *extension.OperationError
	if !errors.As(err, &op) || op.Code != "github_static_token" {
		t.Fatalf("token connection: %v", err)
	}
	if fake.InstallationTokensIssued() != 0 {
		t.Fatal("minted a token on a token connection")
	}
}

// TestGitHandlerScopes: the fake's git endpoint enforces what a token was
// minted for, as github.com does (the Docker test pushes through it).
func TestGitHandlerScopes(t *testing.T) {
	fake, _, call := gitCredLab(t, `{"auth":"app","app_id":"42"}`)
	out, _ := call.do("x/1", `{"protocol":"https","host":"github.com"}`)
	tok := credentialOf(t, out).Password
	h := fake.GitHandler(t.TempDir(), "/nonexistent-git")
	for path, want := range map[string]int{
		"/octo/other.git/info/refs?service=git-receive-pack":       http.StatusUnauthorized,
		"/elsewhere/widgets.git/info/refs?service=git-upload-pack": http.StatusUnauthorized,
	} {
		r, _ := http.NewRequest(http.MethodGet, "https://github.com"+path, nil)
		r.SetBasicAuth("x-access-token", tok)
		w := &recorder{h: http.Header{}}
		h.ServeHTTP(w, r)
		if w.code != want {
			t.Errorf("%s: %d, want %d", path, w.code, want)
		}
	}
}

type recorder struct {
	h    http.Header
	code int
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }

// TestRunEnvironment: a connection with git "bot" gives each run of its
// repository's Work the toolbelt as git's only credential helper, signing
// off and the bot user of the App that mints git's tokens (the bot App,
// else the sign-in App) as author and committer, with the noreply address
// GitHub links to that user. The identity is looked up once. A connection
// without git "bot" gives only the static token helper, and git "bot"
// needs an App.
func TestRunEnvironment(t *testing.T) {
	for _, c := range []struct{ name, auth, app string }{
		{"sign-in App", `{"auth":"app","app_id":"42","git":"bot"}`, "42"},
		{"bot App", `{"auth":"token","bot_app_id":"43","git":"bot"}`, "43"},
		{"bot App over a sign-in App", `{"auth":"app","app_id":"42","bot_app_id":"43","git":"bot"}`, "43"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake, tools, call := gitCredLab(t, c.auth)
			_, login, id := githubtest.AppBot(c.app)
			email := fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
			for i := 0; i < 2; i++ {
				env, err := tools.RunEnvironment(context.Background(), extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: "eng", WorkID: call.work, ExecutionID: "x/1"}})
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{
					"GIT_CONFIG_COUNT": "4",
					"GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "",
					"GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!os git-credential",
					"GIT_CONFIG_KEY_2": "commit.gpgsign", "GIT_CONFIG_VALUE_2": "false",
					"GIT_CONFIG_KEY_3": "tag.gpgsign", "GIT_CONFIG_VALUE_3": "false",
					"GIT_AUTHOR_NAME": login, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_NAME": login, "GIT_COMMITTER_EMAIL": email,
				}
				if !maps.Equal(env, want) {
					t.Fatalf("env %v, want %v", env, want)
				}
			}
			n := 0
			for _, r := range fake.Requests() {
				if r == "GET /app" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("GET /app %d times, want once", n)
			}
		})
	}
	_, tools, call := gitCredLab(t, `{"auth":"app","app_id":"42"}`)
	tokenOnly := map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_0": gh.TokenHelper}
	if env, err := tools.RunEnvironment(context.Background(), extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: "eng", WorkID: call.work, ExecutionID: "x/1"}}); err != nil || !maps.Equal(env, tokenOnly) {
		t.Errorf("without git bot: %v, %v; want only the static token helper", env, err)
	}
	if _, err := gh.ParseConnection([]byte(`{"auth":"token","git":"bot"}`)); err == nil {
		t.Error(`git "bot" on a token connection without a bot App was accepted`)
	}
	if _, err := gh.ParseConnection([]byte(`{"git":"someone"}`)); err == nil {
		t.Error("an unknown git was accepted")
	}
}

// TestTokenHelper runs the static token helper as git would: `get` answers
// with GH_TOKEN, else GITHUB_TOKEN; without either, and for any other
// action, it says nothing, so git asks its next helper.
func TestTokenHelper(t *testing.T) {
	run := func(action string, env ...string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", strings.TrimPrefix(gh.TokenHelper, "!")+` "$@"`, "helper", action)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return string(out)
	}
	for _, c := range []struct {
		action string
		env    []string
		want   string
	}{
		{"get", []string{"GH_TOKEN=gh-tok", "GITHUB_TOKEN=other"}, "username=x-access-token\npassword=gh-tok\n"},
		{"get", []string{"GITHUB_TOKEN=github-tok"}, "username=x-access-token\npassword=github-tok\n"},
		{"get", nil, ""},
		{"store", []string{"GH_TOKEN=gh-tok"}, ""},
	} {
		if got := run(c.action, c.env...); got != c.want {
			t.Errorf("%s with %v: %q, want %q", c.action, c.env, got, c.want)
		}
	}
}

// TestRunEnvironmentBinding: a binding naming another `github` connection
// with git "bot" (a second App) commits as that App's bot user and pushes
// with a token that App minted, while a binding in the same scope naming
// none, the entry's, or one the extension can't act as stays on the entry's
// connection. The binding's connection decides: one without git "bot"
// leaves git the operator's (with only the static token helper) even though
// the entry sets it, and one that
// can't be used fails the run rather than falling back.
func TestRunEnvironmentBinding(t *testing.T) {
	fake, tools, call := gitCredLabWith(t, `{"auth":"app","app_id":"42","git":"bot"}`, map[string]string{
		"github-reviewer": `{"auth":"app","app_id":"44","git":"bot"}`,
		"github-plain":    `{"auth":"app","app_id":"44"}`,
		"github-ghe":      `{"api_url":"https://ghe.example/api/v3","auth":"app","app_id":"44","git":"bot"}`,
	})
	env := func(conn string) (map[string]string, error) {
		return tools.RunEnvironment(context.Background(), extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: "eng", WorkID: call.work, ExecutionID: domain.ID("x/" + conn)}, Connection: conn})
	}
	minted := func(conn string) string {
		t.Helper()
		out, err := call.as(conn).do(domain.ID("x/"+conn), `{"protocol":"https","host":"github.com","path":"octo/widgets.git"}`)
		if err != nil {
			t.Fatalf("%s: %v", conn, err)
		}
		app, ok := fake.TokenApp(credentialOf(t, out).Password)
		if !ok {
			t.Fatalf("%s: credential not an installation token", conn)
		}
		return app
	}
	for conn, app := range map[string]string{"github-reviewer": "44", "": "42", "github": "42", "slack": "42"} {
		e, err := env(conn)
		if err != nil {
			t.Fatalf("%q: %v", conn, err)
		}
		_, login, _ := githubtest.AppBot(app)
		if e["GIT_AUTHOR_NAME"] != login || e["GIT_COMMITTER_NAME"] != login || e["GIT_CONFIG_VALUE_1"] != "!os git-credential" {
			t.Errorf("binding naming %q: env %v, want App %s's bot", conn, e, app)
		}
		if got := minted(conn); got != app {
			t.Errorf("binding naming %q: git token minted by App %s, want %s", conn, got, app)
		}
	}
	if e, err := env("github-plain"); err != nil || e["GIT_CONFIG_COUNT"] != "1" || e["GIT_CONFIG_VALUE_0"] != gh.TokenHelper || e["GIT_AUTHOR_NAME"] != "" {
		t.Errorf("binding connection without git bot: %v, %v; want only the static token helper", e, err)
	}
	if _, err := env("github-ghe"); err == nil {
		t.Error("a binding connection on another GitHub fell back to the entry's")
	}
	var op *extension.OperationError
	if _, err := call.as("github-ghe").do("x/ghe", `{"protocol":"https","host":"github.com"}`); !errors.As(err, &op) || op.Code != "github_writer" {
		t.Errorf("credential for a binding connection on another GitHub: %v", err)
	}
}
