package shell_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func runKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

// A run directory made before directories named their execution is mapped
// through its segment's intent; one naming no execution is kept, reported
// unmapped; a known execution's is kept and not reported.
func TestSweepStateMapsLegacyAndKeepsUnmapped(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	known, _ := request("exec-sweep-kept", 1, `{"command":"true"}`)
	run(t, e, known)
	legacy := filepath.Join(dir, "runs", runKey("exec-legacy"), "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "intent.json"), []byte(`{"execution_id":"exec-legacy","nonce":"0123456789abcdef0123456789abcdef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unmapped := filepath.Join(dir, "runs", runKey("nobody"))
	if err := os.MkdirAll(filepath.Join(unmapped, "0123456789abcdef0123456789abcdef"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := e.SweepState(context.Background(), func(_ context.Context, ids []domain.ID) (map[domain.ID]bool, error) {
		return map[domain.ID]bool{"exec-sweep-kept": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Removed, []domain.ID{"exec-legacy"}) {
		t.Fatalf("removed %v", report.Removed)
	}
	if len(report.Kept) != 1 || report.Kept[0] != (extension.StateKept{Ref: unmapped, Kept: "unmapped"}) {
		t.Fatalf("kept %+v", report.Kept)
	}
	if _, err := os.Stat(filepath.Dir(legacy)); !os.IsNotExist(err) {
		t.Fatalf("legacy run directory stayed: %v", err)
	}
	if _, err := os.Stat(unmapped); err != nil {
		t.Fatalf("unmapped run directory removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", runKey("exec-sweep-kept"))); err != nil {
		t.Fatalf("known run directory removed: %v", err)
	}
}

// A segment still running keeps its execution's state, reported in_use,
// however unknown the execution is.
func TestSweepStateKeepsLiveSegment(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	gate := filepath.Join(stateDir(t), "go")
	r, _ := request("exec-sweep-live", 1, `{"command":"while [ ! -e '`+gate+`' ]; do sleep 0.02; done"}`)
	r.PersistRecovery = func(domain.Payload) error { return nil }
	h, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.SweepState(context.Background(), func(context.Context, []domain.ID) (map[domain.ID]bool, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 0 || len(report.Kept) != 1 || report.Kept[0].Execution != "exec-sweep-live" || report.Kept[0].Kept != "in_use" {
		t.Fatalf("report %+v", report)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err = e.SweepState(context.Background(), func(context.Context, []domain.ID) (map[domain.ID]bool, error) { return nil, nil })
	if err != nil || !slices.Equal(report.Removed, []domain.ID{"exec-sweep-live"}) {
		t.Fatalf("after it finished: %+v %v", report, err)
	}
}
