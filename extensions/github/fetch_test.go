package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// A link is read into the repository it is under, so the tool can check
// the Work may read that repository before anything is sent signed in.
func TestParseFetchLink(t *testing.T) {
	cfg, _ := ParseConnection([]byte(`{"api_url":"https://api.github.com"}`))
	c, err := NewClient(cfg, secrets(map[string]string{SecretToken: "t"}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		link, repo, url string
		signed          bool
		err             error
	}{
		{link: "https://github.com/octo/widgets/blob/main/docs/a.md", repo: "octo/widgets", url: "https://raw.githubusercontent.com/octo/widgets/main/docs/a.md", signed: true},
		{link: "https://github.com/octo/widgets/issues/1", repo: "octo/widgets", signed: true},
		{link: "https://raw.githubusercontent.com/octo/secret/main/.env", repo: "octo/secret", signed: true},
		{link: "https://api.github.com/repos/octo/secret/contents/.env", repo: "octo/secret", signed: true},
		{link: "https://github.com/user-attachments/assets/1234", signed: true},
		{link: "https://private-user-images.githubusercontent.com/1/x.png?jwt=abc"},
		{link: "https://user-images.githubusercontent.com/1/x.png"},
		{link: "https://camo.githubusercontent.com/abc/def"},
		{link: "https://api.github.com/user", err: ErrNoRepoLink},
		{link: "https://api.github.com/orgs/octo/repos", err: ErrNoRepoLink},
		{link: "https://github.com/octo", err: ErrNoRepoLink},
		{link: "https://github.com/octo/widgets/../secret/blob/main/.env", err: ErrNoRepoLink},
		{link: "https://api.github.com/repos/octo/widgets/../../repos/octo/secret/contents/.env", err: ErrNoRepoLink},
		{link: "https://api.github.com/repos/octo%2Fsecret/x", err: ErrNoRepoLink},
		{link: "https://objects.githubusercontent.com/x", err: ErrForeignLink},
		{link: "https://example.com/octo/widgets/x", err: ErrForeignLink},
		{link: "https://user@github.com/octo/widgets/x", err: ErrForeignLink},
		{link: "file:///etc/passwd", err: ErrForeignLink},
	} {
		got, err := c.ParseFetchLink(tc.link)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: err %v, want %v", tc.link, err, tc.err)
			}
			continue
		}
		want := tc.url
		if want == "" {
			want = tc.link
		}
		if err != nil || got.Repo != tc.repo || got.Signed != tc.signed || got.URL.String() != want {
			t.Errorf("%s: %+v %v, want repo %q signed %v url %s", tc.link, got, err, tc.repo, tc.signed, want)
		}
	}
	// On Enterprise the API is under /api/v3 on the web host.
	cfg, _ = ParseConnection([]byte(`{"api_url":"https://ghe.example/api/v3"}`))
	ghe, _ := NewClient(cfg, secrets(map[string]string{SecretToken: "t"}), ClientOptions{})
	if l, err := ghe.ParseFetchLink("https://ghe.example/api/v3/repos/octo/secret/contents/x"); err != nil || l.Repo != "octo/secret" {
		t.Fatalf("enterprise API link: %+v %v", l, err)
	}
	if l, err := ghe.ParseFetchLink("https://ghe.example/octo/widgets/files/x"); err != nil || l.Repo != "octo/widgets" {
		t.Fatalf("enterprise web link: %+v %v", l, err)
	}
	if _, err := ghe.ParseFetchLink("https://camo.githubusercontent.com/abc"); !errors.Is(err, ErrForeignLink) {
		t.Fatalf("camo on Enterprise: %v", err)
	}
}

