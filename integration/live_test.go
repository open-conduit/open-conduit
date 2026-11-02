package integration_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// liveExecutor is a history fixture that shows its output while it runs.
type liveExecutor struct {
	*historyExecutor
	mu   sync.Mutex
	text string
	done bool
}

func (e *liveExecutor) write(s string) {
	e.mu.Lock()
	e.text += s
	e.mu.Unlock()
}

func (e *liveExecutor) Live(_ context.Context, _ domain.ID, tail int64) ([]extension.LiveOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return nil, extension.ErrNotLive
	}
	b := []byte(e.text)
	size := int64(len(b))
	if tail > 0 && size > tail {
		b = b[size-tail:]
	}
	return []extension.LiveOutput{
		{Name: "activity", MediaType: "text/plain; charset=utf-8", Size: size, Content: b},
		{Name: "transcript", MediaType: "application/x-ndjson", Size: 2, Content: []byte("{}")},
	}, nil
}

// TestRunsLogFollowsARunningRun: `runs log` prints a running run's output
// so far, --follow prints what it writes next until it ends, and once it
// has ended the log is its artifact as before.
func TestRunsLogFollowsARunningRun(t *testing.T) {
	ctx := context.Background()
	state, bin := shellState(t, mixedConfig())
	agent := &liveExecutor{historyExecutor: &historyExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltMCP}, block: make(chan struct{}), result: func(extension.RunRequest) extension.Result {
		return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "answered"}}
	}}}
	job := &historyExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltNone}}
	h := newShellHostWith(t, state, bin, conformance.NewClock(), "daemon", false, shellHostOptions{ingress: true,
		executors: map[string]extension.Executor{"agent": agent, "job": job},
		handlers:  map[string]extension.RouteHandler{"agent": typeRoute("operator.agent", "fixture.conversation"), "job": typeRoute("operator.job", "fixture.job")}})
	agent.artifacts, job.artifacts = h.artifacts, h.artifacts
	c := h.operator()
	work := h.submit("operator.agent", "agent-1", "agent-1").RoutedWorkID
	a, err := h.scheduler.Start(ctx, domain.ID(work))
	if err != nil {
		t.Fatal(err)
	}
	id := string(a.Lease.ExecutionID)
	agent.write("session started\nassistant: reading the issue\n")

	live, err := c.RunLive(ctx, id, 0)
	if err != nil || len(live.Outputs) != 2 || !strings.Contains(live.Outputs[0].Content, "reading the issue") {
		t.Fatalf("live run: %+v %v", live, err)
	}
	args := []string{"--endpoint", h.endpoint, "--token-file", state + "/private/operator.token"}
	env := cli.Env{Getenv: func(string) string { return "" }}
	var out, errOut strings.Builder
	if code := cli.RunOperator("t", "c", append(args, "runs", "log", id), env, &out, &errOut); code != 0 || out.String() != "session started\nassistant: reading the issue\n" {
		t.Fatalf("runs log on a running run: %d %q %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := cli.RunOperator("t", "c", append(args, "runs", "log", id, "--transcript"), env, &out, &errOut); code != 0 || out.String() != "{}" {
		t.Fatalf("runs log --transcript: %d %q %q", code, out.String(), errOut.String())
	}

	defer func(d time.Duration) { cli.FollowInterval = d }(cli.FollowInterval)
	cli.FollowInterval = 10 * time.Millisecond
	var followed strings.Builder
	var mu sync.Mutex
	done := make(chan int)
	go func() {
		var w lockedWriter
		w.mu, w.b = &mu, &followed
		var e strings.Builder
		done <- cli.RunOperator("t", "c", append(args, "runs", "log", id, "--follow"), env, w, &e)
	}()
	waitUntil(t, "the first output is followed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(followed.String(), "reading the issue")
	})
	agent.write("  tool Bash ls\n")
	waitUntil(t, "new output is followed", func() bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(followed.String(), "tool Bash ls") })
	agent.mu.Lock()
	agent.done = true
	agent.mu.Unlock()
	close(agent.block)
	h.observe(a)
	select {
	case code := <-done:
		mu.Lock()
		got := followed.String()
		mu.Unlock()
		if code != 0 || got != "session started\nassistant: reading the issue\n  tool Bash ls\n" {
			t.Fatalf("followed log: %d %q", code, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("--follow did not end with the run")
	}
	// Ended, the run's log is its artifact; it has no live output.
	out.Reset()
	if code := cli.RunOperator("t", "c", append(args, "runs", "log", id), env, &out, &errOut); code != 0 || !strings.Contains(out.String(), "log of "+id) {
		t.Fatalf("runs log on an ended run: %d %q %q", code, out.String(), errOut.String())
	}
	if _, err := c.RunLive(ctx, id, 0); !client.IsNotLive(err) {
		t.Fatalf("ended run is live: %v", err)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
