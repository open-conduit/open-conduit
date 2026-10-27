package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
)

func TestSharedExecutorConformance(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	r, _ := h.request("work/c", "exec-conformance", 1, nil)
	conformance.RunExecutor(t, func() extension.Executor { return h.executor() }, r)
}

// TestSharedInjectConformance runs the generic inject acknowledgement case:
// the fake claude echoes the injected message (consumption), a retried
// dispatch is not written twice, a finished turn takes nothing more and the
// recorded proof survives recovery.
func TestSharedInjectConformance(t *testing.T) {
	h := newHarness(t)
	h.mode("wait")
	r, _ := h.request("work/ci", "exec-inject-conformance", 1, map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "FAKE_MESSAGES": "2"}})
	conformance.RunInjectExecutor(t, func() conformance.InjectFixture {
		return conformance.InjectFixture{Executor: h.executor(), Request: r,
			Ready: func() error {
				waitFor(t, "the turn started", func() bool { _, err := os.Stat(filepath.Join(h.fake, "started")); return err == nil })
				return nil
			},
			Finish: func() error {
				waitFor(t, "the echo", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
				return os.WriteFile(filepath.Join(h.fake, "release"), nil, 0o600)
			}}
	})
}

func TestCapabilitiesAndBinding(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	caps := e.Capabilities()
	if !caps.Interrupt || !caps.Inject || !caps.Resume || !caps.Workspace || caps.Toolbelt != extension.ToolbeltMCP {
		t.Fatalf("capabilities %+v", caps)
	}
	if err := extension.ValidateBinding(domain.Binding{Name: "agent", Requires: domain.Capabilities{Toolbelt: "mcp", Inject: true, Workspace: true}}, e); err != nil {
		t.Fatal(err)
	}
	if err := extension.ValidateBinding(domain.Binding{Name: "agent", Requires: domain.Capabilities{Toolbelt: "cli"}}, e); err == nil {
		t.Fatal("a cli requirement bound to the MCP executor")
	}
}

