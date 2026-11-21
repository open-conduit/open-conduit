package backup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const signingBackend = sqlite.Name

var signingBackends = Backends(storage.LookupBackend)

func signingTarget() ArtifactTarget { return ArtifactTarget{Backend: artifacts.Backend} }

func signingFixture(t *testing.T, root string) (storage.Database, storage.ArtifactStore, time.Time) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	files, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return db, files, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
}

func signingKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	private, public, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// signedBundle writes a manifest and its signature, which is all the check
// reads.
func signedBundle(t *testing.T, key ed25519.PrivateKey) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	manifest := []byte("{\"Version\":1}\n")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		if err := writeSignature(dir, manifest, key); err != nil {
			t.Fatal(err)
		}
	}
	return dir, manifest
}

func TestTrustPolicy(t *testing.T) {
	key, pub := signingKey(t)
	_, other := signingKey(t)
	signed, manifest := signedBundle(t, key)
	unsigned, plain := signedBundle(t, nil)
	cases := []struct {
		name   string
		bundle string
		bytes  []byte
		trust  Trust
		status string // "" is refused
	}{
		{"a trusted signature verifies", signed, manifest, Trust{Keys: []ed25519.PublicKey{other, pub}}, Verified},
		{"an untrusted key is refused", signed, manifest, Trust{Keys: []ed25519.PublicKey{other}}, ""},
		{"accepting unsigned doesn't accept an untrusted key", signed, manifest, Trust{Keys: []ed25519.PublicKey{other}, AcceptUnsigned: true}, ""},
		{"a changed manifest is refused", signed, append([]byte(" "), manifest...), Trust{Keys: []ed25519.PublicKey{pub}}, ""},
		{"accepting unsigned doesn't accept a wrong signature", signed, append([]byte(" "), manifest...), Trust{Keys: []ed25519.PublicKey{pub}, AcceptUnsigned: true}, ""},
		{"a stripped signature is refused", unsigned, plain, Trust{Keys: []ed25519.PublicKey{pub}}, ""},
		{"unsigned passes when explicitly accepted", unsigned, plain, Trust{Keys: []ed25519.PublicKey{pub}, AcceptUnsigned: true}, Unsigned},
		{"unsigned passes with no trust keys", unsigned, plain, Trust{}, Unsigned},
		{"a signature with no trust keys proves nothing", signed, manifest, Trust{}, Unverified},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.trust.check(c.bundle, c.bytes)
			if c.status == "" {
				if !errors.Is(err, ErrNotAuthentic) {
					t.Fatalf("got %+v, %v; want refusal", got, err)
				}
				return
			}
			if err != nil || got.Status != c.status {
				t.Fatalf("got %+v, %v; want %s", got, err, c.status)
			}
		})
	}
}

func TestSignatureFileIsStrict(t *testing.T) {
	key, pub := signingKey(t)
	for name, mutate := range map[string]func(*signatureFile){
		"algorithm": func(s *signatureFile) { s.Algorithm = "none" },
		"version":   func(s *signatureFile) { s.Version = 2 },
		"length":    func(s *signatureFile) { s.Signature = s.Signature[:10] },
	} {
		t.Run(name, func(t *testing.T) {
			dir, manifest := signedBundle(t, key)
			path := filepath.Join(dir, signatureName)
			data, _ := os.ReadFile(path)
			var s signatureFile
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatal(err)
			}
			mutate(&s)
			data, _ = json.Marshal(s)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := (Trust{Keys: []ed25519.PublicKey{pub}}).check(dir, manifest); !errors.Is(err, ErrNotAuthentic) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestRestoreRefusesAForgedBundle re-signs a bundle with an attacker's key:
// every digest still matches, so only the signature check can catch it. The
// restore of the genuine bundle is the negative control.
func TestRestoreRefusesAForgedBundle(t *testing.T) {
	ctx := context.Background()
	key, pub := signingKey(t)
	attacker, _ := signingKey(t)
	root := t.TempDir()
	db, files, now := signingFixture(t, root)
	bundle := filepath.Join(root, "bundle")
	if err := Create(ctx, bundle, key, signingBackend, db, files, "test-build", kvRetention()); err != nil {
		t.Fatal(err)
	}
	trust := Trust{Keys: []ed25519.PublicKey{pub}}
	target := signingTarget()
	if err := Restore(ctx, bundle, filepath.Join(root, "good"), trust, now, nil, signingBackends, target, nil); err != nil {
		t.Fatalf("a genuine bundle: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, signatureName)); err != nil {
		t.Fatal(err)
	}
	if err := writeSignature(bundle, raw, attacker); err != nil {
		t.Fatal(err)
	}
	err = Restore(ctx, bundle, filepath.Join(root, "forged"), trust, now, nil, signingBackends, target, nil)
	if !errors.Is(err, ErrNotAuthentic) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "forged")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused bundle published a directory: %v", err)
	}
}
