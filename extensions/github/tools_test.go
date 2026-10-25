package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/storage"
)

// Ported from Open Supervisor's core wait tests and toolbelt
// TestHoldForChecksBeforeAnyCall: the agent-facing syntax only.
func TestParseCondAndHoldMarkers(t *testing.T) {
	for in, want := range map[string]string{
		"pr:12": "pr:12:merged", "pr:#12:closed": "pr:12:closed", "issue:7": "issue:7:closed", "issue:7:review": "issue:7:review",
		"job:backup": "job:backup", "pr:club/88:merged": "pr:club/88:merged",
	} {
		c, err := gh.ParseCond(in)
		if err != nil || c.String() != want {
			t.Fatalf("%s: %v %v", in, c, err)
		}
	}
	for in, want := range map[string]string{
		"pr:abc": "isn't a PR number", "job:backup:ok": "next run to end", "issue:3:sleeping": "can be waited on", "bug:1": "waits are on",
		"pr:/3": "<project>/<ref>", "pr": "isn't a condition",
	} {
		if _, err := gh.ParseCond(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", in, err, want)
		}
	}
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	h := gh.Hold{Conds: []gh.Cond{{Kind: gh.CondPR, Ref: "1257", On: "merged"}, {Kind: gh.CondIssue, Ref: "12", On: "review"}}, Until: &at}
	if h.Marker() != "<!-- waitFor: pr:1257:merged issue:12:review; until 2026-10-25T09:00Z -->" {
		t.Fatalf("marker %s", h.Marker())
	}
	if h.Describe() != "PR #1257 to merge, or the agent on #12 to end a turn in review, or 25 Oct 2026 09:00 UTC" {
		t.Fatalf("describe %s", h.Describe())
	}
	// The last valid marker wins; both runtimes read and write them.
	body := "a\n<!-- runAt: 2026-10-25T09:00Z -->\nb [waitFor: pr:3] <!-- waitFor: nonsense -->"
	got, ok := gh.HoldIn(body)
	if !ok || len(got.Conds) != 1 || got.Conds[0].String() != "pr:3:merged" || got.Until != nil {
		t.Fatalf("hold %+v %v", got, ok)
	}
	if got, ok := gh.HoldIn("<!-- runAt: 2026-10-25T09:00Z -->"); !ok || !got.Until.Equal(at) {
		t.Fatalf("runAt %+v", got)
	}
	if got, ok := gh.HoldIn("never mind\n<!-- waitFor: cancel -->"); !ok || !got.Cancel {
		t.Fatalf("cancel %+v", got)
	}
	if _, ok := gh.HoldIn("<!-- waitFor: -->"); ok {
		t.Fatal("empty hold parsed")
	}
	if c, _ := gh.ParseCond("pr:9:merged"); c.Happened(gh.TypePullClosed) != "PR #9 was closed without merging, so it won't merge" {
		t.Fatal(c.Happened(gh.TypePullClosed))
	}
}

func TestDependencyFilterSelectsTheNamedItem(t *testing.T) {
	filter := func(f map[string]any) domain.Payload {
		data, _ := json.Marshal(f)
		return domain.Payload{Schema: gh.SchemaFilter, Data: data}
	}
	ev := func(schema string, p any) domain.Event {
		data, _ := json.Marshal(p)
		return domain.Event{Data: domain.Payload{Schema: schema, Data: data}}
	}
	pr := filter(map[string]any{"kind": "pr", "repo": repo, "number": 8})
	if !gh.DependencyFilter(pr, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 8})) {
		t.Fatal("named PR rejected")
	}
	if gh.DependencyFilter(pr, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 7})) || gh.DependencyFilter(pr, ev(gh.SchemaPull, gh.PullPayload{Repo: "other/repo", Number: 8})) {
		t.Fatal("another PR on the same key accepted")
	}
	turn := filter(map[string]any{"kind": "turn", "repo": repo, "number": 21, "state": "review"})
	if !gh.DependencyFilter(turn, ev(gh.SchemaTurn, gh.TurnPayload{Repo: repo, Number: 21, State: "review"})) || gh.DependencyFilter(turn, ev(gh.SchemaTurn, gh.TurnPayload{Repo: repo, Number: 21, State: "building"})) {
		t.Fatal("turn state filter")
	}
	if gh.DependencyFilter(domain.Payload{Schema: "other"}, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 8})) {
		t.Fatal("foreign filter schema accepted")
	}
}

