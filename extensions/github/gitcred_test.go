package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
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
	fake := githubtest.New(repo, "pat")
	t.Cleanup(fake.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddApp("42", &key.PublicKey)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cfg, err := gh.ParseConnection([]byte(auth))
	if err != nil {
		t.Fatal(err)
	}
	to, _ := url.Parse(fake.URL)
	client, err := gh.NewClient(cfg, func(_ context.Context, name string) (string, error) {
		if name == gh.SecretToken {
			return "pat", nil
		}
		return pemText, nil
	}, gh.ClientOptions{HTTP: &http.Client{Transport: apiOnly{to}}, Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	work := domain.Work{ID: "work/3", ScopeID: "eng", CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: gh.IssueKey(3, "")}}}
	tools, err := gh.NewTools(gh.ToolsOptions{Store: workStore{work}, Sources: func() []gh.Source { return []gh.Source{{Settings: s, Client: client}} }, Now: fake.Now})
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

// credCall calls github.git.credential as an execution of the Work.
type credCall struct {
	op   extension.Operation
	work domain.ID
}

func (c credCall) run(ctx context.Context, exec domain.ID, in string) (domain.Payload, error) {
	return c.op.Handler(ctx, extension.Authority{ScopeID: "eng", WorkID: c.work, ExecutionID: exec}, domain.Payload{Data: json.RawMessage(in)})
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
