package bootstrap

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// TestDrainSurvivesAStartAnywhere: a copy of the state can't know the daemon
// it came from has stopped, so reopening it at another path keeps the drain.
func TestDrainSurvivesAStartAnywhere(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, moved := filepath.Join(root, "state"), filepath.Join(root, "elsewhere")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, err := runtime.BeginDrainInTx(tx, now, drainOrigin(state), "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, filepath.Join(moved, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var logged bytes.Buffer
	if err := logDrainInForce(ctx, store, now, slog.New(slog.NewTextHandler(&logged, nil)), moved); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level=WARN", "drain end", "old daemon", "reason=moving", "origin=" + state, "state_dir=" + moved} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the start log lacks %q: %s", want, logged.String())
		}
	}
	logged.Reset()
	if err := logDrainInForce(ctx, store, now, slog.New(slog.NewTextHandler(&logged, nil)), state); err != nil {
		t.Fatal(err)
	}
	if s := logged.String(); !strings.Contains(s, "drain end") || strings.Contains(s, "old daemon") {
		t.Errorf("a start where the drain began logged: %s", s)
	}
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		d, on, err := runtime.DrainInTx(tx)
		if err == nil && (!on || d.Origin != state) {
			t.Errorf("after the move: draining %v, origin %q", on, d.Origin)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The negative control: with no drain in force, a start says nothing.
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, err := runtime.EndDrainInTx(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	if err := logDrainInForce(ctx, store, now, slog.New(slog.NewTextHandler(&logged, nil)), moved); err != nil || logged.Len() != 0 {
		t.Errorf("no drain: %v, logged %q", err, logged.String())
	}
}
