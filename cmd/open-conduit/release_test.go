package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