func TestConfigValidation(t *testing.T) {
	e := newHarness(t).executor()
	for _, bad := range []string{
		`{"unknown":1}`, `{"permission_mode":"yolo"}`, `{"playbook":"../x"}`, `{"plugin_dir":"rel"}`,
		`{"env":{"OS_TOKEN":"x"}}`, `{"pass_env":["OS_ENDPOINT"]}`, `{"cold_turns":"draw"}`, `{"handoff_minutes":-1}`,
		`{"extra_args":["--mcp-config","x"]}`, `{"extra_args":["--resume"]}`, `{} {}`,
	} {
		if err := e.ValidateConfig(json.RawMessage(bad), nil); !errors.Is(err, claudecode.ErrConfig) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
	if err := e.ValidateConfig(json.RawMessage(`{"secret_env":{"key":"ANTHROPIC_API_KEY"}}`), nil); err == nil {
		t.Error("secret_env naming an unselected secret accepted")
	}
	if err := e.ValidateConfig(json.RawMessage(`{"secret_env":{"key":"ANTHROPIC_API_KEY"},"model":"sonnet","cold_turns":"resume"}`), []string{"key"}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	// The removed experiment's setting is still accepted for one release.
	if err := e.ValidateConfig(json.RawMessage(`{"handoff_share":0.5}`), nil); err != nil {
		t.Errorf("handoff_share rejected: %v", err)
	}
	if err := e.ValidateConfig(nil, nil); err != nil {
		t.Errorf("an empty config takes every default: %v", err)
	}
	// A turn is declared under the core schema; source-named schemas from
	// earlier setups read as it, and other schemas are kept.
	for in, want := range map[string]string{`{}`: domain.TurnSchema, `{"status_schema":"github.status.v1"}`: domain.TurnSchema, `{"status_schema":"linear.status.v1"}`: domain.TurnSchema, `{"status_schema":"acme.result.v2"}`: "acme.result.v2"} {
		if c, err := claudecode.ParseConfig([]byte(in)); err != nil || c.StatusSchema != want {
			t.Errorf("%s: status schema %q (%v), want %q", in, c.StatusSchema, err, want)
		}
	}
}

// TestTurnInvocationEnvironmentAndResult checks the first turn: a fresh
// session on the work-issue playbook with the rendered input, the MCP
// toolbelt configuration (credential in a replaceable file, never in
// claude's environment), the sanitized environment with the API key only
// from a selected secret, and the structured outcome, usage and artifacts.
func TestTurnInvocationEnvironmentAndResult(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	r, saved := h.request("work/1", "exec-1", 1, map[string]any{"secret_env": map[string]string{"key": "ANTHROPIC_API_KEY"}})
	r.Context.Secrets = map[string]string{"key": "sk-from-secret"}
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken = "unix:///tmp/x.sock", "tok-1"
	res := h.run(e, r)
	if saved.Schema != claudecode.RecoverySchema {
		t.Fatalf("recovery metadata %+v", saved)
	}
	if res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "turn 1" || res.Outcome.Data.Schema != domain.TurnSchema {
		t.Fatalf("outcome %+v", res.Outcome)
	}
	d := dataOf(t, res)
	if d.Status != "review" || d.Reason != "status" || d.PRs[0] != 7 || d.Kind != "start" || d.ToolCalls != 2 || d.ModelCalls != 2 || d.Models[0] != "claude-x" {
		t.Fatalf("data %+v", d)
	}
	if strings.Contains(string(res.Outcome.Data.Data), "note 1") || strings.Contains(string(res.Outcome.Data.Data), "session") {
		t.Fatalf("private continuity leaked into the outcome: %s", res.Outcome.Data.Data)
	}
	if res.Usage.Cost == nil || *res.Usage.Cost != 0.5 || res.Usage.Currency != "USD" || *res.Usage.InputTokens != 10 || *res.Usage.CachedTokens != 100 {
		t.Fatalf("usage %+v", res.Usage)
	}
	if d.Transcript == "" || len(res.Artifacts) == 0 || !strings.Contains(string(h.art.data[d.Transcript]), `"type":"result"`) {
		t.Fatalf("transcript artifact %q %+v", d.Transcript, res.Artifacts)
	}
	inv := h.invocations()
	if len(inv) != 1 || !strings.Contains(inv[0], "resume=0") || !strings.Contains(inv[0], "mcp="+h.state) {
		t.Fatalf("invocation %v", inv)
	}
	prompts := h.prompts()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "/open-conduit:work-issue 12\n\n## Already waiting\n\n### github.comment.human (issue) #12 by alice") || !strings.Contains(prompts[0], "please fix") {
		t.Fatalf("prompt %q", prompts)
	}
	env, _ := os.ReadFile(filepath.Join(h.fake, "env"))
	got := string(env)
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-from-secret") || strings.Contains(got, "ambient") || strings.Contains(got, "CLAUDECODE") || strings.Contains(got, "CLAUDE_CODE_ENTRYPOINT") || strings.Contains(got, "UNRELATED") {
		t.Fatalf("environment:\n%s", got)
	}
	if strings.Contains(got, "OS_TOKEN") || strings.Contains(got, "OS_ENDPOINT") || !strings.Contains(got, "OS_WORK_ID=work/1") || !strings.Contains(got, "OS_EXECUTION_ID=exec-1") {
		t.Fatalf("toolbelt credentials or identity in claude's environment:\n%s", got)
	}
	// The MCP server definition names `os mcp`, the endpoint and the credential
	// file, never the token itself.
	mcpPath := strings.TrimPrefix(inv[0][strings.Index(inv[0], "mcp="):], "mcp=")
	b, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	var mcp struct {
		Servers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	srv := mcp.Servers[claudecode.MCPServerName]
	if filepath.Base(srv.Command) != "os" || len(srv.Args) != 1 || srv.Args[0] != "mcp" || srv.Env["OS_ENDPOINT"] != "unix:///tmp/x.sock" || srv.Env["OS_TOKEN_FILE"] == "" || strings.Contains(string(b), "tok-1") {
		t.Fatalf("mcp config %s", b)
	}
	if _, err := os.Stat(srv.Env["OS_TOKEN_FILE"]); !os.IsNotExist(err) {
		t.Fatalf("the token file outlived the segment: %v", err)
	}
	rec := h.store.record(t, "work/1")
	if rec["session"] == "" || rec["handoff"] != "note 1" || rec["turns"] != float64(1) {
		t.Fatalf("continuity %v", rec)
	}
}

// TestResumedFreshAndColdTurns follows Open Supervisor's
// TestColdSessionHandsOffToAFreshOne through the executor: a warm turn
// resumes with its own cumulative usage delta; a cold turn starts a fresh
// session from the note and keeps the old one (cold_turns: handoff, the
// default); with cold_turns: resume a cold turn resumes; attaching forces the
// next turn to resume; and terminal Work's continuity is released.
func TestResumedFreshAndColdTurns(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	cfg := map[string]any{}
	turn := func(n int) extension.Result {
		t.Helper()
		r, _ := h.request("work/8", "exec-8-"+strconv.Itoa(n), 1, cfg)
		return h.run(e, r)
	}
	cold := func() { h.clock.Advance(2 * time.Hour) }
	session := func() string { return h.store.record(t, "work/8")["session"].(string) }

	turn(1)
	first := session()
	if h.store.record(t, "work/8")["handoff"] != "note 1" {
		t.Fatal("the handoff note wasn't kept")
	}

	// A warm session resumes, measuring its own turn.
	h.clock.Advance(10 * time.Minute)
	warm := turn(2)
	if inv := h.invocations(); !strings.Contains(inv[1], "session="+first+" resume=1") {
		t.Fatalf("a warm turn didn't resume: %v", inv)
	}
	if d := dataOf(t, warm); d.Kind != "events" || d.Tokens.CacheRead != 100 || d.Tokens.Input != 10 || math.Abs(*warm.Usage.Cost-0.5) > 1e-9 {
		t.Fatalf("a resumed turn's usage is not its own: %+v %+v", d.Tokens, warm.Usage)
	}
	if !strings.HasPrefix(h.prompts()[1], "/open-conduit:work-issue 12 events\n\n## Since your last turn") {
		t.Fatalf("warm prompt %q", h.prompts()[1])
	}

	// A cold one starts afresh from the note.
	cold()
	turn(3)
	fresh := session()
	if fresh == first || !strings.Contains(h.invocations()[2], "session="+fresh+" resume=0") {
		t.Fatalf("a cold turn didn't start a fresh session: %v", h.invocations())
	}
	p := h.prompts()[2]
	if !strings.HasPrefix(p, "/open-conduit:work-issue 12 handoff\n\n## Handoff from your last session\n\nnote 2") || !strings.Contains(p, "please fix") {
		t.Fatalf("the fresh session lacks the handoff or the events:\n%s", p)
	}
	rec := h.store.record(t, "work/8")
	if past := rec["past_sessions"].([]any); len(past) != 1 || past[0] != first {
		t.Fatalf("earlier sessions %v", past)
	}
	turns := h.reports.named("agent_turn")
	if last := turns[len(turns)-1]; last["handoff"] != true || last["session"] != fresh {
		t.Fatalf("turn telemetry %v", last)
	}

	// With cold_turns: resume, a cold one resumes.
	cfg["cold_turns"] = "resume"
	cold()
	turn(4)
	if session() != fresh || !strings.Contains(h.invocations()[3], "session="+fresh+" resume=1") {
		t.Fatalf("a cold turn set to resume: %v", h.invocations())
	}
	delete(cfg, "cold_turns")

	// Attach is the public optional executor surface; Work without a
	// session has nothing to attach to.
	var attacher extension.AttachExecutor = e
	if _, err := attacher.Attach(context.Background(), "work/none"); !errors.Is(err, extension.ErrNothingToAttach) {
		t.Fatalf("attach without a session: %v", err)
	}
	// After the operator attaches, the next turn resumes even when cold.
	spec, err := e.Attach(context.Background(), "work/8")
	if err != nil || spec.Argv[0] != h.binary || spec.Argv[1] != "--resume" || spec.Argv[2] != fresh || spec.Dir == "" {
		t.Fatalf("attach %+v %v", spec, err)
	}
	cold()
	turn(5)
	if !strings.Contains(h.invocations()[4], "session="+fresh+" resume=1") {
		t.Fatalf("a turn after attaching didn't resume: %v", h.invocations())
	}

	h.mode("finished")
	res := turn(6)
	if res.Outcome.Kind != domain.OutcomeCompleted || dataOf(t, res).Status != "finished" {
		t.Fatalf("a finished turn %+v", res.Outcome)
	}
	// Terminal Work's continuity is released.
	if _, err := e.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := h.store.record(t, "work/8"); len(rec) == 0 {
		t.Fatal("a live Work's continuity was released")
	}
	h.store.setWork(domain.Work{ID: "work/8", ScopeID: "eng", State: domain.WorkCompleted})
	if _, err := e.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := h.store.record(t, "work/8"); len(rec) != 0 {
		t.Fatalf("continuity outlived its Work: %v", rec)
	}
}

// TestColdDecisionSurvivesRestart covers continuity across restart: the
// decision is recorded before launch, so an executor restarted while a cold
// handoff turn runs adopts it; and a decision whose launch never finished is
// taken over by the next segment instead of deciding again.
func TestColdDecisionSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	first := h.executor()
	r, _ := h.request("work/9", "exec-9-1", 1, nil)
	h.run(first, r)
	original := h.store.record(t, "work/9")["session"].(string)

	// The cold turn hands off and still runs when the daemon goes away.
	h.clock.Advance(2 * time.Hour)
	h.mode("wait")
	r2, saved := h.request("work/9", "exec-9-2", 1, nil)
	if _, err := first.Run(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cold turn running", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	pending := h.store.record(t, "work/9")["pending"].(map[string]any)
	if pending["kind"] != "handoff" || pending["session"] == original {
		t.Fatalf("decision not recorded before launch: %v", pending)
	}

	// A new instance adopts at a higher epoch.
	second := h.executor()
	r2.Execution.Lease.Epoch = 2
	r2.Context.ToolbeltToken = "tok-2"
	adopted, err := second.Recover(context.Background(), r2, *saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.fake, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.wait(adopted)
	if res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("adopted outcome %+v", res.Outcome)
	}
	rec := h.store.record(t, "work/9")
	if rec["session"] != pending["session"] || rec["turns"] != float64(2) || rec["pending"] != nil {
		t.Fatalf("continuity after adoption %v", rec)
	}

	// A decision whose launch never finished (a crash after recording it) is
	// reused: the same session resumes though the turn is cold.
	os.Remove(filepath.Join(h.fake, "release"))
	os.Remove(filepath.Join(h.fake, "pid"))
	h.mode("review")
	h.store.edit(t, "work/9", func(m map[string]any) {
		m["pending"] = map[string]any{"execution": "exec-9-crashed", "segment": "x", "session": rec["session"], "kind": "events", "started_at": h.clock.Now()}
	})
	h.clock.Advance(2 * time.Hour)
	third := h.executor()
	r3, _ := h.request("work/9", "exec-9-3", 1, nil)
	h.run(third, r3)
	if inv := h.invocations(); !strings.Contains(inv[len(inv)-1], "session="+rec["session"].(string)+" resume=1") {
		t.Fatalf("taken-over decision did not resume its session: %v", inv)
	}
}

// TestExperimentRecordsAreIgnored: continuity written while the handoff
// experiment ran (a trial, feedback questions, a drawn arm) still loads, and
// a Work that ends releases it.
func TestExperimentRecordsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	r, _ := h.request("work/d", "exec-d-1", 1, nil)
	h.run(e, r)
	h.store.edit(t, "work/d", func(m map[string]any) {
		m["trial"] = map[string]any{"id": "work/d-2", "arm": "resume", "started_at": h.clock.Now(), "turns": 1}
		m["feedback"] = []any{map[string]any{"intent": "claude-code/handoff-feedback/x/y", "arm": "resume", "state": "watching", "ref": "comment-1"}}
	})
	h.clock.Advance(10 * time.Minute)
	r2, _ := h.request("work/d", "exec-d-2", 1, nil)
	if res := h.run(e, r2); res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("a turn on an experiment record: %+v", res.Outcome)
	}
	if inv := h.invocations(); !strings.Contains(inv[1], "resume=1") {
		t.Fatalf("the warm turn didn't resume: %v", inv)
	}
	h.store.setWork(domain.Work{ID: "work/d", ScopeID: "eng", State: domain.WorkCancelled})
	if _, err := e.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := h.store.record(t, "work/d"); len(rec) != 0 {
		t.Fatalf("continuity outlived its cancelled Work: %v", rec)
	}
}

