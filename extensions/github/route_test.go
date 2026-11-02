package github_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

func scopeWith(t *testing.T, config string) domain.Scope {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "repo", "type": "github", "config": json.RawMessage(config), "secret_names": []string{}},
	}})
	return domain.Scope{ID: "eng", Name: "eng", Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
}

func event(typ, external string, schema string, p any) domain.AcceptedEvent {
	data, _ := json.Marshal(p)
	return domain.AcceptedEvent{Event: domain.Event{ID: "e", Type: typ, Source: ns, DeliveryID: "d",
		CorrelationKey: &domain.CorrelationKey{Source: ns, ExternalID: external}, Data: domain.Payload{Schema: schema, Data: data}}}
}

func match(t *testing.T, scope domain.Scope, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
	t.Helper()
	d, err := gh.Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: scope, Work: w})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRouteClaimAdmission(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","webhook_secret":"hook"}`)
	issue := func(labels ...string) gh.IssuePayload {
		return gh.IssuePayload{Repo: repo, Number: 1, State: "open", Labels: append([]string{}, labels...), Author: "alice", AuthorAssociation: "MEMBER"}
	}
	stranger := issue()
	stranger.Author, stranger.AuthorAssociation = "mallory", "NONE"
	unknown := issue()
	unknown.AuthorAssociation = ""
	contributor := issue()
	contributor.AuthorAssociation = "CONTRIBUTOR"
	agentFiled := issue()
	agentFiled.Author, agentFiled.AuthorType, agentFiled.AuthorAssociation, agentFiled.ByConnection = "openconduit-bot[bot]", "Bot", "NONE", true
	agentFiled.Body = "Follow-up.\n\n<!-- open-conduit:agent work=work/1 -->"
	// Another App's bot carrying a marker (release notes Dependabot copied
	// in, say) is not this connection's, so it is refused.
	otherBot := agentFiled
	otherBot.Author, otherBot.ByConnection = "dependabot[bot]", false
	forgedMarker := stranger
	forgedMarker.Body = agentFiled.Body
	for name, tc := range map[string]struct {
		p     gh.IssuePayload
		claim bool
	}{
		"plain":                    {issue(), true},
		"skip label":               {issue("backlog"), false},
		"human":                    {issue("bug", "human"), false},
		"claimed by another owner": {issue("agent-in-progress"), false},
		"scheduled waits for hold": {issue("scheduled"), false},
		"closed":                   {gh.IssuePayload{State: "closed"}, false},
		"owner":                    {func() gh.IssuePayload { p := issue(); p.AuthorAssociation = "OWNER"; return p }(), true},
		"collaborator":             {func() gh.IssuePayload { p := issue(); p.AuthorAssociation = "COLLABORATOR"; return p }(), true},
		"stranger":                 {stranger, false},
		"contributor":              {contributor, false},
		"unknown association":      {unknown, false},
		"an agent's follow-up":     {agentFiled, true},
		"another App's marked bot": {otherBot, false},
		"a stranger's marker":      {forgedMarker, false},
	} {
		d := match(t, scope, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, tc.p), nil)
		if d.Match != tc.claim || d.Match && d.Kind != gh.WorkKind {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	reopened := issue("agent-in-progress")
	reopened.Generation = "reopen-9"
	if d := match(t, scope, event(gh.TypeIssueOpen, "issue/1/generation/reopen-9", gh.SchemaIssue, reopened), nil); !d.Match {
		t.Fatal("a reopened generation carrying our old claim label must be claimable")
	}
	optIn := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","labels":{"require":["agent"]}}`)
	if match(t, optIn, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, issue()), nil).Match {
		t.Fatal("opt-in scope claimed an unlabelled issue")
	}
	if !match(t, optIn, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, issue("agent")), nil).Match {
		t.Fatal("opt-in label ignored")
	}
	d := match(t, scope, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, stranger), nil)
	if !strings.Contains(d.Reason, "mallory is NONE") || !strings.Contains(d.Reason, "admit.authors") {
		t.Fatalf("a refused author's reason doesn't say why: %q", d.Reason)
	}
	widened := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","admit":{"authors":["owner","member","collaborator","contributor"]}}`)
	if !match(t, widened, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, contributor), nil).Match || match(t, widened, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, stranger), nil).Match {
		t.Fatal("admit.authors did not widen to exactly the associations named")
	}
	anyone := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","admit":{"authors":["*"]}}`)
	if !match(t, anyone, event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, stranger), nil).Match {
		t.Fatal(`admit.authors "*" refused a stranger`)
	}
	// A claim by hand overrides the author rule, as it does the labels.
	claim := event(gh.TypeClaimRequested, "issue/1", "", struct{}{})
	claim.Event.Source = extension.ActionSource
	if !match(t, scope, claim, nil).Match {
		t.Fatal("a claim by hand was refused")
	}
	// Comments, PRs and checks never create Work.
	for _, typ := range []string{gh.TypeCommentHuman, gh.TypePullOpen, gh.TypeChecksFailed, gh.TypeIssueClosed} {
		if match(t, scope, event(typ, "issue/1", gh.SchemaComment, gh.CommentPayload{Body: "x"}), nil).Match {
			t.Fatalf("%s created Work", typ)
		}
	}
	// Events from another namespace or scope are not this handler's.
	foreign := event(gh.TypeIssueOpen, "issue/1", gh.SchemaIssue, issue())
	foreign.Event.Source = "github/github.com/other/repo"
	if match(t, scope, foreign, nil).Match {
		t.Fatal("foreign namespace matched")
	}
}

