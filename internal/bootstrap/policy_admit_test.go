package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func TestAdmitPolicy(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, b := &config.Policy{Digest: "aaaa", Path: "/a"}, &config.Policy{Digest: "bbbb", Path: "/b"}
	type step struct {
		policy *config.Policy
		accept string
		refuse bool
	}
	for name, steps := range map[string][]step{
		"the same policy serves":           {{policy: a}, {policy: a}},
		"no policy throughout serves":      {{}, {}},
		"adding a policy needs nothing":    {{}, {policy: a}},
		"dropping it is refused":           {{policy: a}, {refuse: true}, {policy: a}},
		"changing it is refused":           {{policy: a}, {policy: b, refuse: true}},
		"accepting none":                   {{policy: a}, {accept: "none"}, {}},
		"accepting the digest":             {{policy: a}, {policy: b, accept: "sha256:bbbb"}, {policy: b}},
		"accepting another digest refused": {{policy: a}, {policy: b, accept: "sha256:cccc", refuse: true}},
		"none doesn't accept a policy":     {{policy: a}, {policy: b, accept: "none", refuse: true}},
		"a stale accept stays refused":     {{policy: a}, {policy: b, accept: "sha256:bbbb"}, {accept: "sha256:bbbb", refuse: true}},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for i, s := range steps {
				err := admitPolicy(ctx, db, now, s.policy, s.accept, "owner", log)
				if s.refuse != errors.Is(err, ErrPolicyChanged) || (!s.refuse && err != nil) {
					t.Fatalf("step %d: %v", i, err)
				}
			}
		})
	}
}

// State that has run but holds no record may have run under any policy: a
// restored bundle from before the first record is the way to get here. New
// state, the control, starts with none and needs nothing.
func TestAdmitPolicyOfStateWithNoRecord(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func(t *testing.T, ran bool) *sqlite.Store {
		t.Helper()
		db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if ran {
			if err = db.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
				return tx.Put("core.config", "active", []byte(`{"Generation":1}`))
			}); err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	if err := admitPolicy(ctx, open(t, false), now, nil, "", "owner", log); err != nil {
		t.Fatalf("new state: %v", err)
	}
	if err := admitPolicy(ctx, open(t, true), now, nil, "", "owner", log); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("state that ran, with no record and no policy: %v", err)
	}
	if err := admitPolicy(ctx, open(t, true), now, nil, "none", "owner", log); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	// Any policy file is a claim about state whose last policy isn't known: it
	// could be looser than the one the state ran under.
	if err := admitPolicy(ctx, open(t, true), now, &config.Policy{Digest: "aaaa"}, "", "owner", log); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("state that ran, with no record and a policy: %v", err)
	}
	if err := admitPolicy(ctx, open(t, true), now, &config.Policy{Digest: "aaaa"}, "none", "owner", log); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("accepting a different policy: %v", err)
	}
	accepted := open(t, true)
	if err := admitPolicy(ctx, accepted, now, &config.Policy{Digest: "aaaa"}, "sha256:aaaa", "owner", log); err != nil {
		t.Fatalf("accepted by digest: %v", err)
	}
	var records []domain.AuditRecord
	if err := accepted.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		var err error
		records, err = tx.AuditRecords(0, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Detail["previous"] != "unknown" || records[0].Detail["current"] != "sha256:aaaa" {
		t.Fatalf("audit of unknown state: %+v", records)
	}
	// Once accepted, the record is what holds it, so no flag is needed again.
	if err := admitPolicy(ctx, accepted, now, &config.Policy{Digest: "aaaa"}, "", "owner", log); err != nil {
		t.Fatalf("after acceptance: %v", err)
	}
	// New state with a policy, the control, needs nothing.
	if err := admitPolicy(ctx, open(t, false), now, &config.Policy{Digest: "aaaa"}, "", "owner", log); err != nil {
		t.Fatalf("new state with a policy: %v", err)
	}
}

// A newer binary with the wrong policy must leave the stored format alone, or
// a correctly configured standby on the older binary can't open the state. The
// control opens the same state without the check and raises it.
func TestRefusedPolicyLeavesTheFormat(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	path := filepath.Join(state, "conduit.db")
	choice, _ := json.Marshal(storageChoice{Backend: sharedTestBackend, Location: path})
	if err := os.WriteFile(filepath.Join(state, storageFile), choice, 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := sqlite.OpenPriorUnowned(ctx, path)
	if err != nil || seed == nil {
		t.Fatalf("older state: %v", err)
	}
	seed.Close()
	func() {
		defer sqlite.SimulateRecordFormat(olderFormat)()
		db, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		raw, _ := json.Marshal(enforcedPolicy{Digest: "aaaa", Path: "/policy"})
		if err = db.Transact(ctx, storage.Guard{Now: upgradeStart}, func(tx storage.Tx) error {
			return tx.Put(policyNamespace, policyKey, raw)
		}); err != nil {
			t.Fatal(err)
		}
	}()
	d := upgradeDaemon(state, upgradeStart)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, db, err := openServing(ctx, state, log, time.Minute, time.Millisecond, d.prepareToOpen, func() {})
	if !errors.Is(err, ErrPolicyChanged) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("opening under the wrong policy: %v", err)
	}
	if got := storedFormat(t, path); got != fmt.Sprint(olderFormat) {
		t.Fatalf("a refused daemon raised the format to %s", got)
	}
	_, db, err = openServing(ctx, state, log, time.Minute, time.Millisecond, d.preserveBeforeUpgrade, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := storedFormat(t, path); got != fmt.Sprint(sqlite.RecordFormat) {
		t.Fatalf("control: the same open without the check left the format at %s", got)
	}
}

// A change leaves a record of what it replaced.
func TestAdmitPolicyAudits(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = admitPolicy(ctx, db, now, &config.Policy{Digest: "aaaa"}, "", "one", log); err != nil {
		t.Fatal(err)
	}
	if err = admitPolicy(ctx, db, now, nil, "none", "two", log); err != nil {
		t.Fatal(err)
	}
	var records []domain.AuditRecord
	if err = db.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		records, err = tx.AuditRecords(0, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Action != "core.policy.changed" || records[1].Detail["previous"] != "sha256:aaaa" || records[1].Detail["current"] != "none" || records[1].Detail["instance"] != "two" {
		t.Fatalf("audit: %+v", records)
	}
}
