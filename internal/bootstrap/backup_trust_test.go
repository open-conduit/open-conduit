package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
)

func trustConfig(t *testing.T, signing, trust string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"version":1,"settings":{"backup":{"signing_key":%q,"trust_keys":[%q]}},"connections":[],"scopes":[{"id":"org","name":"org"}]}`, signing, trust)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseFile([]byte(body)); err != nil {
		t.Fatal(err)
	}
	return path
}

// A bundle is signed with the config's key and checked against its trust
// keys, through the commands an operator runs.
func TestBundleTrustThroughTheCommands(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	private, public, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPublic, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OC_TEST_BACKUP_KEY", private)
	trusted := trustConfig(t, "env:OC_TEST_BACKUP_KEY", public)
	rotated := trustConfig(t, "env:OC_TEST_BACKUP_KEY", otherPublic)
	bundle := filepath.Join(t.TempDir(), "signed")
	if _, err := BackupState(ctx, f.state, bundle, "test", cli.BackupOptions{ConfigPath: trusted}); err != nil {
		t.Fatal(err)
	}
	info, err := VerifyBundle(ctx, bundle, cli.RestoreOptions{ConfigPath: trusted})
	if err != nil || info.Authenticity != backup.Verified || info.SigningKey == "" {
		t.Fatalf("verify = %+v, %v", info, err)
	}
	// The flag trusts a key on a host that has lost its config.
	if info, err = VerifyBundle(ctx, bundle, cli.RestoreOptions{TrustKeys: []string{public}}); err != nil || info.Authenticity != backup.Verified {
		t.Fatalf("verify with --trust-key = %+v, %v", info, err)
	}
	if _, err = VerifyBundle(ctx, bundle, cli.RestoreOptions{ConfigPath: rotated}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("verify against another key: %v", err)
	}
	if _, err = RestoreState(ctx, bundle, filepath.Join(t.TempDir(), "refused"), cli.RestoreOptions{ConfigPath: rotated}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("restore against another key: %v", err)
	}

	// An in-place restore that restores nothing doesn't need the key that
	// signed the bundle any more, so a rotation can't stop a container
	// restarting with --restore; one that would restore still does.
	if _, err = RestoreInPlace(ctx, bundle, f.state, cli.RestoreOptions{ConfigPath: trusted, Once: true}); err != nil {
		t.Fatal(err)
	}
	again, err := RestoreInPlace(ctx, bundle, f.state, cli.RestoreOptions{ConfigPath: rotated, Once: true})
	if err != nil || again.InPlace == nil || !again.InPlace.AlreadyRestored {
		t.Fatalf("restart after rotation = %+v, %v", again.InPlace, err)
	}
	if _, err = RestoreInPlace(ctx, bundle, f.state, cli.RestoreOptions{ConfigPath: rotated}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("an explicit restore against another key: %v", err)
	}

	// A stripped signature is refused until the operator accepts it.
	if err := os.Remove(filepath.Join(bundle, "manifest.sig")); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBundle(ctx, bundle, cli.RestoreOptions{ConfigPath: trusted}); !errors.Is(err, backup.ErrNotAuthentic) {
		t.Fatalf("verify of a stripped bundle: %v", err)
	}
	if info, err = VerifyBundle(ctx, bundle, cli.RestoreOptions{ConfigPath: trusted, AcceptUnsigned: true}); err != nil || info.Authenticity != backup.Unsigned {
		t.Fatalf("verify accepting unsigned = %+v, %v", info, err)
	}
}

// A config that can't be read must not turn the signature check off.
func TestDamagedConfigKeepsTheCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreTrust(cli.RestoreOptions{ConfigPath: path}); err == nil {
		t.Fatal("a damaged config gave a policy")
	}
	missing, err := restoreTrust(cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "absent.json")})
	if err != nil || len(missing.Keys) != 0 {
		t.Fatalf("an absent config = %+v, %v", missing, err)
	}
}

// A signing key the daemon can't get doesn't cost the backup: it publishes
// unsigned and says so until signing works again.
func TestScheduledBackupSurvivesAMissingKey(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	d.reportedAt = map[string]time.Time{}
	d.resolver = &config.Resolver{LookupEnv: func(string) (string, bool) { return "", false }}
	if key := d.signingKeyFrom(ctx, "env:OC_TEST_ABSENT"); key != nil {
		t.Fatal("got a key from nothing")
	}
	if got := d.reportedCode("backup_signing", ""); got != "signing_key_unavailable" {
		t.Fatalf("diagnostic = %q", got)
	}
	if err := d.backupNow(ctx, start); err != nil {
		t.Fatal(err)
	}
	private, _, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	d.resolver = &config.Resolver{LookupEnv: func(string) (string, bool) { return private, true }}
	if key := d.signingKeyFrom(ctx, "env:OC_TEST_PRESENT"); key == nil {
		t.Fatal("no key")
	}
	if got := d.reportedCode("backup_signing", ""); got != "" {
		t.Fatalf("diagnostic not cleared: %q", got)
	}
}