func TestRouteInputSelection(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	work := &domain.Work{ID: "work/1", Kind: gh.WorkKind, ScopeID: "eng", State: domain.WorkRunning,
		CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "issue/5"}}}
	comment := func(typ string, p gh.CommentPayload) bool {
		return match(t, scope, event(typ, "issue/5", gh.SchemaComment, p), work).Match
	}
	if !comment(gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindIssue, Body: "please change the colour"}) {
		t.Fatal("human comment not delivered")
	}
	if comment(gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindIssue, Body: "x", Edited: true}) {
		t.Fatal("edit delivered as new input")
	}
	if comment(gh.TypeCommentBot, gh.CommentPayload{Kind: gh.KindIssue, Body: "beep"}) {
		t.Fatal("unmarked bot comment delivered")
	}
	if comment(gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindIssue, Body: "my note", Agent: &gh.Agent{Work: "work/1"}}) {
		t.Fatal("own note handed back")
	}
	if comment(gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindIssue, Body: "v1 note", Agent: &gh.Agent{From: "5"}}) {
		t.Fatal("own v1-marked note handed back")
	}
	if !comment(gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindIssue, Body: "I'm doing the header", Agent: &gh.Agent{Work: "work/9"}}) {
		t.Fatal("another agent's comment not delivered")
	}
	if !comment(gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindIssue, Body: "v1 other", Agent: &gh.Agent{From: "9"}}) {
		t.Fatal("another v1 agent's comment not delivered")
	}
	if comment(gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindReview, State: "APPROVED"}) {
		t.Fatal("empty review delivered (v1 takes reviews with a body)")
	}
	if !comment(gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindReview, State: "CHANGES_REQUESTED", Body: "rename"}) {
		t.Fatal("review with body not delivered")
	}
	for typ, want := range map[string]bool{
		gh.TypeIssueClosed: true, gh.TypePullMerged: true, gh.TypePullClosed: true, gh.TypePullConflicts: true, gh.TypeChecksPassed: true, gh.TypeChecksFailed: true,
		gh.TypeIssueOpen: false, gh.TypePullOpen: false, gh.TypeChecksPending: false, gh.TypeDelivery: false,
	} {
		if got := match(t, scope, event(typ, "issue/5", "", struct{}{}), work).Match; got != want {
			t.Fatalf("%s on existing Work: match=%v want %v", typ, got, want)
		}
	}
}

// A GitHub issue's Work says what it is: the issue's title, number, author
// and link, then the link of a PR whose input reaches it.
func TestRouteDisplay(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","webhook_secret":"hook"}`)
	issue := gh.IssuePayload{Repo: repo, Number: 136, State: "open", Title: "TUI: show a held Work's reason in the list\nmore", Author: "example-user", AuthorAssociation: "OWNER", URL: "https://github.com/" + repo + "/issues/136"}
	d := match(t, scope, event(gh.TypeIssueOpen, "issue/136", gh.SchemaIssue, issue), nil)
	want := domain.WorkDisplay{Title: "TUI: show a held Work's reason in the list", Ref: "#136", Requester: "example-user", Links: []domain.Link{{Label: "Issue #136", URL: "https://github.com/" + repo + "/issues/136"}}}
	if d.Display == nil || !reflect.DeepEqual(*d.Display, want) {
		t.Fatalf("display %+v", d.Display)
	}
	if err := extension.ValidateDisplay(d.Display); err != nil {
		t.Fatal(err)
	}
	w := &domain.Work{ID: "work/136", Kind: gh.WorkKind, State: domain.WorkReady, Display: d.Display}
	d = match(t, scope, event(gh.TypeChecksPassed, "issue/136", gh.SchemaChecks, gh.ChecksPayload{Repo: repo, Number: 137, Complete: true}), w)
	if !d.Match || d.Display == nil || len(d.Display.Links) != 2 || d.Display.Links[1] != (domain.Link{Label: "PR #137", URL: "https://github.com/" + repo + "/pull/137"}) {
		t.Fatalf("checks on PR #137: %+v", d.Display)
	}
	w.Display = d.Display
	if d = match(t, scope, event(gh.TypePullMerged, "issue/136", gh.SchemaPull, gh.PullPayload{Repo: repo, Number: 137, URL: "https://github.com/" + repo + "/pull/137"}), w); !d.Match || d.Display != nil {
		t.Fatalf("a PR already linked changed the display: %+v", d.Display)
	}
}