// TestInterjection hands an accepted input to the running turn; claude echoes
// it back, which is recorded as consumed. A finished turn takes no more.
func TestInterjection(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	cfg := map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "FAKE_MESSAGES": "2"}}
	r, _ := h.request("work/i", "exec-i", 1, cfg)
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn started", func() bool { _, err := os.Stat(filepath.Join(h.fake, "started")); return err == nil })
	ev := domain.Event{ID: "ev-more", Type: "github.comment.human", Data: domain.Payload{Data: json.RawMessage(`{"author":"bob","body":"also the footer"}`)}}
	if err := e.Inject(context.Background(), "exec-i", ev); err != nil {
		t.Fatal(err)
	}
	res := h.wait(handle)
	d := dataOf(t, res)
	if d.Injected == nil || d.Injected.Accepted != 1 || d.Injected.Consumed != 1 {
		t.Fatalf("injection %+v", d.Injected)
	}
	if len(res.ConsumedInputs) != 1 || res.ConsumedInputs[0] != "ev-more" {
		t.Fatalf("consumption proof %+v", res.ConsumedInputs)
	}
	if p := h.prompts(); len(p) != 2 || !strings.HasPrefix(p[1], "## Arrived during this turn") || !strings.Contains(p[1], "also the footer") {
		t.Fatalf("interjection %q", p)
	}
	if err := e.Inject(context.Background(), "exec-i", ev); !errors.Is(err, claudecode.ErrNotAccepting) {
		t.Fatalf("a finished turn took a message: %v", err)
	}
	if err := e.Inject(context.Background(), "unknown", ev); !errors.Is(err, claudecode.ErrUnknownExecution) {
		t.Fatalf("an unknown execution took a message: %v", err)
	}
}

