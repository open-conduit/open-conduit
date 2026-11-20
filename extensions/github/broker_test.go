package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

// brokerFor starts a broker that holds the key of App appID (registered
// with fake) and serves the daemon known as client, limited to repos and
// installations. It returns the broker and the PEM of the daemon's own
// key, which GitHub doesn't know.
func brokerFor(t *testing.T, fake *githubtest.Server, appID string, appKey *rsa.PrivateKey, client string, repos []string, installations []int64) (*githubtest.Broker, string) {
	t.Helper()
	daemonKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := githubtest.NewBroker(fake, appID, appKey)
	t.Cleanup(b.Close)
	b.Register(client, githubtest.BrokerClient{Key: &daemonKey.PublicKey, Repos: repos, Installations: installations})
	return b, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(daemonKey)}))
}

// brokerLab is a GitHub fake with App 42, a broker holding its key that
// serves daemon team-a on repo (installation 77), and a client signed in
// through the broker with team-a's own key.
func brokerLab(t *testing.T, repos []string, installations []int64) (*githubtest.Server, *githubtest.Broker, *gh.Client, string) {
	t.Helper()
	fake := githubtest.New(repo)
	t.Cleanup(fake.Close)
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddApp("42", &appKey.PublicKey)
	broker, daemonPEM := brokerFor(t, fake, "42", appKey, "team-a", repos, installations)
	cfg, err := gh.ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"team-a","app_api_url":"` + broker.URL + `/","git":"bot"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := gh.NewClient(cfg, func(context.Context, string) (string, error) { return daemonPEM, nil }, gh.ClientOptions{Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	return fake, broker, client, daemonPEM
}

// TestBrokeredApp: with app_api_url the calls that authenticate as the
// App go to the broker, signed with the daemon's own key, and everything
// else goes to GitHub with the installation tokens the broker minted:
// reads and writes succeed, a token is replaced before it expires so no
// call is refused, the App's installation permissions and bot are known,
// and the daemon's key alone gets nothing from GitHub (#742).
func TestBrokeredApp(t *testing.T) {
	fake, broker, client, daemonPEM := brokerLab(t, []string{repo}, []int64{77})
	ctx := context.Background()
	fake.SetTokenTTL(10 * time.Minute)
	fake.ExpireInstallationTokens()
	n := fake.OpenIssue("Hello", "", alice)
	if _, err := client.Issue(ctx, repo, n); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Comment(ctx, repo, n, "hi"); err != nil {
		t.Fatal(err)
	}
	// Six minutes on, four are left: the next call mints a new token
	// rather than using one that would be refused in four.
	fake.Advance(6 * time.Minute)
	if _, err := client.Issue(ctx, repo, n); err != nil {
		t.Fatal(err)
	}
	fake.Advance(6 * time.Minute)
	if _, err := client.Comment(ctx, repo, n, "again"); err != nil {
		t.Fatal(err)
	}
	if got := fake.InstallationTokensIssued(); got != 3 {
		t.Fatalf("%d installation tokens minted over 12 minutes of 10-minute tokens, want 3", got)
	}
	if u := fake.Unauthorized(); len(u) != 0 {
		t.Fatalf("GitHub refused %v", u)
	}
	if r := broker.Refused(); len(r) != 0 {
		t.Fatalf("broker refused %v", r)
	}
	for _, r := range fake.Requests() {
		if strings.HasPrefix(r, "GET /app") || strings.Contains(r, "/installation") && !strings.HasPrefix(r, "DELETE /installation/token") {
			// Only the broker, with the App's own key, makes App calls.
			if !slices.Contains(broker.Requests(), strings.SplitN(r, "?", 2)[0]) {
				t.Fatalf("the daemon made App call %q to GitHub itself", r)
			}
		}
	}
	want := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write"}
	fake.SetInstallationPermissions(want)
	perms, known, err := client.ReaderPermissions(ctx, repo)
	if err != nil || !known || !maps.Equal(perms, want) {
		t.Fatalf("installation permissions %v (known %v, %v), want %v", perms, known, err, want)
	}
	a, err := client.Verify(ctx, repo)
	if err != nil || !a.App || a.Known {
		t.Fatalf("verify: %+v %v", a, err)
	}
	_, login, _ := githubtest.AppBot("42")
	if bot, err := client.WriterBot(ctx, repo); err != nil || bot != login {
		t.Fatalf("writer bot %q %v, want %q", bot, err, login)
	}
	if !strings.Contains(client.Writer(), "via broker "+broker.URL) {
		t.Fatalf("writer %q doesn't name the broker", client.Writer())
	}

	// Negative control: the same key and ID sent to GitHub, without the
	// broker, are refused.
	direct, err := gh.ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := gh.NewClient(direct, func(context.Context, string) (string, error) { return daemonPEM, nil }, gh.ClientOptions{Now: fake.Now})
	if _, err := c.Issue(ctx, repo, n); !gh.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("the daemon's key straight to GitHub: %v, want 401", err)
	}
}

// TestBrokeredGitToken: a git token through the broker is limited to the
// one repository and the granted operations' permissions, as from an App
// whose key is local, and revoking it at GitHub works as before.
func TestBrokeredGitToken(t *testing.T) {
	fake, broker, client, _ := brokerLab(t, []string{repo}, []int64{77})
	ctx := context.Background()
	fake.SetInstallationPermissions(map[string]string{"contents": "write", "pull_requests": "write", "issues": "write"})
	tok, _, err := client.GitToken(ctx, repo, []gh.Permission{{Name: "pull_requests", Level: "write"}})
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := fake.Scope(tok)
	if !ok || !slices.Equal(scope.Repositories, []string{"widgets"}) || !maps.Equal(scope.Permissions, map[string]string{"contents": "write", "pull_requests": "write"}) {
		t.Fatalf("git token minted for %+v (valid %v)", scope, ok)
	}
	if err := client.RevokeToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Scope(tok); ok {
		t.Fatal("the git token still works once revoked")
	}
	if r := broker.Refused(); len(r) != 0 {
		t.Fatalf("broker refused %v", r)
	}
}

// TestBrokerRefusal: the broker refuses an installation or repository
// outside what the daemon may use, and the daemon reports it as
// repo_unreachable, as it does GitHub saying no.
func TestBrokerRefusal(t *testing.T) {
	// The repository is the daemon's, but its installation isn't.
	fake, broker, client, _ := brokerLab(t, []string{repo}, []int64{78})
	n := fake.OpenIssue("Hello", "", alice)
	_, err := client.Issue(context.Background(), repo, n)
	if !gh.IsStatus(err, http.StatusNotFound) || gh.CallCode(err) != "hook_permission_denied" {
		t.Fatalf("installation refused: %v (%s)", err, gh.CallCode(err))
	}
	if r := broker.Refused(); len(r) != 1 || r[0] != "POST /app/installations/77/access_tokens: installation" {
		t.Fatalf("broker refused %v", r)
	}
	if fake.InstallationTokensIssued() != 0 {
		t.Fatal("a token was minted for an installation the daemon may not use")
	}
}

// TestExtensionDoctorBrokeredApp: the connection test of a brokered App
// lists the App installation's permissions under the broker's name, and
// reports repo_unreachable for a repository the broker won't serve.
func TestExtensionDoctorBrokeredApp(t *testing.T) {
	fake := githubtest.New(repo)
	t.Cleanup(fake.Close)
	fake.SetPermissions(map[string]bool{"admin": false, "push": false, "pull": false})
	fake.RequireRulesetChecks("main", "ci")
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddApp("42", &appKey.PublicKey)
	broker, daemonPEM := brokerFor(t, fake, "42", appKey, "team-a", []string{repo}, []int64{77})
	settings, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	view := gh.View{Entries: []gh.ViewEntry{{Name: "github/eng/main", Scope: "eng", Connection: "owner", Settings: settings}}}
	x := gh.NewExtension(func(context.Context) gh.View { return view })
	if err := x.Start(context.Background(), inproc.Host{Now: fake.Now, Secrets: func(_ context.Context, _ domain.ID, _ string, names []string) (map[string]string, error) {
		out := map[string]string{}
		for _, n := range names {
			out[n] = daemonPEM
		}
		return out, nil
	}}); err != nil {
		t.Fatal(err)
	}
	owner := protocol.ConnectionEntry{ID: "owner", Type: gh.Type, Config: json.RawMessage(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"team-a","app_api_url":"` + broker.URL + `"}`), Secrets: []string{gh.SecretAppPrivateKey}}
	x.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: gh.Type, Name: "main", Connection: &owner}}})
	_, deep := doctorChecks(t, x)

	// Refused by the broker before anything is cached, then served.
	broker.Allow("team-a", nil, []int64{77})
	if c := deep(context.Background()); c.Status != inproc.CheckFail || c.Code != "repo_unreachable" {
		t.Fatalf("broker refusing the repository: %+v", c)
	}
	broker.Allow("team-a", []string{repo}, []int64{77})
	fake.SetInstallationPermissions(map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "checks": "write", "statuses": "write", "metadata": "read"})
	c := deep(context.Background())
	items := strings.Join(c.Items, "\n")
	if c.Status != inproc.CheckOK || !strings.Contains(items, "App team-a via broker "+broker.URL+" holds Contents write, Issues write, Pull requests write") || strings.Contains(items, "pull=") {
		t.Fatalf("brokered App: %+v", c)
	}

}
