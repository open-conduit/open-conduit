package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Example external extension binaries (07-02), built once per test binary.
var (
	extOnce               sync.Once
	notesBin, runnerBin   string
	extBuildErr           error
	extensionScope        = domain.ID("notes-scope")
	extensionNoteTime     = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	extensionLimits       = runtime.CausalLimits{MaxDepth: 8, PerMinute: 1000}
	extensionHandshakeMax = 30 * time.Second
)

func extensionBinaries(t *testing.T) (string, string) {
	t.Helper()
	extOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oc-extension-bin-")
		if err != nil {
			extBuildErr = err
			return
		}
		for _, b := range []struct{ out, pkg string }{{"notes-extension", "./protocol/examples/notes/cmd/notes-extension"}, {"runner-extension", "./protocol/examples/runner/cmd/runner-extension"}} {
			args := []string{"build", "-o", filepath.Join(dir, b.out)}
			if raceEnabled {
				args = append(args, "-race")
			}
			cmd := exec.Command("go", append(args, b.pkg)...)
			cmd.Dir = ".."
			if out, err := cmd.CombinedOutput(); err != nil {
				extBuildErr = fmt.Errorf("build %s: %v\n%s", b.out, err, out)
				return
			}
		}
		notesBin, runnerBin = filepath.Join(dir, "notes-extension"), filepath.Join(dir, "runner-extension")
	})
	if extBuildErr != nil {
		t.Fatal(extBuildErr)
	}
	return notesBin, runnerBin
}

// extEnv is one host: SQLite coordination, an install root and a state
// directory, with the trusted clock.
type extEnv struct {
	t       *testing.T
	root    string
	state   string
	store   storage.Store
	clock   *conformance.Clock
	service string
}

func newExtEnv(t *testing.T) *extEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "coordination.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Runs after every extension the test loaded has been closed: a race
	// the detector found inside an extension process fails the test too.
	t.Cleanup(func() {
		logs, _ := filepath.Glob(filepath.Join(dir, "state", "logs", "extensions", "*.log"))
		for _, path := range logs {
			if b, _ := os.ReadFile(path); bytes.Contains(b, []byte("WARNING: DATA RACE")) {
				t.Errorf("the race detector reported a data race in %s:\n%s", filepath.Base(path), b)
			}
		}
	})
	e := &extEnv{t: t, root: filepath.Join(dir, "config", extproc.Dir), state: filepath.Join(dir, "state"), store: db, clock: conformance.NewClock(), service: filepath.Join(dir, "service.json")}
	if err := db.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: extensionScope, Name: "notes"}, 0); err != nil {
			return err
		}
		return tx.PutScope(domain.Scope{ID: "other-scope", Name: "other"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.writeService(notes.Note{ID: "a", Rev: "r1", Text: "hello world", At: extensionNoteTime}, notes.Note{ID: "b", Rev: "r1", Text: "second note", At: extensionNoteTime})
	return e
}

func (e *extEnv) writeService(ns ...notes.Note) {
	e.t.Helper()
	raw, err := json.Marshal(notes.ServiceFile{Notes: ns})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.service, raw, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// install writes the manifest into <root>/<name>/ beside a link to binary
// named as the manifest's command.
func (e *extEnv) install(m protocol.Manifest, binary string) extproc.Found {
	e.t.Helper()
	dir := filepath.Join(e.root, m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	link := filepath.Join(dir, filepath.Base(m.Command[0]))
	_ = os.Remove(link)
	if err := os.Symlink(binary, link); err != nil {
		e.t.Fatal(err)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		e.t.Fatal(err)
	}
	parsed, err := protocol.ParseManifest(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	return extproc.Found{Dir: dir, Manifest: parsed, Raw: raw}
}

func (e *extEnv) deps() extproc.Deps {
	return extproc.Deps{Store: e.store, Now: e.clock.Now, Limits: extensionLimits, Scopes: func() []domain.ID { return []domain.ID{extensionScope} }}
}

func (e *extEnv) configure() protocol.ConfigureParams {
	cfg, _ := json.Marshal(map[string]string{"service": e.service})
	return protocol.ConfigureParams{Generation: "generation-1", Entries: []protocol.ConfigEntry{{ScopeID: string(extensionScope), Type: "notes", Config: cfg}}}
}

func (e *extEnv) options(name string) extproc.Options {
	return extproc.Options{StateDir: filepath.Join(e.state, "extensions", name), LogDir: filepath.Join(e.state, "logs", "extensions"), Host: protocol.HostInfo{Name: "open-conduit", Version: "test"}, Configure: e.configure, KV: extproc.StoreKV{Store: e.store, Now: e.clock.Now}, MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, HandshakeTimeout: extensionHandshakeMax}
}

// load starts f and closes it at cleanup.
func (e *extEnv) load(f extproc.Found, deps extproc.Deps, o extproc.Options) *extproc.Remote {
	e.t.Helper()
	r, err := extproc.Load(context.Background(), f, deps, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = r.Process().Close() })
	return r
}

// crash kills the extension and waits for the supervisor's restart.
func crash(t *testing.T, r *extproc.Remote) {
	t.Helper()
	gen := r.Process().Generation()
	r.Process().Kill()
	awaitRestart(t, r, gen)
}

func awaitRestart(t *testing.T, r *extproc.Remote, gen uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), extensionHandshakeMax)
	defer cancel()
	if err := r.Process().AwaitGeneration(ctx, gen); err != nil {
		t.Fatalf("extension did not restart: %v", err)
	}
}

// pipeHost connects an extension-side Host to a service over an in-memory
// protocol connection (the wire codec and the host's scoping, without a
// process).
func pipeHost(t *testing.T, svc *extproc.Service) *sdk.Host {
	t.Helper()
	hr, ew := io.Pipe()
	er, hw := io.Pipe()
	host := jsonrpc.NewConn(hr, hw, svc.Handle, jsonrpc.Options{})
	ext := jsonrpc.NewConn(er, ew, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, protocol.Errorf(protocol.CodeMethodNotFound, "none")
	}, jsonrpc.Options{})
	t.Cleanup(func() { _ = host.Close(); _ = ext.Close() })
	return sdk.NewHost(ext)
}

func eventCount(t *testing.T, store storage.Store, now time.Time) int {
	t.Helper()
	n := 0
	if err := store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		n = len(events)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventByDelivery(t *testing.T, store storage.Store, now time.Time, delivery string) (domain.AcceptedEvent, bool) {
	t.Helper()
	var out domain.AcceptedEvent
	found := false
	if err := store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		for _, e := range events {
			if e.Event.DeliveryID == delivery {
				out, found = e, true
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out, found
}
