package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	is "github.com/alsiola/open-conduit/extensions/introspection"
)

// Workflow D (05-03) on the real `open-conduit serve` daemon with shell
// executors calling the real `os` toolbelt: an engineering execution
// reports a defect; the extension's own calendar timer starts the scope's
// standing triage (caught up by a config edit, as v1's six-hour rule
// allows); the triage reads the evidence and requests a fix; the fix runs as
// its own Work on the user's fix binding and resolves the report; the fix
// traces through causation to the report and the engineering origin; the
// telemetry adapter records each report and fix once; a SIGKILL and restart
// keep the schedule; and removing the entry by config apply retires the
// standing triage and removes the operations.

const introspectionAgentScript = `set -e
x=$(printf %s "$OS_EXECUTION_ID" | tr -c 'a-zA-Z0-9' _)
if os call introspection.report-issue --request-id defect --data '{"kind":"runtime","title":"os call failed","body":"Tried os context; got 503.\nWorkaround: none."}' > "$W_DIR/agent-$x.out" 2> "$W_DIR/agent-$x.err"; then
  echo reported
else
  echo refused
fi
`

const introspectionTriageScript = `set -e
x=$(printf %s "$OS_EXECUTION_ID" | tr -c 'a-zA-Z0-9' _)
echo "$OS_TRIGGER_TYPE" > "$W_DIR/triage-trigger-$x"
os call introspection.evidence --data '{"for":"triage"}' > "$W_DIR/evidence-$x.json"
os call introspection.reports > "$W_DIR/open-$x.json"
id=$(grep -o '"id": *"[^"]*"' "$W_DIR/open-$x.json" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
if [ -n "$id" ]; then
  os call introspection.fix --request-id "fix-$id" --data "{\"reports\":[\"$id\"],\"title\":\"os call fails with 503\",\"body\":\"Fix the endpoint wiring in scripts/run.sh.\"}" > "$W_DIR/fix-$x.json"
fi
`

const introspectionFixScript = `set -e
echo "$OS_TRIGGER_ID" > "$W_DIR/fix-trigger"
os call introspection.reports --data '{"state":"fixing"}' > "$W_DIR/fixing.json"
for id in $(grep -o '"id": *"[^"]*"' "$W_DIR/fixing.json" | sed 's/.*"\([^"]*\)"$/\1/'); do
  os call introspection.report.resolve --data "{\"id\":\"$id\",\"summary\":\"fixed in scripts/run.sh\",\"link\":\"https://example.test/pull/1\"}" >> "$W_DIR/resolved.json"
done
`

func introspectionDrillConfig(dir string, entry map[string]any) []byte {
	ops := []string{"read_context", "declare_outcome", "wait", is.OpReport, is.OpList, is.OpResolve, is.OpEvidence, is.OpNotesResolve, is.OpFix}
	shell := func(name, script string, ops []string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"command": script, "env": map[string]string{"W_DIR": dir}}}
	}
	route := func(name, binding, work string) map[string]any {
		return map[string]any{"name": name, "handler": is.Type, "binding": binding, "active_policy": "queue", "config": map[string]any{"work": work}}
	}
	extensions := []any{}
	if entry != nil {
		extensions = append(extensions, map[string]any{"name": "introspection", "type": is.Type, "config": entry, "secret_names": []any{}})
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{}}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{
				shell("agent", introspectionAgentScript, []string{"read_context", is.OpReport}),
				shell("processor", "true", []string{}),
				shell("triager", introspectionTriageScript, []string{is.OpEvidence, is.OpList, is.OpFix, is.OpResolve, is.OpNotesResolve}),
				shell("fixer", introspectionFixScript, []string{is.OpList, is.OpResolve}),
			},
			"routes": []any{
				map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": map[string]any{}},
				route("reports", "processor", is.WorkReport),
				route("triage", "triager", is.WorkTriage),
				route("fixes", "fixer", is.WorkFix),
			},
			"extensions":  extensions,
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

