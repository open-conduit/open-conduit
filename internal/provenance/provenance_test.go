package provenance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

// testdata holds a real release's checksums.txt and its bundle (v6.10.0),
// and Sigstore's public-good trusted root as TUF served it then, so the
// check runs on real material without the network.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var releaseWorkflow = Identity{
	Subject: "https://github.com/open-conduit/open-conduit/.github/workflows/release.yml@refs/heads/main",
	Issuer:  GitHubActions,
}

func publicGood(t *testing.T) root.TrustedMaterial {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(testdata(t, "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestVerifyARealBundle(t *testing.T) {
	if err := Verify(publicGood(t), testdata(t, "checksums.txt.sigstore.json"), testdata(t, "checksums.txt"), releaseWorkflow); err != nil {
		t.Fatal(err)
	}
}

// Each case changes one thing the positive case above relies on, and must
// be refused.
func TestVerifyRefuses(t *testing.T) {
	bundle, sums := testdata(t, "checksums.txt.sigstore.json"), testdata(t, "checksums.txt")
	other, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		trusted  root.TrustedMaterial
		bundle   []byte
		artifact []byte
		id       Identity
	}{
		"a changed artifact":   {publicGood(t), bundle, append([]byte("0000  extra.tar.gz\n"), sums...), releaseWorkflow},
		"another subject":      {publicGood(t), bundle, sums, Identity{strings.Replace(releaseWorkflow.Subject, "refs/heads/main", "refs/heads/dev", 1), GitHubActions}},
		"another repository":   {publicGood(t), bundle, sums, Identity{strings.Replace(releaseWorkflow.Subject, "open-conduit/open-conduit", "someone/open-conduit", 1), GitHubActions}},
		"another issuer":       {publicGood(t), bundle, sums, Identity{releaseWorkflow.Subject, "https://accounts.google.com"}},
		"no identity":          {publicGood(t), bundle, sums, Identity{}},
		"another trusted root": {other, bundle, sums, releaseWorkflow},
		"no trusted root":      {nil, bundle, sums, releaseWorkflow},
		"a bundle that isn't":  {publicGood(t), []byte(`{"mediaType": "nope"}`), sums, releaseWorkflow},
		"an empty bundle":      {publicGood(t), nil, sums, releaseWorkflow},
		"a truncated bundle":   {publicGood(t), bundle[:len(bundle)/2], sums, releaseWorkflow},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(c.trusted, c.bundle, c.artifact, c.id); err == nil {
				t.Fatal("verified")
			}
		})
	}
}

// A signature from a CA the trusted root does trust, but by another
// identity, is refused; by the expected one, accepted.
func TestVerifyMatchesTheIdentity(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("checksums")
	entity, err := vs.Sign(releaseWorkflow.Subject, releaseWorkflow.Issuer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyEntity(vs, entity, artifact, releaseWorkflow); err != nil {
		t.Fatal(err)
	}
	impostor, err := vs.Sign("https://github.com/someone/fork/.github/workflows/release.yml@refs/heads/main", releaseWorkflow.Issuer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyEntity(vs, impostor, artifact, releaseWorkflow); err == nil {
		t.Fatal("verified another identity's signature")
	}
}

// The TUF fetcher reports a status as TUF expects and stops at its limit.
func TestFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	f := &httpFetcher{ctx: context.Background(), client: srv.Client()}
	if b, err := f.DownloadFile(srv.URL+"/ok", 10, 0); err != nil || string(b) != "0123456789" {
		t.Fatalf("ok: %q %v", b, err)
	}
	if _, err := f.DownloadFile(srv.URL+"/ok", 9, 0); err == nil {
		t.Fatal("over the limit")
	}
	if _, err := f.DownloadFile(srv.URL+"/missing", 10, 0); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&httpFetcher{ctx: ctx, client: srv.Client()}).DownloadFile(srv.URL+"/ok", 10, 0); err == nil {
		t.Fatal("fetched on a cancelled context")
	}
}
