package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
