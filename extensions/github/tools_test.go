package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/storage"
)

// Ported from Open Supervisor's core wait tests and toolbelt
// TestHoldForChecksBeforeAnyCall: the agent-facing syntax only.
func TestParseCondAndHoldMarkers(t *testing.T) {
	for in, want := range map[string]string{
		"pr:12": "pr:12:merged", "pr:#12:closed": "pr:12:closed", "issue:7": "issue:7:closed", "issue:7:review": "issue:7:review",
		"job:backup": "job:backup", "pr:club/88:merged": "pr:club/88:merged", "pr:5:commented": "pr:5:commented", "pr:5:Pushed": "pr:5:pushed",
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
	if c, _ := gh.ParseCond("pr:9:commented"); c.Describe() != "a reply or review on PR #9" || c.Happened(gh.TypeCommentHuman) != "someone replied on PR #9" {
		t.Fatal(c.Describe(), c.Happened(gh.TypeCommentHuman))
	}
	if _, err := gh.ParseCond("pr:9:reviewed"); err == nil || !strings.Contains(err.Error(), "merged, closed, commented, pushed, checks") {
		t.Fatal(err)
	}
	if c, _ := gh.ParseCond("pr:9:merged"); c.Happened(gh.TypePullClosed) != "PR #9 was closed without merging, so it won't merge" {
		t.Fatal(c.Happened(gh.TypePullClosed))
	}
	if c, err := gh.ParseCond("pr:137:checks"); err != nil || c.Describe() != "checks on PR #137" || c.Happened(gh.TypeChecksFailed) != "checks on PR #137 finished: failed" || c.Happened(gh.TypePullOpen) != "PR #137 got a new push before its checks finished; wait on pr:137:checks again for the new head's" {
		t.Fatal(c.Describe(), err)
	}
	// A Wait on a hold is described by its conditions, or by its time alone.
	due := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	c137, _ := gh.ParseCond("pr:137:checks")
	if d := (gh.Hold{Conds: []gh.Cond{c137}}).WaitDescription(); d != "checks on PR #137" {
		t.Fatal(d)
	}
	if d := (gh.Hold{Until: &due}).WaitDescription(); d != "until 25 Oct 2026 09:00 UTC" {
		t.Fatal(d)
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
	checks := filter(map[string]any{"kind": "pr_checks", "repo": repo, "number": 8, "head": "aaa"})
	if !gh.DependencyFilter(checks, ev(gh.SchemaChecks, gh.ChecksPayload{Repo: repo, Number: 8, SHA: "aaa", Complete: true})) {
		t.Fatal("checks on the waited head rejected")
	}
	if gh.DependencyFilter(checks, ev(gh.SchemaChecks, gh.ChecksPayload{Repo: repo, Number: 8, SHA: "bbb", Complete: true})) || gh.DependencyFilter(checks, ev(gh.SchemaChecks, gh.ChecksPayload{Repo: repo, Number: 7, SHA: "aaa"})) {
		t.Fatal("checks on another head or PR accepted")
	}
	turn := filter(map[string]any{"kind": "turn", "repo": repo, "number": 21, "state": "review"})
	ended := func(status string) domain.Event {
		e := ev(domain.SchemaTurnEnded, domain.TurnEnded{Status: status, Work: "work/21", Execution: "x/1"})
		e.Type = domain.TypeTurnEnded
		return e
	}
	if !gh.DependencyFilter(turn, ended("review")) || gh.DependencyFilter(turn, ended("building")) {
		t.Fatal("turn state filter")
	}
	peer := filter(map[string]any{"kind": "peer", "number": 5, "work": "work/1"})
	for _, c := range []struct {
		agent *gh.Agent
		edit  bool
		want  bool
	}{
		{&gh.Agent{Work: "work/9"}, false, true},
		{&gh.Agent{From: "#9"}, false, true},
		{&gh.Agent{Work: "work/1"}, false, false},
		{&gh.Agent{From: "5"}, false, false},
		{&gh.Agent{}, false, false},
		{nil, false, false},
		{&gh.Agent{Work: "work/9"}, true, false},
	} {
		if got := gh.DependencyFilter(peer, ev(gh.SchemaComment, gh.CommentPayload{Kind: gh.KindIssue, Body: "x", Agent: c.agent, Edited: c.edit})); got != c.want {
			t.Fatalf("peer filter on %+v (edited %v) = %v", c.agent, c.edit, got)
		}
	}
	// A push is a new head; any other change to the PR (a title, a label) is not.
	push := filter(map[string]any{"kind": "pr_push", "repo": repo, "number": 8, "head": "s1"})
	if !gh.DependencyFilter(push, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 8, HeadSHA: "s2"})) || gh.DependencyFilter(push, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 8, HeadSHA: "s1", Title: "Renamed"})) || gh.DependencyFilter(push, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 7, HeadSHA: "s2"})) {
		t.Fatal("push filter")
	}
	// A new comment, review or thread reply on the PR from anyone but the
	// waiting Work; not an edit, nor a comment on an issue of the same number.
	comment := filter(map[string]any{"kind": "pr_comment", "repo": repo, "number": 8, "work": "work/me"})
	for _, c := range []gh.CommentPayload{{Kind: gh.KindPullRequest}, {Kind: gh.KindReview}, {Kind: gh.KindReviewComment, InReplyTo: 4}, {Kind: gh.KindPullRequest, Agent: &gh.Agent{Work: "work/author"}}} {
		c.Repo, c.Number = repo, 8
		if !gh.DependencyFilter(comment, ev(gh.SchemaComment, c)) {
			t.Fatalf("comment filter rejected %+v", c)
		}
	}
	for _, c := range []gh.CommentPayload{{Kind: gh.KindPullRequest, Edited: true}, {Kind: gh.KindIssue}, {Kind: gh.KindPullRequest, Agent: &gh.Agent{Work: "work/me"}}} {
		c.Repo, c.Number = repo, 8
		if gh.DependencyFilter(comment, ev(gh.SchemaComment, c)) {
			t.Fatalf("comment filter accepted %+v", c)
		}
	}
	// The open issue seen without its scheduled label releases a hold; with
	// it, closed, or another issue does not.
	unscheduled := filter(map[string]any{"kind": "unscheduled", "repo": repo, "number": 3, "label": "scheduled"})
	issue := func(p gh.IssuePayload) domain.Event {
		e := ev(gh.SchemaIssue, p)
		e.Type = gh.TypeIssueOpen
		return e
	}
	if !gh.DependencyFilter(unscheduled, issue(gh.IssuePayload{Repo: repo, Number: 3, State: "open", Labels: []string{"bug"}})) {
		t.Fatal("unscheduled filter rejected the issue without its label")
	}
	for _, p := range []gh.IssuePayload{{Repo: repo, Number: 3, State: "open", Labels: []string{"Scheduled"}}, {Repo: repo, Number: 4, State: "open"}, {Repo: repo, Number: 3, State: "closed"}} {
		if gh.DependencyFilter(unscheduled, issue(p)) {
			t.Fatalf("unscheduled filter accepted %+v", p)
		}
	}
	if gh.DependencyFilter(domain.Payload{Schema: "other"}, ev(gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 8})) {
		t.Fatal("foreign filter schema accepted")
	}
}

