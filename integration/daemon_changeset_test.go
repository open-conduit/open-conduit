package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestDaemonSetupChangeSet drives a change set through the composed daemon.
// The agent proposes and withdraws, and a literal secret is refused. The
// operator sees each proposal (references only in the panel, never in an
// Event), rejects one, approves the rest with Approve all, validates, and
// applies the candidate with its expected generation. The applied hook
// tells the agent. While the agent's turn runs, its own execution token
// can't apply, read the candidate, invoke an action or validate as the
// operator: only the operator applies.
func TestDaemonSetupChangeSet(t *testing.T) {
	fake := t.TempDir()
	release := filepath.Join(fake, "release")
	propose := func(id, op, pointer string, value any, why string) extensiontest.Call {
		return extensiontest.Call{Operation: "setup.propose", RequestID: id, Input: map[string]any{"op": op, "pointer": pointer, "value": value, "why": why}}
	}
	// The first turn proposes, validates and probes, asks whether to go
	// on, then holds until the test releases it (so its token is live);
	// the next keeps what it was resumed with and finishes.
	claude, err := extensiontest.FakeClaude(fake,
		extensiontest.Turn{State: "help", Barrier: release, Calls: []extensiontest.Call{
			propose("c1", "replace", "/scopes/setup/name", "Set up", "a clearer name"),
			propose("c2", "add", "/settings/reconcile_seconds", 600, "reconcile every ten minutes"),
			propose("literal", "add", "/scopes/setup/secret_refs/token", "ghp_notareference", "the token"),
			propose("c3", "add", "/scopes/setup/secret_refs/token", "keyring:github-token", "the token, by reference"),
			{Operation: "setup.withdraw", RequestID: "w3", Input: map[string]any{"change": "c3", "reason": "not needed yet"}},
			propose("c4", "add", "/scopes/setup/secret_refs/token", "keyring:github-token", "the token, by reference"),
			{Operation: "setup.validate", RequestID: "v1", Input: map[string]any{}},
			{Operation: "setup.probe", RequestID: "p1", Input: map[string]any{"probe": "setup.tools", "why": "which tools are here"}},
			{Operation: "setup.probe", RequestID: "p2", Input: map[string]any{"probe": "setup.cat", "args": map[string]any{"path": "/etc/passwd"}, "why": "peek"}},
			{Operation: "setup.read", RequestID: "r1", Input: map[string]any{}},
			{Operation: "setup.say", RequestID: "leak", Input: map[string]any{"text": "your token is ghp_abcdefghijklmnopqrstuvwxyz0123456789"}},
			{Operation: "setup.trace", RequestID: "t1", Input: map[string]any{"scope": "setup", "sample": "setup/session-started", "why": "would a session start"}},
			{Operation: "setup.trace", RequestID: "t2", Input: map[string]any{"scope": "nowhere", "why": "no such scope"}},
			{Operation: "setup.ask", RequestID: "go-on", Input: map[string]any{"question": "Shall I go on?", "choices": []string{"yes", "no"}}},
		}},
		extensiontest.Turn{State: "finished"},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := setupBinding(fake, claude.Path)
	binding["operations"] = []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask", "setup.probe", "setup.read", "setup.propose", "setup.withdraw", "setup.validate", "setup.trace"}
	var doc bytes.Buffer
	if err := json.Indent(&doc, setupDaemonConfig(binding), "", "    "); err != nil {
		t.Fatal(err)
	}
	d := extensiontest.Daemon(t, extensiontest.Options{Config: doc.Bytes(), Packages: []string{setupPackage(t)}})
	c := d.Client()
	ctx := context.Background()
	res, err := c.InvokeAction(ctx, client.ActionTarget{ScopeID: "setup"}, "setup", "start", "opus", "start")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	var first extensiontest.TurnRecord
	d.Until("the agent proposed and handed over its token", 30*time.Second, func() bool {
		var ok bool
		first, ok = claude.Turn(1)
		return ok
	})
	result := map[string]extensiontest.Result{}
	for _, r := range first.Results {
		result[r.RequestID] = r
	}
	if r := result["literal"]; !r.IsError || !strings.Contains(r.Text, "not_a_reference") {
		t.Fatalf("a literal secret was not refused: %+v", r)
	}
	// Probes, the read and the trace, each through the toolbelt: an unknown
	// probe and text with a token's shape are refused.
	var probed strings.Builder
	for _, id := range []string{"p1", "p2", "r1", "leak", "t1", "t2"} {
		probed.WriteString(result[id].Text + "\n")
	}
	for _, want := range []string{`"name": "git"`, "unknown_probe", `"outline"`, `"name": "Setup"`, "looks_like_secret", `"kind": "start"`, `"verdict": "match"`, "has no scope nowhere"} {
		if !strings.Contains(probed.String(), want) {
			t.Errorf("probes without %s: %s", want, probed.String())
		}
	}
	if strings.Contains(probed.String(), "ghp_abcdef") {
		t.Errorf("a token reached the output: %s", probed.String())
	}

	// Only the operator applies: the agent's own endpoint serves none of it,
	// and its token is refused on the management socket too.
	exec, err := client.New(first.Endpoint, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := c.Config(ctx)
	if _, err = exec.ApplyConfigText(ctx, doc.Bytes(), cfg.Hash, cfg.Generation, "agent-apply"); !isStatus(err, 404) {
		t.Errorf("the agent applied config: %v", err)
	}
	if _, err = exec.WorkConfigCandidate(ctx, session.WorkID); !isStatus(err, 404) {
		t.Errorf("the agent read the candidate: %v", err)
	}
	if _, err = exec.ValidateConfig(ctx, doc.Bytes()); !isStatus(err, 404) {
		t.Errorf("the agent validated as the operator: %v", err)
	}
	if _, err = exec.InvokeAction(ctx, session, "setup", "approve_c1", "", "agent-approve"); !isStatus(err, 404) {
		t.Errorf("the agent approved its own change: %v", err)
	}
	mgmt, err := client.New(d.Socket(), first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mgmt.ValidateConfig(ctx, doc.Bytes()); !isStatus(err, 403) {
		t.Errorf("the agent's token reached an operator endpoint on the management socket: %v", err)
	}
	for name, call := range map[string]client.ToolCall{
		"an apply op":         {Operation: "setup.propose", Input: json.RawMessage(`{"op":"apply","pointer":"/","value":{},"why":"apply it"}`)},
		"an expected gen":     {Operation: "setup.propose", Input: json.RawMessage(`{"op":"replace","pointer":"/scopes/setup/name","value":"x","why":"x","expected_generation":"1"}`)},
		"a validate to apply": {Operation: "setup.validate", Input: json.RawMessage(`{"apply":true}`)},
		"core config apply":   {Operation: "config.apply", Input: json.RawMessage(`{}`)},
		"core emit":           {Operation: "emit", Input: json.RawMessage(`{"type":"setup.applied","delivery_id":"x"}`)},
	} {
		if _, err = exec.ToolbeltCall(ctx, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if now, _ := c.Config(ctx); now.Generation != "1" {
		t.Fatalf("the generation moved under the agent: %s", now.Generation)
	}
	if err = os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	work := func() client.Work {
		w, _ := c.Work(ctx, session.WorkID)
		return w
	}
	d.Until("the agent parked on its question", 30*time.Second, func() bool { return work().State == "waiting" })

	events := func(typ string) []client.Event {
		page, _ := c.Events(ctx, url.Values{"work_id": {session.WorkID}, "type": {typ}})
		return page.Items
	}
	if probes := events("setup.probe"); len(probes) != 3 || !strings.Contains(string(probes[0].Data.Data), `"probe":"setup.tools"`) || !strings.Contains(string(probes[1].Data.Data), `"verdict":"match"`) || !strings.Contains(string(probes[2].Data.Data), `"error":"the candidate has no scope nowhere"`) {
		t.Fatalf("probe events: %+v", probes)
	}
	if said := events("setup.message"); len(said) != 0 {
		t.Fatalf("a message with a token's shape was recorded: %+v", said)
	}
	proposals := events("setup.proposal")
	if len(proposals) != 4 {
		t.Fatalf("proposals: %+v", proposals)
	}
	for _, p := range proposals {
		if strings.Contains(string(p.Data.Data), "keyring:") || strings.Contains(string(p.Data.Data), "ghp_") {
			t.Fatalf("an Event carries reference text: %s", p.Data.Data)
		}
	}
	if !strings.Contains(string(proposals[3].Data.Data), `"after":"[redacted]"`) {
		t.Fatalf("the reference's proposal: %s", proposals[3].Data.Data)
	}
	// The session draws itself as panels, its conversation with a note of
	// each proposal; core reviews the change set, reference text included,
	// from the candidate, which has nothing to apply until a change is
	// approved.
	panels, err := c.Panels(ctx, session)
	if err != nil || len(panels) != 2 || panels[0].ID != "conversation" || !strings.Contains(mustJSONString(panels), "Proposed c1: replace /scopes/setup/name") || strings.Contains(mustJSONString(panels), "keyring:") {
		t.Fatalf("panels: %s %v", mustJSONString(panels), err)
	}
	review, err := c.WorkConfigCandidate(ctx, session.WorkID)
	if err != nil || review.CandidateHash != "" || review.ConfigText != "" || len(review.Changes) != 4 || string(review.Changes[0].Before) != `"Setup"` ||
		review.Changes[0].Approve != "approve_c1" || review.Changes[2].State != "withdrawn" || review.Changes[2].Approve != "" || !strings.Contains(mustJSONString(review.Changes), "keyring:github-token") || !strings.HasPrefix(review.ApproveAll, "approve_all_") {
		t.Fatalf("review: %s %v", mustJSONString(review), err)
	}

	actions := func() map[string]client.Action {
		acts, _ := c.Actions(ctx, session)
		out := map[string]client.Action{}
		for _, a := range acts {
			if a.Provider == "setup" {
				out[a.ID] = a
			}
		}
		return out
	}
	if _, ok := actions()["approve_c3"]; ok {
		t.Fatal("a withdrawn change can be approved")
	}
	if _, err = c.InvokeAction(ctx, session, "setup", "reject_c4", "not until the secret is stored", "reject-c4"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	all := ""
	for id, a := range actions() {
		if strings.HasPrefix(id, "approve_all_") {
			all = id
			if a.Label != "Approve all 2" {
				t.Fatalf("approve all: %+v", a)
			}
		}
	}
	if _, err = c.InvokeAction(ctx, session, "setup", "approve_all_00000000", "", "stale"); err == nil {
		t.Fatal("an Approve all for proposals the operator didn't see was accepted")
	}
	if _, err = c.InvokeAction(ctx, session, "setup", all, "", "all"); err != nil {
		t.Fatalf("approve all: %v", err)
	}
	if _, err = c.InvokeAction(ctx, session, "setup", "validate", "", "validate"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	checks := events("setup.checks")
	if len(checks) != 2 || !strings.Contains(string(checks[1].Data.Data), `"valid":true`) || !strings.Contains(string(checks[1].Data.Data), `"approved":2`) {
		t.Fatalf("checks: %+v", checks)
	}

	cand, err := c.WorkConfigCandidate(ctx, session.WorkID)
	if err != nil || cand.BaseGeneration != "1" || !strings.Contains(cand.ConfigText, `"name": "Set up"`) || !strings.Contains(cand.ConfigText, `"reconcile_seconds": 600`) || strings.Contains(cand.ConfigText, "github-token") {
		t.Fatalf("candidate: %+v %v", cand, err)
	}
	if _, err = c.ApplyConfigText(ctx, []byte(cand.ConfigText), cand.CandidateHash, cand.BaseGeneration, "apply"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if saved, _ := os.ReadFile(d.ConfigPath()); string(saved) != cand.ConfigText {
		t.Fatalf("apply did not save the text as given:\n%s", saved)
	}
	d.Until("the session heard its changes were applied", 30*time.Second, func() bool { return len(events("setup.applied")) == 1 })
	if a := events("setup.applied")[0]; !strings.Contains(string(a.Data.Data), `"generation":"2"`) || !strings.Contains(string(a.Data.Data), `"changes":["c1","c2"]`) {
		t.Fatalf("applied: %s", a.Data.Data)
	}
	if _, ok := actions()["validate"]; ok {
		t.Fatal("validate is still offered with nothing approved left to apply")
	}

	// The question offered yes or no; an answer in the operator's own words
	// still answers it.
	if _, err = c.InvokeAction(ctx, session, "setup", "answer", "Yes, and raise the slots to 3", "answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	d.Until("the session finished", 30*time.Second, func() bool { return work().State == "completed" })
	resumed, _ := claude.Turn(2)
	for _, want := range []string{"rejected c4", "not until the secret is stored", "approved c1, c2", "applied your changes", "Generation 2", "Yes, and raise the slots to 3"} {
		if !strings.Contains(resumed.Input, want) {
			t.Errorf("resumed without %q: %s", want, resumed.Input)
		}
	}
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