// TestInterjectionEchoedAsTheTurnEnds: claude echoes an injected input and
// ends its turn at once, before the follower polls its output again (the
// follower is held back here, so this is deterministic). Settling the turn
// still finds the echo, so the input is reported consumed and core does not
// start another attempt to present it (hosted CI run 40293639876).
func TestInterjectionEchoedAsTheTurnEnds(t *testing.T) {
	release := make(chan struct{})
	defer claudecode.HoldFollowers(release)()
	defer close(release)
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	cfg := map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "FAKE_MESSAGES": "2"}}
	r, _ := h.request("work/e", "exec-e", 1, cfg)
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn started", func() bool { _, err := os.Stat(filepath.Join(h.fake, "started")); return err == nil })
	ev := domain.Event{ID: "ev-last", Type: "github.comment.human", Data: domain.Payload{Data: json.RawMessage(`{"author":"bob","body":"one last thing"}`)}}
	if err := e.Inject(context.Background(), "exec-e", ev); err != nil {
		t.Fatal(err)
	}
	res := h.wait(handle)
	if d := dataOf(t, res); d.Injected == nil || d.Injected.Accepted != 1 || d.Injected.Consumed != 1 {
		t.Fatalf("injection %+v", d.Injected)
	}
	if len(res.ConsumedInputs) != 1 || res.ConsumedInputs[0] != "ev-last" {
		t.Fatalf("consumption proof %+v", res.ConsumedInputs)
	}
}