func TestDaemonIntrospection(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	dir := filepath.Join(d.root, "w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"triage": map[string]any{"cron": "17 4 * * 0"}, "telemetry": true}
	if err := os.WriteFile(d.config, introspectionDrillConfig(dir, entry), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, f := range files {
			b, _ := os.ReadFile(f)
			t.Logf("%s:\n%s", filepath.Base(f), b)
		}
		if d.c != nil {
			page, _ := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
			for _, w := range page.Items {
				t.Logf("work %s %s %s", w.ID, w.Kind, w.State)
			}
		}
	})
	d.start("")
	works := func(kind string) []client.Work {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		if err != nil {
			return nil
		}
		var out []client.Work
		for _, w := range page.Items {
			if w.Kind == kind {
				out = append(out, w)
			}
		}
		return out
	}
	doctorItems := func() (client.Check, bool) {
		doc, err := d.c.Doctor(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.Name == "introspection" {
				return c, true
			}
		}
		return client.Check{}, false
	}
	apply := func(cfg []byte, generation, key string) {
		v, err := d.c.ValidateConfig(ctx, cfg)
		if err != nil || !v.Valid {
			t.Fatalf("validate: %+v %v", v, err)
		}
		if _, err = d.c.ApplyConfig(ctx, cfg, v.CandidateHash, generation, key); err != nil {
			t.Fatal(err)
		}
	}

	// The new schedule waits for its next slot.
	if c, ok := doctorItems(); !ok || len(c.Items) != 1 || !strings.Contains(c.Items[0], "triage next") || !strings.Contains(c.Items[0], "telemetry on") {
		t.Fatalf("doctor: %+v", c)
	}

	// An engineering execution reports a defect.
	root, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "eng", Type: "drill.defect", DeliveryID: "defect-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "defect-1")
	if err != nil {
		t.Fatal(err)
	}
	d.until("the report to be processed", 30*time.Second, func() bool {
		ws := works(is.WorkKind)
		return len(ws) == 1 && ws[0].State == "completed"
	})
	reports, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeReported}})
	if err != nil || len(reports.Items) != 1 {
		t.Fatalf("report events: %+v %v", reports, err)
	}
	report := reports.Items[0]

	// Editing the triage time to an hour ago catches that slot up once: the
	// standing triage runs, reads the evidence and requests a fix.
	slot := time.Now().UTC().Add(-time.Hour)
	entry["triage"] = map[string]any{"cron": fmt.Sprintf("%d %d * * *", slot.Minute(), slot.Hour())}
	apply(introspectionDrillConfig(dir, entry), "1", "apply-2")
	d.until("the fix to run", 60*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "resolved.json"))
		fixes := works(is.WorkKindFix)
		return err == nil && len(fixes) == 1 && fixes[0].State == "completed"
	})
	triages := works(is.WorkKindTriage)
	if len(triages) != 1 || triages[0].State != "ready" || !strings.HasPrefix(triages[0].ID, "introspection/eng/triage/") {
		t.Fatalf("standing triage: %+v", triages)
	}
	triggers, _ := filepath.Glob(filepath.Join(dir, "triage-trigger-*"))
	if len(triggers) != 1 {
		t.Fatalf("triage runs: %v", triggers)
	}
	if b, _ := os.ReadFile(triggers[0]); strings.TrimSpace(string(b)) != is.TypeTriageDue {
		t.Fatalf("triage trigger type: %q", b)
	}
	evidence, _ := filepath.Glob(filepath.Join(dir, "evidence-*.json"))
	if b, _ := os.ReadFile(evidence[0]); !strings.Contains(string(b), "os call failed") || !strings.Contains(string(b), `"for"`) {
		t.Fatalf("evidence: %s", b)
	}

	// The fix traces through causation to the report and the origin; its
	// Event names the triage execution; the fix Work's trigger is the fix.
	fixEvents, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeFixRequested}})
	if err != nil || len(fixEvents.Items) != 1 {
		t.Fatalf("fix events: %+v %v", fixEvents, err)
	}
	fix := fixEvents.Items[0]
	chain, err := d.c.EventChain(ctx, fix.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) < 3 || chain[0].ID != fix.ID || chain[1].ID != report.ID || chain[len(chain)-1].ID != root.ID {
		t.Fatalf("fix chain: %+v", chain)
	}
	triageRuns, err := d.c.Runs(ctx, url.Values{"work_id": {triages[0].ID}})
	if err != nil || len(triageRuns.Items) != 1 || fix.Source != "execution/"+triageRuns.Items[0].ID {
		t.Fatalf("fix source %s, triage runs %+v %v", fix.Source, triageRuns.Items, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "fix-trigger")); strings.TrimSpace(string(b)) != fix.ID {
		t.Fatalf("fix trigger: %q, want %s", b, fix.ID)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "resolved.json")); !strings.Contains(string(b), "resolved") {
		t.Fatalf("resolve: %s", b)
	}

	// Telemetry events: each report and fix exactly once.
	logPath := filepath.Join(d.state, "logs", "introspection-telemetry.jsonl")
	d.until("telemetry", 10*time.Second, func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Count(string(b), `"event":"`+is.EventFix+`"`) == 1
	})
	if b, _ := os.ReadFile(logPath); strings.Count(string(b), `"event":"`+is.EventReport+`"`) != 1 || strings.Count(string(b), `"event":"`+is.EventRunDue+`"`) != 1 {
		t.Fatalf("telemetry log:\n%s", b)
	}

	// SIGKILL and restart keep the schedule and the standing triage.
	d.kill()
	d.start("")
	if c, ok := doctorItems(); !ok || !strings.Contains(c.Items[0], "triage next") {
		t.Fatalf("doctor after restart: %+v", c)
	}
	if ws := works(is.WorkKindTriage); len(ws) != 1 || ws[0].State != "ready" {
		t.Fatalf("standing triage after restart: %+v", ws)
	}

	// Removing the entry by config apply retires the standing triage and
	// removes the operations; the agent's binding keeps running.
	apply(introspectionDrillConfig(dir, nil), "2", "apply-3")
	d.until("the standing triage to be retired", 30*time.Second, func() bool {
		ws := works(is.WorkKindTriage)
		return len(ws) == 1 && ws[0].State == "cancelled"
	})
	if c, ok := doctorItems(); !ok || c.Code != "not_configured" {
		t.Fatalf("doctor after removal: %+v", c)
	}
	if _, err = d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "eng", Type: "drill.defect", DeliveryID: "defect-2", CorrelationKey: &client.Key{Source: "drill", ExternalID: "two"}}, "defect-2"); err != nil {
		t.Fatal(err)
	}
	d.until("the agent after removal", 30*time.Second, func() bool {
		n := 0
		for _, w := range works("manual.work") {
			if w.State == "completed" {
				n++
			}
		}
		return n == 2
	})
	errs, _ := filepath.Glob(filepath.Join(dir, "agent-*.err"))
	undeclared := 0
	for _, f := range errs {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "undeclared") {
			undeclared++
		}
	}
	if undeclared != 1 {
		t.Fatalf("after removal: %d undeclared calls", undeclared)
	}
	// Records and history stay.
	if after, _ := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeFixRequested}}); len(after.Items) != 1 {
		t.Fatalf("fix events after removal: %+v", after.Items)
	}
}