// A sign-in App fetches with a token minted read-only for the one
// repository the link is under and revoked afterwards, so even a link
// that got past the tool's check could not reach another repository the
// installation covers.
func TestFetchMintsAReadOnlyTokenForOneRepository(t *testing.T) {
	fake := githubtest.New("octo/widgets")
	defer fake.Close()
	key, pemText := rsaKey(t)
	fake.AddApp("42", &key.PublicKey)
	fake.ServeFile("shot.png", []byte("png"), "")
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"42"}`))
	c, err := NewClient(cfg, secrets(map[string]string{SecretAppPrivateKey: pemText}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	link, _ := c.ParseFetchLink(fake.URL + "/octo/widgets/files/shot.png")
	if data, _, err := c.Fetch(ctx, []string{"octo/widgets"}, link); err != nil || string(data) != "png" {
		t.Fatalf("fetch: %q %v", data, err)
	}
	reads := fake.FileReads()
	if len(reads) != 1 {
		t.Fatalf("reads: %+v", reads)
	}
	if _, valid := fake.Scope(reads[0].Token); valid {
		t.Fatal("the fetch token was not revoked")
	}
	// Mint again to see its scope while it is live: the same call Fetch makes.
	tok, _, err := c.who.Load().read.(*appToken).scoped(ctx, "octo/widgets", readPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := fake.Scope(tok)
	if !slices.Equal(scope.Repositories, []string{"widgets"}) || !maps.Equal(scope.Permissions, readPermissions) {
		t.Fatalf("fetch token scope: %+v", scope)
	}
	for _, p := range scope.Permissions {
		if p != "read" {
			t.Fatalf("fetch token can write: %+v", scope)
		}
	}
	// Negative control: the same kind of token can't read another
	// repository, which a whole-installation token (the old behaviour) can.
	other, _ := c.ParseFetchLink(fake.URL + "/octo/secret/files/shot.png")
	if _, _, err := c.Fetch(ctx, []string{"octo/widgets"}, other); !IsStatus(err, http.StatusNotFound) {
		t.Fatalf("a token for widgets read octo/secret: %v", err)
	}
	whole, err := c.who.Load().read.token(ctx, "octo/widgets")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, fake.URL+"/octo/secret/files/shot.png", nil)
	req.Header.Set("Authorization", "token "+whole)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("control: the installation token was refused too (%d), so the check above proves nothing", res.StatusCode)
	}
}

// A comment attachment may sit on the tracker or the code repository,
// so its token covers both, still read-only and nothing else.
func TestAttachmentTokenCoversTrackerAndCodeRepository(t *testing.T) {
	fake := githubtest.New("octo/widgets")
	defer fake.Close()
	key, pemText := rsaKey(t)
	fake.AddApp("42", &key.PublicKey)
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"42"}`))
	c, _ := NewClient(cfg, secrets(map[string]string{SecretAppPrivateKey: pemText}), ClientOptions{})
	tok, done, err := c.fetchToken(context.Background(), []string{"octo/widgets", "octo/docs"}, readPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scope, valid := fake.Scope(tok)
	if !valid || !slices.Equal(scope.Repositories, []string{"widgets", "docs"}) || !maps.Equal(scope.Permissions, readPermissions) {
		t.Fatalf("attachment token scope: %+v %v", scope, valid)
	}
	done()
	if _, valid := fake.Scope(tok); valid {
		t.Fatal("the attachment token was not revoked")
	}
	if _, _, err := c.fetchToken(context.Background(), []string{"octo/widgets", "other/docs"}, readPermissions); err == nil {
		t.Fatal("a token was minted across owners")
	}
}

type recordingTransport struct {
	mu   sync.Mutex
	seen map[string]string // host → Authorization
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen[req.URL.Host] = req.Header.Get("Authorization")
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("png")), Request: req}, nil
}