// TestUsageLimitDelaysTurns: a turn stopped by the usage limit fails with a
// retry hint at the reset, is not counted against its trial, and holds every
// later turn on the executor (another Work's included) without launching it.
func TestUsageLimitDelaysTurns(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(time.Hour).Truncate(time.Second)
	h.mode("limit")
	e := h.executor()
	cfg := map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "FAKE_RESET": strconv.FormatInt(reset.Unix(), 10)}}
	r, _ := h.request("work/l", "exec-l-1", 1, cfg)
	res := h.run(e, r)
	if res.Outcome.Kind != domain.OutcomeFailed || res.RetryHint == nil || res.RetryHint.ResetAt == nil || !res.RetryHint.ResetAt.Equal(reset) {
		t.Fatalf("limit result %+v hint %+v", res.Outcome, res.RetryHint)
	}
	if d := dataOf(t, res); d.Reason != "usage_limit" || !d.Limited {
		t.Fatalf("limit data %+v", d)
	}
	r2, _ := h.request("work/m", "exec-m-1", 1, cfg)
	h.mode("review")
	held := h.run(e, r2)
	if held.Outcome.Kind != domain.OutcomeFailed || held.RetryHint == nil || !held.RetryHint.ResetAt.Equal(reset) || dataOf(t, held).Reason != "usage_limit_hold" {
		t.Fatalf("a turn started while the executor was at its limit: %+v %+v", held.Outcome, held.RetryHint)
	}
	if n := len(h.invocations()); n != 1 {
		t.Fatalf("%d claude launches while held", n)
	}
	// Once the limit lifts (with its grace) turns run again.
	h.clock.Advance(time.Hour + 3*time.Minute)
	r3, _ := h.request("work/m", "exec-m-2", 1, cfg)
	if res := h.run(e, r3); res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("after the limit lifted %+v", res.Outcome)
	}
}

