package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

type holderFixture struct {
	t     *testing.T
	store storage.Store
	now   func() time.Time
	dir   string
}

func newHolderFixture(t *testing.T) holderFixture {
	t.Helper()
	at := time.Unix(1_800_000_000, 0).UTC()
	return holderFixture{t: t, store: reference.New(), now: func() time.Time { return at }, dir: t.TempDir()}
}

func (f holderFixture) path(host string) string {
	return filepath.Join(f.dir, host, "private", "operator.token")
}

func (f holderFixture) read(host string) management.TokenFile {
	f.t.Helper()
	file, err := management.ReadTokenFile(f.path(host))
	if err != nil {
		f.t.Fatal(err)
	}
	return file
}

func (f holderFixture) ensure(host string) (string, error) {
	return management.EnsureHolderOperator(context.Background(), f.store, f.now, f.path(host), host)
}

// issued asserts that ensuring host's credential mints a new one, which is
// how a file whose credential no longer works shows up.
func (f holderFixture) issued(host string) {
	f.t.Helper()
	if id, err := f.ensure(host); err != nil || id == "" {
		f.t.Fatalf("host %s: issued %q, %v", host, id, err)
	}
}

// kept asserts that host's file already holds a working credential.
func (f holderFixture) kept(host string) {
	f.t.Helper()
	if id, err := f.ensure(host); err != nil || id != "" {
		f.t.Fatalf("host %s: issued %q, %v", host, id, err)
	}
}

func TestHolderOperatorIssuedForASuccessor(t *testing.T) {
	f := newHolderFixture(t)
	if err := management.BootstrapOperator(context.Background(), f.store, f.now, f.path("a")); err != nil {
		t.Fatal(err)
	}
	f.kept("a")
	f.issued("b")
	f.kept("b")
	// A third host retires b's credential and leaves the first one working.
	f.issued("c")
	f.kept("a")
	// b's file now holds a retired credential, so b gets a new one, which
	// retires c's in turn.
	before := f.read("b")
	f.issued("b")
	if f.read("b") == before {
		t.Fatal("the returning host kept its retired credential")
	}
	f.kept("b")
	f.issued("c")
}

// An operator's revocation stands: restarting the host must not mint it a new
// admin credential.
func TestHolderOperatorHonoursARevocation(t *testing.T) {
	f := newHolderFixture(t)
	ctx := context.Background()
	if err := management.BootstrapOperator(ctx, f.store, f.now, f.path("a")); err != nil {
		t.Fatal(err)
	}
	revoked := f.read("a")
	if err := management.RecoverOperator(ctx, f.store, f.now, f.path("b")); err != nil {
		t.Fatal(err)
	}
	if id, err := f.ensure("a"); !errors.Is(err, management.ErrCredentialExists) || id != "" {
		t.Fatalf("a revoked file: issued %q, %v", id, err)
	}
	if f.read("a") != revoked {
		t.Fatal("a revoked file was replaced")
	}
}

// auth recover also covers a credential a takeover had already retired. The
// control is the same sequence without the recover, where the host does get a
// new credential, so the refusal is the recover's doing.
func TestHolderOperatorRecoverOverridesATakeover(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		f := newHolderFixture(t)
		ctx := context.Background()
		if err := management.BootstrapOperator(ctx, f.store, f.now, f.path("first")); err != nil {
			t.Fatal(err)
		}
		f.issued("a")
		f.issued("b") // retires a's
		retired := f.read("a")
		if !recovered {
			f.issued("a")
			continue
		}
		if err := management.RecoverOperator(ctx, f.store, f.now, f.path("operator")); err != nil {
			t.Fatal(err)
		}
		if id, err := f.ensure("a"); !errors.Is(err, management.ErrCredentialExists) || id != "" {
			t.Fatalf("a host retired before the recover: issued %q, %v", id, err)
		}
		if f.read("a") != retired {
			t.Fatal("the retired file was replaced after the recover")
		}
	}
}

func TestHolderOperatorLeavesAnUnreadableFile(t *testing.T) {
	f := newHolderFixture(t)
	path := f.path("a")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := f.ensure("a"); !errors.Is(err, management.ErrCredentialExists) || id != "" {
		t.Fatalf("an unreadable file: issued %q, %v", id, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "not a token" {
		t.Fatalf("the file was replaced: %q", b)
	}
}
