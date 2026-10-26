package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// lastChecks is the latest observed checks Event of a type.
func (e *toolsEnv) lastChecks(t *testing.T, typ string) gh.ChecksPayload {
	t.Helper()
	var found *domain.Event
	for _, ev := range e.events() {
		if ev.Event.Type == typ {
			x := ev.Event
			found = &x
		}
	}
	if found == nil {
		t.Fatalf("no %s observed", typ)
	}
	var c gh.ChecksPayload
	if err := json.Unmarshal(found.Data.Data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *toolsEnv) renderLast(typ string) string {
	var text string
	for _, ev := range e.events() {
		if ev.Event.Type == typ {
			text, _ = gh.RenderEvent(ev.Event)
		}
	}
	return text
}

// Port of v1 c37edf1 engine TestMissingRequiredCheckIsNotAPass, with the
// protection and ruleset sources, a refused read and the github.checks tool:
// the agent never hears "Checks passed" while a check the base branch
// requires has not reported.
func TestGitHubRequiredChecks(t *testing.T) {
	t.Run("all required pass", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Protected", "", "bug")
		pr := e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireChecks("main", "test", "netlify/site/deploy-preview")
		e.fake.SetCheck("h1", "test", "completed", "success")
		e.fake.SetStatus("h1", "netlify/site/deploy-preview", "success", "https://preview.example/1")
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksPassed {
			t.Fatalf("queued: %v", q)
		}
		a := e.start(w.ID)
		out := e.must(a, gh.OpChecks, "", map[string]any{"number": pr})
		if out["state"] != "passed" || out["missing"] != nil || !strings.Contains(fmt.Sprint(out["required"]), "branch_protection") {
			t.Fatalf("checks tool: %v", out)
		}
		e.finish(a, nil)
	})

	t.Run("one missing until the timeout", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Deploy skipped", "", "bug")
		pr := e.fake.OpenPull("Change [skip netlify]", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireChecks("main", "test", "netlify/site/deploy-preview")
		e.fake.SetCheck("h1", "test", "completed", "success")
		e.sync()
		if n := e.countType(gh.TypeChecksPassed) + e.countType(gh.TypeChecksMissing); n != 0 {
			t.Fatalf("the deploy check hadn't reported yet, so nothing should have been reported (%d)", n)
		}
		if held := e.lastChecks(t, gh.TypeChecksPending); strings.Join(held.Missing, ",") != "netlify/site/deploy-preview" {
			t.Fatalf("held head: %+v", held)
		}
		for _, typ := range e.queued(w.ID) {
			if strings.HasPrefix(typ, "github.checks.") {
				t.Fatalf("a held head reached the Work: %v", e.queued(w.ID))
			}
		}
		a := e.start(w.ID)
		out := e.must(a, gh.OpChecks, "", map[string]any{"number": pr})
		if out["state"] != "waiting on required checks" || fmt.Sprint(out["missing"]) != "[netlify/site/deploy-preview]" {
			t.Fatalf("checks tool while held: %v", out)
		}
		e.finish(a, nil)

		e.clock.Add(11 * time.Minute)
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksMissing {
			t.Fatalf("queued: %v", q)
		}
		text := e.renderLast(gh.TypeChecksMissing)
		if !strings.Contains(text, "Required checks never reported on PR #"+itoa(pr)+" at h1: netlify/site/deploy-preview") || strings.Contains(text, "Checks passed") {
			t.Fatalf("checks turn:\n%s", text)
		}
		if e.countType(gh.TypeChecksPassed) != 0 {
			t.Fatal("a missing required check was reported as a pass")
		}
		before := e.countType(gh.TypeChecksMissing)
		e.sync()
		if e.countType(gh.TypeChecksMissing) != before {
			t.Fatal("missing checks observed twice for one head state")
		}
		// The deploy finally reports: the head now passes.
		e.fake.SetStatus("h1", "netlify/site/deploy-preview", "success", "https://preview.example/1")
		e.sync()
		if q := e.queued(w.ID); q[len(q)-1] != gh.TypeChecksPassed {
			t.Fatalf("queued after the deploy reported: %v", q)
		}
	})

	t.Run("ruleset source and nothing reported", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Ruleset", "", "bug")
		e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireRulesetChecks("main", "typecheck")
		e.sync()
		e.clock.Add(11 * time.Minute)
		e.sync()
		if e.countType(gh.TypeChecksNone) != 0 {
			t.Fatal("no checks ran reported although the base branch requires one")
		}
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksMissing {
			t.Fatalf("queued: %v", q)
		}
		if c := e.lastChecks(t, gh.TypeChecksMissing); strings.Join(c.Missing, ",") != "typecheck" || c.None {
			t.Fatalf("missing: %+v", c)
		}
	})

	t.Run("failed and missing", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Both", "", "bug")
		e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireChecks("main", "test", "deploy")
		e.fake.SetCheck("h1", "test", "completed", "failure")
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksFailed {
			t.Fatalf("queued: %v", q)
		}
		if text := e.renderLast(gh.TypeChecksFailed); !strings.Contains(text, "Checks failed on PR") || !strings.Contains(text, "These required checks never reported either: deploy.") {
			t.Fatalf("render:\n%s", text)
		}
	})

	t.Run("refused read degrades", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Fine-grained token", "", "bug")
		pr := e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireChecks("main", "deploy")
		e.fake.RefuseRequiredChecks(true, true)
		e.fake.SetCheck("h1", "test", "completed", "success")
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksPassed {
			t.Fatalf("a refused read must not fail reconcile or hold the head: %v", q)
		}
		a := e.start(w.ID)
		out := e.must(a, gh.OpChecks, "", map[string]any{"number": pr})
		if out["state"] != "passed" || !strings.Contains(fmt.Sprint(out["note"]), "refused branch_protection and rulesets") {
			t.Fatalf("checks tool: %v", out)
		}
		e.finish(a, nil)
	})

	t.Run("unverifiable without check runs", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("Actions only", "", "bug")
		e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RequireChecks("main", "unit-tests")
		e.fake.SetCheck("h1", "CI", "completed", "success")
		e.fake.RefuseCheckRuns(true)
		e.sync()
		e.clock.Add(11 * time.Minute)
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksPassed || e.countType(gh.TypeChecksMissing) != 0 {
			t.Fatalf("workflow names never match required job names, so nothing may be reported missing: %v", q)
		}
	})
}
