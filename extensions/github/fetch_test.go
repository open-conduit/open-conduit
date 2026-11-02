package github

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

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
	if data, _, err := c.Fetch(ctx, "octo/widgets", link); err != nil || string(data) != "png" {
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
	if _, _, err := c.Fetch(ctx, "octo/widgets", other); !IsStatus(err, http.StatusNotFound) {
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
		if _, _, err := c.Fetch(context.Background(), "octo/widgets", l); err != nil {
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
