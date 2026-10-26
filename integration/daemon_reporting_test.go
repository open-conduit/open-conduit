package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	is "github.com/alsiola/open-conduit/extensions/introspection"
)

// Issue reporting through public extension hooks (03-08), on the real
// `open-conduit serve` daemon with shell executors calling the real `os`
// toolbelt: an engineering execution reports a defect; the user-bound
// processing binding reports about each report until the core chain-depth
// limit stops the loop; a SIGKILL and restart keep every report; triage runs
// by hand as ordinary Work; and removing the `introspection` entry by config
// apply removes the operations while bindings that still list them keep
// running.

const reportingDepth = 4

const reportingAgentScript = `set -e
x=$(printf %s "$OS_EXECUTION_ID" | tr -c 'a-zA-Z0-9' _)
if os call introspection.report-issue --request-id defect --data '{"kind":"runtime","title":"os call failed","body":"Tried os context; got 503.\nWorkaround: none."}' > "$REP_DIR/agent-$x.out" 2> "$REP_DIR/agent-$x.err"; then
  echo reported
else
  echo refused
fi
`

const reportingProcessorScript = `set -e
if os call introspection.report-issue --request-id loop --data "{\"kind\":\"other\",\"title\":\"about $OS_TRIGGER_ID\",\"body\":\"a report about a report\"}" >> "$REP_DIR/loop.out" 2>> "$REP_DIR/loop.err"; then
  echo looped
else
  echo refused >> "$REP_DIR/refused"
fi
`

const reportingTriageScript = `set -e
x=$(printf %s "$OS_EXECUTION_ID" | tr -c 'a-zA-Z0-9' _)
os call introspection.reports --data '{"state":"all"}' > "$REP_DIR/triage-$x.json"
id=$(grep -o '"id": *"[^"]*"' "$REP_DIR/triage-$x.json" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
os call introspection.report.resolve --data "{\"id\":\"$id\",\"summary\":\"filed upstream\",\"link\":\"https://example.test/issues/1\"}" > "$REP_DIR/resolve-$x.json"
`

func reportingConfig(dir string, enabled bool) []byte {
	ops := []string{"read_context", "declare_outcome", "wait", is.OpReport, is.OpList, is.OpResolve}
	shell := func(name, script string, ops []string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"command": script, "env": map[string]string{"REP_DIR": dir}}}
	}
	extensions := []any{}
	if enabled {
		extensions = append(extensions, map[string]any{"name": "reports", "type": is.Type, "config": map[string]any{}, "secret_names": []any{}})
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"causal_max_depth": reportingDepth}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{
				shell("agent", reportingAgentScript, []string{"read_context", is.OpReport}),
				shell("processor", reportingProcessorScript, []string{is.OpReport}),
				shell("triage", reportingTriageScript, []string{is.OpList, is.OpResolve}),
			},
			"routes": []any{
				map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "reports", "handler": is.Type, "binding": "processor", "active_policy": "queue", "config": map[string]any{}},
			},
			"extensions":  extensions,
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

func TestDaemonIssueReporting(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	dir := filepath.Join(d.root, "rep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, reportingConfig(dir, true), 0o600); err != nil {
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
				arts, _ := d.c.Artifacts(ctx, url.Values{"work_id": {w.ID}})
				for _, a := range arts.Items {
					var out strings.Builder
					_ = d.c.ArtifactContent(ctx, a.ID, "", &out)
					t.Logf("  artifact %s %s:\n%s", a.ID, a.MediaType, out.String())
				}
			}
		}
	})
	d.start("")

	reportEvents := func() []client.Event {
		page, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeReported}})
		if err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	completed := func(kind string) int {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		if err != nil {
			return 0
		}
		n := 0
		for _, w := range page.Items {
			if w.Kind == kind && w.State == "completed" {
				n++
			}
		}
		return n
	}

	// An engineering execution reports; processing loops until core stops it.
	root, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "eng", Type: "drill.defect", DeliveryID: "defect-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "defect-1")
	if err != nil {
		t.Fatal(err)
	}
	d.until("the loop to be refused", 60*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "refused"))
		return err == nil && completed(is.WorkKind) == reportingDepth
	})
	events := reportEvents()
	if len(events) != reportingDepth {
		t.Fatalf("report events: %d, want %d", len(events), reportingDepth)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "loop.err")); !strings.Contains(string(b), "causal_limit") {
		t.Fatalf("loop refusal: %q", b)
	}
	// The deepest report traces back to the operator's root through every
	// report and the execution that made it.
	var deepest client.Event
	for _, e := range events {
		chain, err := d.c.EventChain(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if chain[len(chain)-1].ID != root.ID {
			t.Fatalf("report %s does not trace to the root: %+v", e.ID, chain)
		}
		if len(chain) == reportingDepth+1 {
			deepest = e
		}
		if !strings.HasPrefix(e.Source, "execution/") || e.WorkID != "" {
			t.Fatalf("report envelope: %+v", e)
		}
	}
	if deepest.ID == "" {
		t.Fatal("no report at the depth limit")
	}

	// SIGKILL and restart: every report and its Event is retained.
	d.kill()
	d.start("")
	if after := reportEvents(); len(after) != reportingDepth {
		t.Fatalf("report events after restart: %d", len(after))
	}

	// Triage runs by hand as ordinary Work, after the restart.
	created, err := d.c.CreateWork(ctx, client.WorkRequest{ScopeID: "eng", Kind: "manual.triage", Binding: "triage"}, "triage-1")
	if err != nil {
		t.Fatal(err)
	}
	d.until("triage", 30*time.Second, func() bool {
		w, err := d.c.Work(ctx, created.Work.ID)
		return err == nil && w.State == "completed"
	})
	matches, _ := filepath.Glob(filepath.Join(dir, "triage-*.json"))
	if len(matches) != 1 {
		t.Fatalf("triage outputs: %v", matches)
	}
	var listed struct {
		Data struct {
			Reports []is.Report `json:"reports"`
		} `json:"data"`
	}
	b, _ := os.ReadFile(matches[0])
	if err := json.Unmarshal(b, &listed); err != nil || len(listed.Data.Reports) != reportingDepth {
		t.Fatalf("triage list: %s (%v)", b, err)
	}
	first := listed.Data.Reports[0]
	if first.Kind != "runtime" || first.Title != "os call failed" || first.Depth != 1 || first.Root != root.ID || first.State != is.StateOpen {
		t.Fatalf("first report: %+v", first)
	}
	if b, _ := os.ReadFile(strings.Replace(matches[0], "triage-", "resolve-", 1)); !strings.Contains(string(b), first.ID) {
		t.Fatalf("resolve: %s", b)
	}

	// Removing the entry by config apply removes the operations; bindings
	// that still list them keep running.
	next := reportingConfig(dir, false)
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if _, err = d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "apply-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "eng", Type: "drill.defect", DeliveryID: "defect-2", CorrelationKey: &client.Key{Source: "drill", ExternalID: "two"}}, "defect-2"); err != nil {
		t.Fatal(err)
	}
	d.until("the agent after disabling", 30*time.Second, func() bool { return completed("manual.work") == 2 })
	errs, _ := filepath.Glob(filepath.Join(dir, "agent-*.err"))
	undeclared := 0
	for _, f := range errs {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "undeclared") {
			undeclared++
		}
	}
	if undeclared != 1 || len(reportEvents()) != reportingDepth {
		t.Fatalf("after disabling: %d undeclared calls, %d report events", undeclared, len(reportEvents()))
	}
}
