package backup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const signingBackend = sqlite.Name

var signingBackends = Backends(storage.LookupBackend)

func signingTarget() Target { return Target{Artifacts: artifacts.Backend} }

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

// swappingBackend replaces the bundle's database with a forged one at the
// last moment before the restore reads it, after the digest was checked.
type swappingBackend struct {
	storage.Backend
	bundleDB string
	forged   []byte
	t        *testing.T
	source   string
}

func (b *swappingBackend) Restore(ctx context.Context, snapshot, destination string, now time.Time) error {
	b.source = snapshot
	if err := os.WriteFile(b.bundleDB, b.forged, 0o600); err != nil {
		b.t.Fatal(err)
	}
	return b.Backend.Restore(ctx, snapshot, destination, now)
}

func scopeNamed(ctx context.Context, t *testing.T, db storage.Database, id domain.ID) {
	t.Helper()
	err := db.Transact(ctx, storage.Guard{Now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: id, Name: string(id)}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRestoreUsesTheBytesItChecked swaps the bundle's database for a forged,
// valid one after the digest check. The restore must hold the genuine state.
// Restoring from the bundle's own path, as it once did, fails this test.
func TestRestoreUsesTheBytesItChecked(t *testing.T) {
	ctx := context.Background()
	key, pub := signingKey(t)
	root := t.TempDir()
	db, files, now := signingFixture(t, root)
	scopeNamed(ctx, t, db, "genuine")
	bundle := filepath.Join(root, "bundle")
	if err := Create(ctx, bundle, key, signingBackend, db, files, "test-build", kvRetention()); err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	other, otherFiles, _ := signingFixture(t, otherRoot)
	scopeNamed(ctx, t, other, "forged")
	forgedBundle := filepath.Join(otherRoot, "bundle")
	if err := Create(ctx, forgedBundle, nil, signingBackend, other, otherFiles, "test-build", kvRetention()); err != nil {
		t.Fatal(err)
	}
	forged, err := os.ReadFile(filepath.Join(forgedBundle, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := storage.LookupBackend(signingBackend)
	if err != nil {
		t.Fatal(err)
	}
	swap := &swappingBackend{Backend: inner, bundleDB: filepath.Join(bundle, snapshotName), forged: forged, t: t}
	backends := func(string) (storage.Backend, error) { return swap, nil }
	destination := filepath.Join(root, "restored")
	if err := Restore(ctx, bundle, destination, Trust{Keys: []ed25519.PublicKey{pub}}, now, nil, backends, signingTarget(), nil); err != nil {
		t.Fatal(err)
	}
	if swap.source == swap.bundleDB {
		t.Fatal("the restore read the database from the bundle")
	}
	if _, err := os.Lstat(swap.source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the private copy was left behind: %v", err)
	}
	restored, err := sqlite.Open(ctx, filepath.Join(destination, snapshotName))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var ids []domain.ID
	err = restored.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		scopes, err := tx.ListScopes()
		ids = nil
		for _, s := range scopes {
			ids = append(ids, s.ID)
		}
		return err
	})
	if err != nil || len(ids) != 1 || ids[0] != "genuine" {
		t.Fatalf("restored scopes = %v, %v", ids, err)
	}
	// The swapped bundle now fails on its next read.
	err = Restore(ctx, bundle, filepath.Join(root, "after-swap"), Trust{Keys: []ed25519.PublicKey{pub}}, now, nil, signingBackends, signingTarget(), nil)
	if err == nil || !strings.Contains(err.Error(), "database digest mismatch") {
		t.Fatalf("err = %v", err)
	}
}