func TestRouteHoldsAndToolInputs(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	hold := func(labels ...string) extension.RouteDecision {
		e := event(gh.TypeIssueOpen, "issue/3", gh.SchemaIssue, gh.IssuePayload{Repo: repo, Number: 3, State: "open", Labels: labels, AuthorAssociation: "OWNER"})
		d, err := gh.HoldRoute{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := hold("scheduled"); !d.Match || d.Kind != gh.WorkKindHeld {
		t.Fatalf("scheduled issue not held: %+v", d)
	}
	stranger := event(gh.TypeIssueOpen, "issue/3", gh.SchemaIssue, gh.IssuePayload{Repo: repo, Number: 3, State: "open", Labels: []string{"scheduled"}, AuthorAssociation: "NONE"})
	if d, _ := (gh.HoldRoute{}).Match(context.Background(), extension.RouteInput{Event: stranger, Scope: scope}); d.Match {
		t.Fatal("a stranger's scheduled issue was held")
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
	for typ, want := range map[string]bool{gh.TypeChecksNone: true, gh.TypeChecksMissing: true, gh.TypeChecksPending: false, domain.TypeTurnEnded: false, gh.TypeDependencyMet: false} {
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
		"### Comment on the issue by bob":                   ev(gh.TypeCommentHuman, gh.SchemaComment, gh.CommentPayload{Kind: gh.KindIssue, Number: 4, Author: "bob", CreatedAt: at}),
		"### Review on PR #4 (changes requested)":           ev(gh.TypeCommentHuman, gh.SchemaComment, gh.CommentPayload{Kind: gh.KindReview, Number: 4, State: "CHANGES_REQUESTED", CreatedAt: at}),
		"### Checks failed on PR #4 at abcdef1: ci":         ev(gh.TypeChecksFailed, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", Failed: []string{"ci"}}),
		"### Checks passed on PR #4 at abcdef1\nPreview: u": ev(gh.TypeChecksPassed, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", Preview: "u"}),
		"### No checks ran on PR #4":                        ev(gh.TypeChecksNone, gh.SchemaChecks, gh.ChecksPayload{Number: 4, SHA: "abcdef1234", None: true}),
		// v1 c37edf1 core TestRender: a required check that never reported.
		"### Required checks never reported on PR #5 at 995bdda: netlify/site/deploy-preview\nThe PR can't merge without them.":                                               ev(gh.TypeChecksMissing, gh.SchemaChecks, gh.ChecksPayload{Number: 5, SHA: "995bdda12", Missing: []string{"netlify/site/deploy-preview"}}),
		"### Checks failed on PR #6 at abcdef1: lint\nSee `call github.checks {\"number\": 6}` for links to each.\nThese required checks never reported either: deploy.":      ev(gh.TypeChecksFailed, gh.SchemaChecks, gh.ChecksPayload{Number: 6, SHA: "abcdef1234", Failed: []string{"lint"}, Missing: []string{"deploy"}}),
		"### PR #4 has conflicts with main at abcdef1\nIt can't merge until they're resolved: rebase the branch onto `main`":                                                  ev(gh.TypePullConflicts, gh.SchemaConflict, gh.ConflictsPayload{Number: 4, HeadSHA: "abcdef1234", BaseRef: "main"}),
		"### PR #4 is open at abcdef1\nOpened by bob at 2026-10-01T09:00:00Z\nTitle: t\nu\nThis comes when the PR opens and again whenever anything about it changes: a push": ev(gh.TypePullOpen, gh.SchemaPull, gh.PullPayload{Number: 4, State: "open", HeadSHA: "abcdef1234", Author: "bob", CreatedAt: at, Title: "t", URL: "u", Body: "b"}),
		"### PR #4 is open as a draft at abcdef1\n":          ev(gh.TypePullOpen, gh.SchemaPull, gh.PullPayload{Number: 4, State: "open", Draft: true, HeadSHA: "abcdef1234"}),
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