// TestDaemonHostWatch: the optional host watch on the real daemon. A disk
// over its threshold becomes an extension-originated report (a root Event
// in the scope's report namespace, processed on the user's report binding
// like any other), doctor lists the reading, and the readings go to the
// telemetry events log.
func TestDaemonHostWatch(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	dir := filepath.Join(d.root, "w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"host_watch": map[string]any{"interval_seconds": 5, "disk": 0.01, "memory": 100, "load": 1000}, "telemetry": true}
	if err := os.WriteFile(d.config, introspectionDrillConfig(dir, entry), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	var report client.Event
	d.until("a host-watch report", 30*time.Second, func() bool {
		page, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeReported}})
		if err != nil || len(page.Items) == 0 {
			return false
		}
		report = page.Items[0]
		return true
	})
	if report.Source != is.SourceNamespace("eng") || report.CausationID != "" || !strings.HasPrefix(report.DeliveryID, is.OriginHostWatch+"/disk:") {
		t.Fatalf("host report envelope: %+v", report)
	}
	if chain, err := d.c.EventChain(ctx, report.ID); err != nil || len(chain) != 1 {
		t.Fatalf("a host report is its own origin: %+v %v", chain, err)
	}
	d.until("the host report to be processed", 30*time.Second, func() bool {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		if err != nil {
			return false
		}
		for _, w := range page.Items {
			if w.Kind == is.WorkKind && w.State == "completed" {
				return true
			}
		}
		return false
	})
	doc, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range doc.Checks {
		if c.Name == "introspection" {
			found = c.Code == "host_over_threshold" && len(c.Items) == 1 && strings.Contains(c.Items[0], "host over: disk")
			if !found {
				t.Fatalf("doctor: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("no introspection check")
	}
	logPath := filepath.Join(d.state, "logs", "introspection-telemetry.jsonl")
	d.until("host telemetry", 10*time.Second, func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), `"event":"`+is.EventHostMetrics+`"`) && strings.Contains(string(b), `"event":"`+is.EventHostAlert+`"`) && strings.Contains(string(b), `"event":"`+is.EventReport+`"`)
	})
}
