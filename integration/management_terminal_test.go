package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensiontest"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// terminalWork is Work whose first attempt handed back, so it has a session
// to attach to and no live attempt.
func terminalWork(t *testing.T, h *managementHarness, key string) (string, domain.ID) {
	t.Helper()
	var created struct {
		Work client.Work `json:"work"`
	}
	h.mustCLI(&created, "work", "create", "--scope-id", "ops", "--kind", "fixture.work", "--binding", "run", "--key", key)
	a, err := h.scheduler.Start(context.Background(), domain.ID(created.Work.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.scheduler.Complete(context.Background(), a.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff}}); err != nil {
		t.Fatal(err)
	}
	return created.Work.ID, a.Request.Execution.ID
}

func terminalClient(t *testing.T, h *managementHarness) *client.Client {
	t.Helper()
	dir := t.TempDir()
	networkCerts(t, dir)
	endpoint := serveNetwork(t, h, management.NetworkConfig{CertFile: filepath.Join(dir, "server.pem"), KeyFile: filepath.Join(dir, "server.key")})
	c, err := client.NewWithOptions(endpoint, ownerToken(t, h), client.Options{CAFile: filepath.Join(dir, "ca.pem")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readUntil(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("reading for %q after %q: %v", want, got.String(), err)
		}
		got.WriteByte(b)
	}
}

func heldUntil(t *testing.T, c *client.Client, id string, held bool) client.Work {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := c.Work(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if (w.Hold != nil) == held {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("hold is %v, want held=%v", w.Hold, held)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// From another machine, a terminal opens the session, input reaches it, the
// Work is held throughout, and its end releases the hold and is on record.
func TestTerminalOverTheNetwork(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "terminal-net")
	c := terminalClient(t, h)
	ctx := context.Background()
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"sh", "-c", `stty -echo; printf 'ready\n'; read line; printf 'got:%s\n' "$line"`}}, nil
	}
	term, err := c.OpenTerminal(ctx, id, client.TerminalOptions{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	r := bufio.NewReader(term)
	readUntil(t, r, "ready")
	w := heldUntil(t, c, id, true)

	// Nothing else may take the session while it is open.
	if _, err = c.ReleaseWork(ctx, id, w.Revision, "", "release-under-terminal"); !isCode(err, "terminal_open") {
		t.Fatalf("release under a terminal: %v", err)
	}
	if _, err = c.OpenTerminal(ctx, id, client.TerminalOptions{}); !isCode(err, "terminal_open") {
		t.Fatalf("a second terminal: %v", err)
	}
	if _, err = term.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, r, "got:hello")
	heldUntil(t, c, id, false)

	events, err := c.Events(ctx, url.Values{"work_id": {id}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, e := range events.Items {
		seen[e.Type] = e.Actor
	}
	for _, typ := range []string{"core.manual.terminal_opened", "core.manual.terminal_closed"} {
		if !strings.HasPrefix(seen[typ], "operator:") {
			t.Fatalf("%s actor %q in %v", typ, seen[typ], seen)
		}
	}
}

// A view credential is refused before anything happens to the Work.
func TestTerminalRefusesView(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "terminal-view")
	ran := false
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		ran = true
		return management.AttachSpec{Argv: []string{"true"}}, nil
	}
	path := filepath.Join(h.state, "viewer.token")
	h.mustCLI(nil, "auth", "add", "vera", "--role", "view", "--out", path)
	f, err := client.ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := client.New(h.endpoint, f.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = viewer.OpenTerminal(context.Background(), id, client.TerminalOptions{}); !isStatus(err, 403) {
		t.Fatalf("a view credential: %v", err)
	}
	var w client.Work
	h.mustCLI(&w, "work", "get", id)
	if ran || w.Hold != nil {
		t.Fatalf("a refused terminal acted: ran %v, hold %v", ran, w.Hold)
	}
}

// A client that dies mid-session releases the hold and ends the process.
func TestTerminalClientKilledReleasesHold(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "terminal-kill")
	c := terminalClient(t, h)
	marker := filepath.Join(h.state, "hup")
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"sh", "-c", `trap 'touch "$0"; exit 0' HUP; printf 'ready\n'; while :; do sleep 0.05; done`, marker}}, nil
	}
	term, err := c.OpenTerminal(context.Background(), id, client.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, bufio.NewReader(term), "ready")
	heldUntil(t, c, id, true)
	if err = term.Abort(); err != nil {
		t.Fatal(err)
	}
	heldUntil(t, c, id, false)
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("the session was not hung up: %v", err)
	}
}