func TestRouteHoldsAndToolInputs(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	hold := func(labels ...string) extension.RouteDecision {
		e := event(gh.TypeIssueOpen, "issue/3", gh.SchemaIssue, gh.IssuePayload{Repo: repo, Number: 3, State: "open", Labels: labels})
		d, err := gh.HoldRoute{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := hold("scheduled"); !d.Match || d.Kind != gh.WorkKindHeld {
		t.Fatalf("scheduled issue not held: %+v", d)
	}
	if hold().Match || hold("scheduled", "backlog").Match || hold("scheduled", "agent-in-progress").Match {
		t.Fatal("hold route ignored admission rules")
	}
	if d := match(t, scope, event(gh.TypeIssueOpen, "issue/3", gh.SchemaIssue, gh.IssuePayload{Repo: repo, Number: 3, State: "open", Labels: []string{"scheduled"}}), nil); d.Match {
		t.Fatal("github route claimed a scheduled issue")
	}
	work := &domain.Work{ID: "work/3", Kind: gh.WorkKindHeld, ScopeID: "eng", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "issue/3"}}}
	if d, _ := (gh.HoldRoute{}).Match(context.Background(), extension.RouteInput{Event: event(gh.TypeCommentHuman, "issue/3", gh.SchemaComment, gh.CommentPayload{Body: "x"}), Scope: scope, Work: work}); d.Match {
		t.Fatal("hold route matched existing Work")
	}
	released := event(gh.TypeHoldReleased, "issue/3", gh.SchemaHoldReleased, gh.ReleasedPayload{Why: "now"})
	released.Event.Source, released.Event.WorkID = "execution/x", work.ID
	if !match(t, scope, released, work).Match {
		t.Fatal("hold release not queued for the agent")
	}
	released.Event.WorkID = "work/other"
	if match(t, scope, released, work).Match {
		t.Fatal("another Work's release accepted")
	}
	for typ, want := range map[string]bool{gh.TypeChecksNone: true, gh.TypeAgentTurn: false, gh.TypeDependencyMet: false} {
		if got := match(t, scope, event(typ, "issue/3", "", struct{}{}), work).Match; got != want {
			t.Fatalf("%s: match=%v want %v", typ, got, want)
		}
	}
}

func TestRenderEventsWithReplyHints(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ev := func(typ, schema string, p any) domain.Event {
		data, _ := json.Marshal(p)
		return domain.Event{Type: typ, Data: domain.Payload{Schema: schema, Data: data}}
	}
	cases := map[string]domain.Event{
		"### Inline comment on PR #4 at a.go:9 by bob (2026-10-01T09:00:00Z; reply with `call github.pr.reply {\"number\": 4, \"comment_id\": 77, \"body\": …}`)": ev(gh.TypeCommentHuman, gh.SchemaComment, gh.CommentPayload{Kind: gh.KindReviewComment, Number: 4, ID: 77, Path: "a.go", Line: 9, Author: "bob", CreatedAt: at, Body: "x"}),
		"### Comment on the issue by bob":                    ev(gh.TypeCommentHuman, gh.SchemaComment, gh.CommentPayload{Kind: gh.KindIssue, Number: 4, Author: "bob", CreatedAt: at}),
		"### Review on PR #4 (changes requested)":            ev(gh.TypeCommentHuman, gh.SchemaComment, gh.CommentPayload{Kind: gh.KindReview, Number: 4, State: "CHANGES_REQUESTED", CreatedAt: at}),
		"### Checks failed on PR #4 at abcdef1: ci":          ev(gh.TypeChecksFailed, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", Failed: []string{"ci"}}),
		"### Checks passed on PR #4 at abcdef1\nPreview: u":  ev(gh.TypeChecksPassed, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", Preview: "u"}),
		"### No checks ran on PR #4":                         ev(gh.TypeChecksNone, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", None: true}),
		"### PR #4 merged":                                   ev(gh.TypePullMerged, gh.SchemaPull, gh.PullPayload{Number: 4}),
		"### What you were waiting for\nThis issue was held": ev(gh.TypeHoldReleased, gh.SchemaHoldReleased, gh.ReleasedPayload{Why: "This issue was held."}),
		"### What you were waiting for\nYou were waiting for PR #9 to merge. That has happened: PR #9 was closed without merging": ev(gh.TypeDependencyMet, "core.subscription.alias.v1", map[string]string{"label": "pr:9:merged", "source_type": gh.TypePullClosed}),
		"### What you were waiting for\nIt is now past": {Type: "core.wait.timeout"},
	}
	for want, e := range cases {
		got, ok := gh.RenderEvent(e)
		if !ok || !strings.HasPrefix(got, want) {
			t.Errorf("render %s:\n got %q\nwant prefix %q", e.Type, got, want)
		}
	}
	if _, ok := gh.RenderEvent(domain.Event{Type: "manual.note"}); ok {
		t.Fatal("rendered an Event the extension does not own")
	}
}

func TestToolsRegistration(t *testing.T) {
	tools, err := gh.NewTools(gh.ToolsOptions{Store: nopStore{}, Sources: func() []gh.Source { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := tools.Registration()
	if r.Version != extension.ContractVersion || r.Name != gh.Type || r.DependencyFilter == nil || len(r.EventTypes) != 1 || r.EventTypes[0] != gh.TypeHoldReleased {
		t.Fatalf("registration %+v", r)
	}
	names := map[string]bool{}
	for _, op := range r.Operations {
		if (op.Handler == nil) == (op.Wait == nil) {
			t.Fatalf("%s: exactly one of Handler and Wait", op.Name)
		}
		names[op.Name] = true
	}
	for _, op := range gh.Operations() {
		if !names[op] {
			t.Fatalf("%s listed but not registered", op)
		}
	}
	if len(names) != len(gh.Operations()) {
		t.Fatal("registered operations not listed")
	}
}

type nopStore struct{}

func (nopStore) Transact(context.Context, storage.Guard, func(storage.Tx) error) error { return nil }