func matchRoute(t *testing.T, scope domain.Scope, config string, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
	t.Helper()
	d, err := gh.Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: scope, Work: w,
		Route: domain.Route{Name: "pulls", Handler: "github", Binding: "reviewer", Config: domain.Payload{Data: json.RawMessage(config)}}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseRoute(t *testing.T) {
	for _, raw := range []string{``, `{}`, `null`, `{"claim":"issues"}`, `{"claim":"pull_requests"}`, `{"claim":"pull_requests","drafts":true}`} {
		if err := gh.ValidateRoute(json.RawMessage(raw)); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
	}
	for _, raw := range []string{`{"claim":"prs"}`, `{"claim":"issues","drafts":true}`, `{"label":"x"}`} {
		if gh.ValidateRoute(json.RawMessage(raw)) == nil {
			t.Fatalf("%q validated", raw)
		}
	}
}

// A pull_requests route claims each open PR's copy as the PR's own Work,
// whether or not an issue's Work owns the PR's issue key, and hands it only
// the copies' news; an issues route never touches the PR's Work.
func TestRouteClaimsPullRequests(t *testing.T) {
	cfg := `{"namespace":"` + ns + `","scope":"eng","repo":"` + repo + `","track_pull_requests":true}`
	scope := scopeWith(t, cfg)
	const pulls = `{"claim":"pull_requests"}`
	pull := gh.PullPayload{Repo: repo, Number: 7, State: "open", Title: "Make the header blue", Author: "alice", AuthorAssociation: "COLLABORATOR", URL: "https://github.com/" + repo + "/pull/7", HeadSHA: "a1"}
	open := event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, pull)

	d := matchRoute(t, scope, pulls, open, nil)
	if !d.Match || d.Kind != gh.WorkKindPull {
		t.Fatalf("open PR not claimed: %+v", d)
	}
	want := domain.WorkDisplay{Title: "Make the header blue", Ref: "#7", Requester: "alice", Links: []domain.Link{{Label: "PR #7", URL: pull.URL}}}
	if d.Display == nil || !reflect.DeepEqual(*d.Display, want) {
		t.Fatalf("PR display %+v", d.Display)
	}
	// A fork's PR, or a stranger's, starts nothing unless admit widens it.
	fork := pull
	fork.Fork = true
	stranger := pull
	stranger.Author, stranger.AuthorAssociation = "mallory", "FIRST_TIME_CONTRIBUTOR"
	for name, p := range map[string]gh.PullPayload{"fork": fork, "stranger": stranger} {
		if d := matchRoute(t, scope, pulls, event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, p), nil); d.Match || d.Reason == "" {
			t.Fatalf("%s's PR claimed: %+v", name, d)
		}
	}
	widened := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","track_pull_requests":true,"admit":{"forks":true}}`)
	if !matchRoute(t, widened, pulls, event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, fork), nil).Match {
		t.Fatal("admit.forks refused a collaborator's fork")
	}
	if matchRoute(t, widened, pulls, event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, stranger), nil).Match {
		t.Fatal("admit.forks admitted a stranger")
	}
	// The issues route leaves the copy alone, and the pull_requests route
	// the PR's original observation (keyed to its issue or to pr/N).
	if match(t, scope, open, nil).Match {
		t.Fatal("an issues route claimed a PR's copy")
	}
	for _, key := range []string{"issue/3", "pr/7"} {
		if matchRoute(t, scope, pulls, event(gh.TypePullOpen, key, gh.SchemaPull, pull), nil).Match {
			t.Fatalf("a pull_requests route claimed the original under %s", key)
		}
	}
	draft := pull
	draft.Draft = true
	if matchRoute(t, scope, pulls, event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, draft), nil).Match {
		t.Fatal("a draft was claimed by default")
	}
	if !matchRoute(t, scope, `{"claim":"pull_requests","drafts":true}`, event(gh.TypePullOpen, gh.PullRequestKey(7), gh.SchemaPull, draft), nil).Match {
		t.Fatal("drafts: true did not claim a draft")
	}
	closed := pull
	closed.State = "closed"
	if matchRoute(t, scope, pulls, event(gh.TypePullClosed, gh.PullRequestKey(7), gh.SchemaPull, closed), nil).Match {
		t.Fatal("a closed PR started Work")
	}
	if matchRoute(t, scope, pulls, event(gh.TypeCommentHuman, gh.PullRequestKey(7), gh.SchemaComment, gh.CommentPayload{Kind: gh.KindPullRequest, Number: 7, Body: "x"}), nil).Match {
		t.Fatal("a comment started Work")
	}
	// Without track_pull_requests the entry observes no copies, and
	// the route says so in a trace.
	plain := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	if d := matchRoute(t, plain, pulls, open, nil); d.Match || !strings.Contains(d.Reason, "track_pull_requests") {
		t.Fatalf("claimed without track_pull_requests: %+v", d)
	}

	own := &domain.Work{ID: "work/r7", Kind: gh.WorkKindPull, ScopeID: "eng", State: domain.WorkRunning,
		CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: gh.PullRequestKey(7)}}}
	in := func(typ string, p any) bool {
		schema := gh.SchemaPull
		if _, ok := p.(gh.CommentPayload); ok {
			schema = gh.SchemaComment
		}
		e := event(typ, gh.PullRequestKey(7), schema, p)
		if match(t, scope, e, own).Match {
			t.Fatalf("an issues route took %s for a PR's Work", typ)
		}
		return matchRoute(t, scope, pulls, e, own).Match
	}
	pushed := pull
	pushed.HeadSHA = "b2"
	merged := pull
	merged.State, merged.Merged = "closed", true
	for name, tc := range map[string]struct {
		typ  string
		p    any
		want bool
	}{
		"push":               {gh.TypePullOpen, pushed, true},
		"merge":              {gh.TypePullMerged, merged, true},
		"close":              {gh.TypePullClosed, closed, true},
		"human reply":        {gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindReviewComment, Number: 7, Body: "done, see a1b2"}, true},
		"builder's reply":    {gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindReviewComment, Number: 7, Body: "fixed", Agent: &gh.Agent{Work: "work/3"}}, true},
		"own review":         {gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindReview, Number: 7, Body: "please rename", Agent: &gh.Agent{Work: "work/r7"}}, false},
		"own inline comment": {gh.TypeCommentAgent, gh.CommentPayload{Kind: gh.KindReviewComment, Number: 7, Body: "here", Agent: &gh.Agent{Work: "work/r7"}}, false},
		"edit":               {gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindPullRequest, Number: 7, Body: "x", Edited: true}, false},
		"empty review":       {gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindReview, Number: 7, State: "APPROVED"}, false},
		"unmarked bot":       {gh.TypeCommentBot, gh.CommentPayload{Kind: gh.KindPullRequest, Number: 7, Body: "deployed"}, false},
		"conversation":       {gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindPullRequest, Number: 7, Body: "looks close"}, true},
		"review with a body": {gh.TypeCommentHuman, gh.CommentPayload{Kind: gh.KindReview, Number: 7, State: "COMMENTED", Body: "one more thing"}, true},
		"checks passed":      {gh.TypeChecksPassed, pull, true},
		"checks failed":      {gh.TypeChecksFailed, pull, true},
		"checks pending":     {gh.TypeChecksPending, pull, false},
		"conflicts":          {gh.TypePullConflicts, pull, true},
	} {
		if got := in(tc.typ, tc.p); got != tc.want {
			t.Fatalf("%s: match=%v want %v", name, got, tc.want)
		}
	}
	// A pull_requests route never feeds an issue's Work.
	builder := &domain.Work{ID: "work/3", Kind: gh.WorkKind, ScopeID: "eng", State: domain.WorkRunning,
		CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "issue/3"}}}
	if matchRoute(t, scope, pulls, event(gh.TypePullMerged, "issue/3", gh.SchemaPull, merged), builder).Match {
		t.Fatal("a pull_requests route fed an issue's Work")
	}
}