// An earlier operator's hold is not the terminal's to remove.
func TestTerminalLeavesAnEarlierHold(t *testing.T) {
	h := newManagementHarness(t)
	id, _ := terminalWork(t, h, "terminal-earlier")
	var w client.Work
	h.mustCLI(&w, "work", "get", id)
	h.mustCLI(nil, "work", "hold", id, "--revision", w.Revision, "--key", "hold-earlier")
	h.attach = func(context.Context, string, domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"true"}}, nil
	}
	c := terminalClient(t, h)
	term, err := c.OpenTerminal(context.Background(), id, client.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(term).ReadByte()
	_ = term.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var open bool
		if err := h.store.Transact(context.Background(), storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
			_, found, e := runtime.TerminalInTx(tx, domain.ID(id))
			open = found
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal stayed open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := heldUntil(t, c, id, true); got.Hold == nil {
		t.Fatal("the earlier hold was removed")
	}
}

// Work in a container is only reached through the provider's attach
// container, which is removed when the terminal ends.
func TestTerminalInContainer(t *testing.T) {
	h := newManagementHarness(t)
	h.container = func(_ context.Context, st runtime.AttemptState, r extension.ResourceBroker, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		return runtime.ContainerBinding{Broker: pinningContainer{pin}, Resources: r, Home: filepath.Join(h.state, "homes", "w")}, nil
	}
	h.executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"claude"}}); err != nil {
			return nil, err
		}
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	id, _ := terminalWork(t, h, "terminal-contained")
	c := terminalClient(t, h)
	hostRan := filepath.Join(h.state, "host-ran")
	h.attach = func(_ context.Context, _ string, w domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"sh", "-c", "touch " + hostRan}}, nil
	}
	if _, err := c.OpenTerminal(context.Background(), id, client.TerminalOptions{}); !isCode(err, "attach_sandboxed") {
		t.Fatalf("a host command for contained Work: %v", err)
	}
	heldUntil(t, c, id, false)
	h.attach = func(_ context.Context, _ string, w domain.Work) (management.AttachSpec, error) {
		return management.AttachSpec{Argv: []string{"open-conduit", "container", "attach", string(w.ID)}}, nil
	}
	detached := make(chan string, 1)
	h.containerAttach = func(context.Context, string, domain.Work, domain.ID) (management.ContainerAttachment, error) {
		return management.ContainerAttachment{Ref: "ctx/attach-1", Argv: []string{"sh", "-c", `printf 'in:%s\n' "$FROM_PROVIDER"`}, Env: []string{"FROM_PROVIDER=yes"}}, nil
	}
	h.containerDetach = func(_ context.Context, _ domain.Work, ref string) error {
		detached <- ref
		return nil
	}
	term, err := c.OpenTerminal(context.Background(), id, client.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	readUntil(t, bufio.NewReader(term), "in:yes")
	select {
	case ref := <-detached:
		if ref != "ctx/attach-1" {
			t.Fatalf("detached %q", ref)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the attach container was not removed")
	}
	heldUntil(t, c, id, false)
	if _, err = os.Stat(hostRan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a host command ran: %v", err)
	}
}

func isCode(err error, code string) bool {
	var api *client.Error
	return errors.As(err, &api) && api.Code == code
}

// A third-party package's container provider serves a terminal through the
// public contract alone: the daemon runs the attach Argv it returned under a
// terminal and removes the container afterwards.
func TestTerminalThirdPartyContainerProvider(t *testing.T) {
	image := "example.test/pod@sha256:" + strings.Repeat("a", 64)
	cfg, err := json.Marshal(map[string]any{"version": 1, "connections": []any{}, "settings": map[string]any{"retry_delays_seconds": []int{1}},
		"scopes": []any{map[string]any{"id": "pods", "name": "Pods",
			"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "pod", "executor": "podterm", "requires": map[string]any{"toolbelt": "none"}, "operations": []any{}, "secret_names": []any{}, "config": map[string]any{},
				"container": map[string]any{"provider": "podterm", "config": map[string]any{"image": image}}}},
			"routes": []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "pod", "active_policy": "queue", "config": map[string]any{}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := extensiontest.Daemon(t, extensiontest.Package("testdata/podterm"), extensiontest.Options{Config: cfg})
	c := d.Client()
	ctx := context.Background()
	out, err := c.CreateWork(ctx, client.WorkRequest{ScopeID: "pods", Kind: "pod.work", Binding: "pod"}, "pod-work")
	if err != nil {
		t.Fatal(err)
	}
	id := out.Work.ID
	d.Until("the first turn to hand back", extensiontest.Deadline, func() bool {
		runs, err := c.Runs(ctx, url.Values{"work_id": {id}})
		return err == nil && len(runs.Items) > 0 && runs.Items[0].State != "running" && runs.Items[0].State != "pending"
	})
	term, err := c.OpenTerminal(ctx, id, client.TerminalOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, d.Log())
	}
	defer term.Close()
	r := bufio.NewReader(term)
	readUntil(t, r, "pod-tty")
	if _, err = term.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, r, "pod-got:hi")
	heldUntil(t, c, id, false)
}

// A terminal lives in the daemon's process: after a crash its record is
// closed under the operator who opened it, and the hold it placed goes, while
// a hold that was there first stays.
func TestTerminalRecoveredAfterRestart(t *testing.T) {
	h := newManagementHarness(t)
	ctx := runtime.WithActor(context.Background(), "operator:olly")
	placed, _ := terminalWork(t, h, "terminal-crash")
	earlier, _ := terminalWork(t, h, "terminal-crash-earlier")
	var w client.Work
	h.mustCLI(&w, "work", "get", earlier)
	h.mustCLI(nil, "work", "hold", earlier, "--revision", w.Revision, "--key", "hold-crash")
	for i, id := range []string{placed, earlier} {
		if _, err := h.control.OpenTerminal(ctx, domain.ID(id), "crashed"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	n, err := h.control.RecoverTerminals(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("recovered %d: %v", n, err)
	}
	c := h.operatorClient()
	heldUntil(t, c, placed, false)
	heldUntil(t, c, earlier, true)
	events, err := c.Events(context.Background(), url.Values{"work_id": {placed}, "type": {"core.manual.terminal_closed"}})
	if err != nil || len(events.Items) != 1 || events.Items[0].Actor != "operator:olly" {
		t.Fatalf("the close on record: %+v %v", events.Items, err)
	}
	if n, err = h.control.RecoverTerminals(context.Background()); err != nil || n != 0 {
		t.Fatalf("a second recovery closed %d: %v", n, err)
	}
}
