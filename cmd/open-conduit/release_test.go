package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/provenance"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// The updater's signer is the identity a real release's checksums.txt is
// signed with: v6.10.0's bundle verifies with it, offline against a saved
// trusted root, and fails for a changed list or an unavailable root.
func TestReleaseVerifierVerifiesARealRelease(t *testing.T) {
	data := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "provenance", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tr, err := root.NewTrustedRootFromJSON(data("trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	verify := releaseVerifier(func(context.Context) (root.TrustedMaterial, error) { return tr, nil })
	bundle, sums := data("checksums.txt.sigstore.json"), data("checksums.txt")
	if err := verify(context.Background(), bundle, sums); err != nil {
		t.Fatal(err)
	}
	if err := verify(context.Background(), bundle, []byte(strings.Replace(string(sums), "linux", "linox", 1))); err == nil {
		t.Fatal("verified a changed checksums.txt")
	}
	offline := releaseVerifier(func(context.Context) (root.TrustedMaterial, error) { return nil, errors.New("offline") })
	if err := offline(context.Background(), bundle, sums); err == nil {
		t.Fatal("verified without a trusted root")
	}
}

// Install's signature check verifies a real Sigstore bundle as a
// publisher given by regular expression, returns the exact identity that
// signed, refuses another publisher, and fetches the trusted root once
// (again after a failure).
func TestPackageVerifierVerifiesARealSignature(t *testing.T) {
	data := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "provenance", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tr, err := root.NewTrustedRootFromJSON(data("trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	fetches, fail := 0, true
	verify := packageVerifier(func(context.Context) (root.TrustedMaterial, error) {
		fetches++
		if fail {
			fail = false
			return nil, errors.New("offline")
		}
		return tr, nil
	})
	bundle, artifact := data("checksums.txt.sigstore.json"), data("checksums.txt")
	publisher := extproc.Signer{SubjectRegexp: `https://github\.com/open-conduit/open-conduit/\.github/workflows/release\.yml@.*`, Issuer: provenance.GitHubActions}
	if _, err := verify(context.Background(), bundle, artifact, publisher); err == nil {
		t.Fatal("verified without a trusted root")
	}
	got, err := verify(context.Background(), bundle, artifact, publisher)
	if err != nil || got.Subject != upgrade.SignerSubject || got.Issuer != upgrade.SignerIssuer || got.SubjectRegexp != "" {
		t.Fatalf("signer %+v %v", got, err)
	}
	other := extproc.Signer{Subject: "https://github.com/acme/ext/.github/workflows/release.yml@refs/heads/main", Issuer: provenance.GitHubActions}
	if _, err := verify(context.Background(), bundle, artifact, other); err == nil {
		t.Fatal("verified as another publisher")
	}
	if fetches != 2 {
		t.Fatalf("fetched the trusted root %d times", fetches)
	}
}