// camo (GitHub's proxy for images hosted elsewhere) and the comment-image
// hosts are fetched without the sign-in token; a repository link on the
// web host still gets it.
func TestFetchDoesNotSignImageProxyLinks(t *testing.T) {
	rt := &recordingTransport{seen: map[string]string{}}
	cfg, _ := ParseConnection([]byte(`{"api_url":"https://api.github.com"}`))
	c, err := NewClient(cfg, secrets(map[string]string{SecretToken: "pat"}), ClientOptions{HTTP: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{
		"https://camo.githubusercontent.com/abc/def",
		"https://private-user-images.githubusercontent.com/1/x.png?jwt=abc",
		"https://user-images.githubusercontent.com/1/x.png",
		"https://github.com/user-attachments/assets/1234",
	} {
		l, err := c.ParseFetchLink(link)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Fetch(context.Background(), []string{"octo/widgets"}, l); err != nil {
			t.Fatal(err)
		}
	}
	for host, auth := range rt.seen {
		if want := host == "github.com"; (auth != "") != want {
			t.Errorf("%s got Authorization %q", host, auth)
		}
	}
	if len(rt.seen) != 4 {
		t.Fatalf("hosts fetched: %v", rt.seen)
	}
}

// A job's log is read with a token for that one repository holding Actions
// read and nothing else, revoked afterwards.
func TestJobLogMintsAnActionsReadTokenForOneRepository(t *testing.T) {
	fake := githubtest.New("octo/widgets")
	defer fake.Close()
	key, pemText := rsaKey(t)
	fake.AddApp("42", &key.PublicKey)
	fake.SetCheck("h1", "test", "completed", "failure")
	jobs := func(c *Client) []ActionsJob {
		t.Helper()
		got, err := c.HeadJobs(context.Background(), "octo/widgets", "h1")
		if err != nil || len(got) != 1 {
			t.Fatalf("jobs: %+v %v", got, err)
		}
		return got
	}
	fake.SetJobLog("h1", "test", []byte("the log"))
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"42"}`))
	c, err := NewClient(cfg, secrets(map[string]string{SecretAppPrivateKey: pemText}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	log, size, err := c.JobLog(context.Background(), "octo/widgets", jobs(c)[0].ID)
	if err != nil || string(log) != "the log" || size != 7 {
		t.Fatalf("log: %q %d %v", log, size, err)
	}
	reads := fake.FileReads()
	if len(reads) == 0 {
		t.Fatal("no log read")
	}
	if _, valid := fake.Scope(reads[0].Token); valid {
		t.Fatal("the log token was not revoked")
	}
	tok, _, err := c.who.Load().read.(*appToken).scoped(context.Background(), "octo/widgets", actionsReadPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := fake.Scope(tok)
	if !slices.Equal(scope.Repositories, []string{"widgets"}) || !maps.Equal(scope.Permissions, map[string]string{"actions": "read"}) {
		t.Fatalf("log token scope: %+v", scope)
	}
	// Negative control: a token with the attachment scope has no Actions
	// read, so the fake refuses it as GitHub would.
	other, _, err := c.who.Load().read.(*appToken).scoped(context.Background(), "octo/widgets", readPermissions)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, fake.URL+"/repos/octo/widgets/actions/jobs/"+strconv.FormatInt(jobs(c)[0].ID, 10)+"/logs", nil)
	req.Header.Set("Authorization", "token "+other)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a token without Actions read got %d", res.StatusCode)
	}
}

func TestLogExcerpt(t *testing.T) {
	stamp := "2026-11-21T10:00:00.1234567Z "
	if got, cut := logExcerpt([]byte("\xef\xbb\xbf"+stamp+"one\r\n"+stamp+"two\n"), 100); got != "one\ntwo" || cut {
		t.Fatalf("a short log: %q %v", got, cut)
	}
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "%sline %d\n", stamp, i)
		if i == 2000 {
			fmt.Fprintf(&b, "%s--- FAIL: TestThing\n%s##[error]Process completed with exit code 1.\n", stamp, stamp)
		}
	}
	got, cut := logExcerpt([]byte(b.String()), 8<<10)
	if !cut || !strings.Contains(got, "--- FAIL: TestThing") || !strings.Contains(got, "##[error]") || !strings.Contains(got, "lines left out") || strings.Contains(got, "line 4999") || strings.Contains(got, stamp) {
		t.Fatalf("around the error: %v\n%s", cut, got)
	}
	// With no error marker the end is kept.
	got, cut = logExcerpt([]byte(strings.ReplaceAll(b.String(), "##[error]", "")), 8<<10)
	if !cut || !strings.Contains(got, "line 4999") || strings.Contains(got, "line 10\n") {
		t.Fatalf("the end: %v\n%s", cut, got)
	}
	// The limit holds, keeping the end, and never splits a character.
	got, cut = logExcerpt([]byte(strings.Repeat("é", 4000)+"\n##[error]end"), 1000)
	if !cut || len(got) > 1000 || !strings.HasSuffix(got, "##[error]end") || !utf8.ValidString(got) {
		t.Fatalf("the limit: %d %v", len(got), cut)
	}
}

func TestReadTailKeepsTheEnd(t *testing.T) {
	data := strings.Repeat("a", 300<<10) + "end"
	got, size, err := readTail(strings.NewReader(data), 100<<10)
	if err != nil || size != int64(len(data)) || len(got) != 100<<10 || !strings.HasSuffix(string(got), "end") {
		t.Fatalf("tail: %d %d %v", len(got), size, err)
	}
}
