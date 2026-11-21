package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

type unsavedFake struct{ extension.ResourceProvider }

func (unsavedFake) Unsaved(context.Context) ([]extension.ResourceUnsaved, error) {
	return []extension.ResourceUnsaved{{Ref: "work-1", Mode: "work", Path: "/w/1", Kept: "unpushed"}}, nil
}

// silentProvider can't report unsaved work.
type silentProvider struct{ extension.ResourceProvider }

// The report names one of each thing tied to the machine, and a secret's
// reference text never appears in it. A file: reference and a URL path are
// the negative controls.
func TestPortabilityNamesEveryItem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "homes", "h"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	doc := `{"version":1,"settings":{},"connections":[{"id":"acme","type":"acme","config":{"checkout":"/old/home/repo","webhook":"/hooks/in"},"secret_refs":{"a":"keyring:token","b":"cmd:echo hunter2","c":"file:token"}}],"scopes":[{"id":"eng","name":"eng"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, extensionsRoot(path), "thirdparty", protocol.Provides{})
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := func() time.Time { return time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC) }
	configs, err := management.OpenConfig(ctx, store, now, config.Registry{Connections: map[string]func(json.RawMessage) error{"acme": nil}}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "w"}
		if err := tx.PutWork(domain.Work{ID: "w", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "eng", CorrelationKeys: []domain.CorrelationKey{key}}, 0); err != nil {
			return err
		}
		event := domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now(), ReceivedAt: now()}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return err
		}
		if err := tx.PutExecution(domain.Execution{ID: "w/attempt/1", WorkID: "w", ExecutorBinding: "b", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger"}, 0); err != nil {
			return err
		}
		_, err := runtime.BeginDrainInTx(tx, now(), "here", "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d := &daemon{o: Options{StateDir: state, ConfigPath: path}, coord: store, configs: configs, now: now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.resources.providers.set(map[string]extension.ResourceProvider{"git": unsavedFake{}, "other": silentProvider{}})

	p, err := d.portability(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "keyring:token") {
		t.Fatalf("the report holds a secret reference's text: %s", raw)
	}
	var schemes []string
	for _, s := range p.Secrets {
		schemes = append(schemes, s.Scheme)
	}
	slices.Sort(schemes)
	if !slices.Equal(schemes, []string{"cmd", "keyring"}) {
		t.Fatalf("secrets %+v", p.Secrets)
	}
	if len(p.Paths) != 1 || p.Paths[0].Value != "/old/home/repo" {
		t.Fatalf("paths %+v", p.Paths)
	}
	if p.Drain.Safe || len(p.Drain.Scopes) != 1 || p.Drain.Scopes[0].ID != "eng" || !slices.Equal(p.Drain.Scopes[0].Blocking, []string{"w"}) {
		t.Fatalf("undrained scope %+v", p.Drain)
	}
	if len(p.Workspaces) != 1 || p.Workspaces[0].Ref != "work-1" || p.Workspaces[0].Kept != "unpushed" {
		t.Fatalf("workspaces %+v", p.Workspaces)
	}
	if len(p.Packages) != 1 || p.Packages[0].Name != "thirdparty" {
		t.Fatalf("packages %+v", p.Packages)
	}
	if len(p.Directories) != 1 || p.Directories[0].Name != "homes" {
		t.Fatalf("directories %+v", p.Directories)
	}
	if len(p.Unchecked) != 1 || !strings.Contains(p.Unchecked[0], "other") {
		t.Fatalf("a provider that can't answer must be listed: %+v", p.Unchecked)
	}
	if p.Portable {
		t.Fatal("reported portable")
	}
}
