package claudecode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
)

// lockedLog is an OpenLog target that records each write.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (*lockedLog) Close() error { return nil }
func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// The extension opens its executor with the host's services: an input
// Event a family owner renders reaches the prompt in its owner's words, the
// rest in the generic rendering; each turn's telemetry goes to its own log
// as JSON lines; and the maintain task releases a terminal Work's
// continuity.
func TestExtensionRendersOwnedEventsLogsAndMaintains(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	log := &lockedLog{}
	x := claudecode.NewExtension()
	err := x.Start(context.Background(), inproc.Host{
		StateDir: filepath.Join(h.root, "daemon"), Store: h.store, Artifacts: h.art, Now: h.clock.Now,
		Render: func(_ context.Context, e domain.Event) (string, bool) {
			if e.Type == "acme.ping" {
				return "ACME OWNER RENDERING", true
			}
			return "", false
		},
		OpenLog: func(name string) (io.WriteCloser, error) {
			if name != claudecode.Name {
				t.Fatalf("log %q", name)
			}
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := x.Executors()[claudecode.Name]
	exec, ok := reg.Executor.(*claudecode.Executor)
	if !ok || reg.Pool != "" {
		t.Fatalf("registration %+v", reg)
	}
	owned := domain.Event{ID: "ev-owned", Type: "acme.ping", Data: domain.Payload{Schema: "acme.ping.v1", Data: json.RawMessage(`{}`)}}
	generic := domain.Event{ID: "ev-generic", Type: "github.comment.human", Data: domain.Payload{Schema: "github.comment.v1", Data: json.RawMessage(`{"author":"alice","body":"please fix","kind":"issue","number":12}`)}}
	r, _ := h.request("work/9", "exec-9", 1, nil, owned, generic)
	h.run(exec, r)
	prompts := h.prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "ACME OWNER RENDERING") || !strings.Contains(prompts[0], "please fix") {
		t.Fatalf("prompt %q", prompts)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	var rec struct{ Name string }
	if len(lines) == 0 || lines[0] == "" || json.Unmarshal([]byte(lines[0]), &rec) != nil || rec.Name == "" {
		t.Fatalf("telemetry log %q", log.String())
	}

	tasks := x.Tasks()
	if len(tasks) != 2 || tasks[0].Name != "maintain" || tasks[1].Name != "tasks" {
		t.Fatalf("tasks %+v", tasks)
	}
	if len(h.store.record(t, "work/9")) == 0 {
		t.Fatal("the turn kept no continuity")
	}
	h.store.setWork(domain.Work{ID: "work/9", ScopeID: "eng", State: domain.WorkCompleted})
	if err := tasks[0].Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := h.store.record(t, "work/9"); len(rec) != 0 {
		t.Fatalf("continuity outlived its Work: %v", rec)
	}
	x.Stop()
}