func TestFailedTurnsAndInterrupt(t *testing.T) {
	h := newHarness(t)
	h.mode("error")
	e := h.executor()
	r, _ := h.request("work/f", "exec-f-1", 1, nil)
	res := h.run(e, r)
	if res.Outcome.Kind != domain.OutcomeFailed || !strings.Contains(res.Outcome.Summary, "claude exited 1 without a result: boom") || res.RetryHint != nil {
		t.Fatalf("failure %+v", res)
	}
	// An unparseable binding config fails without a process.
	bad, _ := h.request("work/f", "exec-f-2", 1, nil)
	bad.Context.Config.Data = json.RawMessage(`{"nope":true}`)
	if res := h.run(e, bad); res.Outcome.Kind != domain.OutcomeFailed || dataOf(t, res).Reason != "invalid_config" {
		t.Fatalf("invalid config %+v", res.Outcome)
	}
	// A missing binary is a structured failure too.
	missing, _ := h.request("work/f", "exec-f-3", 1, map[string]any{"binary": filepath.Join(h.root, "no-such-claude")})
	if res := h.run(e, missing); res.Outcome.Kind != domain.OutcomeFailed || !strings.Contains(res.Outcome.Summary, "without a result") {
		t.Fatalf("missing binary %+v", res.Outcome)
	}
	// Interrupt stops a running turn and reports it.
	h.mode("wait")
	r4, _ := h.request("work/f", "exec-f-4", 1, nil)
	handle, err := e.Run(context.Background(), r4)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	b, _ := os.ReadFile(filepath.Join(h.fake, "pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err := e.Interrupt(context.Background(), "exec-f-4"); err != nil {
		t.Fatal(err)
	}
	res = h.wait(handle)
	if res.Outcome.Kind != domain.OutcomeFailed || !strings.Contains(res.Outcome.Summary, "interrupted") {
		t.Fatalf("interrupt %+v", res.Outcome)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("the interrupted turn survived")
	}
	if err := e.Interrupt(context.Background(), "nope"); !errors.Is(err, claudecode.ErrUnknownExecution) {
		t.Fatalf("unowned interrupt %v", err)
	}
	// Retention forgets finished executions' segments.
	if err := e.Forget(context.Background(), "exec-f-4"); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptedDetachedTurn: a second instance adopts a live turn at a higher
// epoch without relaunching it, replaces the MCP credential file, refuses a
// lower epoch, and Stop on a superseded epoch is refused.
func TestAdoptedDetachedTurn(t *testing.T) {
	h := newHarness(t)
	h.mode("wait")
	first := h.executor()
	r, saved := h.request("work/a", "exec-a", 1, nil)
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken = "unix:///tmp/x.sock", "tok-1"
	if _, err := first.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	second := h.executor()
	r.Execution.Lease.Epoch, r.Context.ToolbeltToken = 2, "tok-2"
	adopted, err := second.Recover(context.Background(), r, *saved)
	if err != nil {
		t.Fatal(err)
	}
	inv := h.invocations()
	mcpPath := strings.TrimPrefix(inv[0][strings.Index(inv[0], "mcp="):], "mcp=")
	token, _ := os.ReadFile(filepath.Join(filepath.Dir(mcpPath), "token"))
	if strings.TrimSpace(string(token)) != "tok-2" {
		t.Fatalf("credential not replaced: %q", token)
	}
	if err := first.Stop(context.Background(), "exec-a", 1); !errors.Is(err, claudecode.ErrSuperseded) {
		t.Fatalf("a superseded epoch stopped the adopted turn: %v", err)
	}
	lower := r
	lower.Execution.Lease.Epoch = 1
	if _, err := h.executor().Recover(context.Background(), lower, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("a lower epoch adopted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.fake, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.wait(adopted)
	if res.Outcome.Kind != domain.OutcomeHandoff || len(h.invocations()) != 1 {
		t.Fatalf("adopted result %+v after %d launches", res.Outcome, len(h.invocations()))
	}
	// The finished result is returned identically to a later recovery.
	again, err := h.executor().Recover(context.Background(), func() extension.RunRequest { x := r; x.Execution.Lease.Epoch = 3; return x }(), *saved)
	if err != nil {
		t.Fatal(err)
	}
	if res2 := h.wait(again); res2.Outcome.Summary != res.Outcome.Summary || len(h.invocations()) != 1 {
		t.Fatalf("recovered result differs %+v", res2.Outcome)
	}
	if _, err := h.executor().Recover(context.Background(), r, domain.Payload{Schema: claudecode.RecoverySchema, Data: json.RawMessage(`{"run":"x/y"}`)}); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("tampered metadata adopted: %v", err)
	}
}

// fakeResources is a resource broker returning one directory per Work.
type fakeResources struct {
	dir      string
	acquired []extension.ResourceRequest
	looked   []string
}

func (f *fakeResources) Acquire(_ context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	f.acquired = append(f.acquired, r)
	return extension.Resource{Kind: "git", Ref: "lease-1", Dir: f.dir, Env: map[string]string{"FROM_WORKSPACE": "1", "OS_TOKEN": "forged"}, Data: domain.Payload{Schema: "open-conduit.git.workspace.v1", Data: json.RawMessage(`{"branch":"agent/issue-12"}`)}}, nil
}
func (f *fakeResources) Lookup(_ context.Context, ref string) (extension.Resource, error) {
	f.looked = append(f.looked, ref)
	return extension.Resource{Kind: "git", Ref: ref, Dir: f.dir}, nil
}

// TestWorkspaceAndResume: a work workspace is acquired with the branch
// template, the session lives there (so its transcript decides resume), and a
// parked attempt's resumed segment looks the lease up instead of acquiring.
func TestWorkspaceAndResume(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	e := h.executor()
	res := &fakeResources{dir: filepath.Join(h.root, "worktree")}
	if err := os.MkdirAll(res.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"branch": "agent/issue-{number}"}
	r, saved := h.request("work/w", "exec-w", 1, cfg)
	r.Context.Resources = res
	out := h.run(e, r)
	if len(res.acquired) != 1 || res.acquired[0].Mode != "work" || !strings.Contains(string(res.acquired[0].Params.Data), `"agent/issue-12"`) {
		t.Fatalf("acquisition %+v", res.acquired)
	}
	if !strings.Contains(string(out.Outcome.Data.Data), `"ref":"lease-1"`) || strings.Contains(string(out.Outcome.Data.Data), res.dir) {
		t.Fatalf("workspace in outcome %s", out.Outcome.Data.Data)
	}
	env, _ := os.ReadFile(filepath.Join(h.fake, "env"))
	if strings.Contains(string(env), "forged") {
		t.Fatalf("a workspace layer set a toolbelt variable:\n%s", env)
	}
	// The wake resumes the same attempt as a new segment in the same lease.
	wake := domain.Event{ID: "ev-wake", Type: "github.checks.passed", Data: domain.Payload{Data: json.RawMessage(`{"number":7,"state":"passed"}`)}}
	r.Trigger, r.Inputs = wake, []domain.Event{wake}
	r.Execution.Lease.Epoch = 2
	handle, err := e.Resume(context.Background(), r, *saved)
	if err != nil {
		t.Fatal(err)
	}
	h.wait(handle)
	if len(res.acquired) != 1 || len(res.looked) != 1 || res.looked[0] != "lease-1" {
		t.Fatalf("resume acquired instead of looking up: %+v %v", res.acquired, res.looked)
	}
	if inv := h.invocations(); len(inv) != 2 || !strings.Contains(inv[1], "resume=1") {
		t.Fatalf("resume did not continue the session: %v", inv)
	}
	if p := h.prompts(); !strings.Contains(p[1], "### github.checks.passed #7") {
		t.Fatalf("wake prompt %q", p[1])
	}
}

// The daemon proves physical stops and attaches through the public optional
// interfaces.
var (
	_ extension.StopExecutor   = (*claudecode.Executor)(nil)
	_ extension.AttachExecutor = (*claudecode.Executor)(nil)
)

// TestLiveOutput: a running turn shows its activity and transcript so far,
// in the order its artifacts will have, and is not live once it ends.
func TestLiveOutput(t *testing.T) {
	h := newHarness(t)
	h.mode("wait")
	e := h.executor()
	cfg := map[string]any{"env": map[string]string{"FAKE_DIR": h.fake}}
	r, _ := h.request("work/l", "exec-l", 1, cfg)
	if _, err := e.Live(context.Background(), "exec-l", 0); !errors.Is(err, extension.ErrNotLive) {
		t.Fatalf("live before launch: %v", err)
	}
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn is running", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	out, err := e.Live(context.Background(), "exec-l", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Name != "activity" || out[1].Name != "transcript" || out[2].Name != "stderr" {
		t.Fatalf("live outputs %+v", out)
	}
	if !strings.Contains(string(out[0].Content), "session started") || out[0].MediaType != claudecode.ActivityMediaType {
		t.Fatalf("live activity %q", out[0].Content)
	}
	if !strings.Contains(string(out[1].Content), `"subtype":"init"`) || out[1].MediaType != claudecode.TranscriptMediaType || out[1].Size != int64(len(out[1].Content)) {
		t.Fatalf("live transcript %+v", out[1])
	}
	tail, err := e.Live(context.Background(), "exec-l", 5)
	if err != nil || len(tail[1].Content) != 5 || tail[1].Size != out[1].Size {
		t.Fatalf("tailed transcript %+v %v", tail, err)
	}
	if err := os.WriteFile(filepath.Join(h.fake, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.wait(handle)
	if len(res.Artifacts) != 2 || res.Artifacts[0].MediaType != claudecode.ActivityMediaType || res.Artifacts[1].MediaType != claudecode.TranscriptMediaType {
		t.Fatalf("final artifacts %+v", res.Artifacts)
	}
	if _, err := e.Live(context.Background(), "exec-l", 0); !errors.Is(err, extension.ErrNotLive) {
		t.Fatalf("live after the turn: %v", err)
	}
}
