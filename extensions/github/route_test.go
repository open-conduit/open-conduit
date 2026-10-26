package github_test

import (
	"context"
	"encoding/json"
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
		return gh.IssuePayload{Repo: repo, Number: 1, State: "open", Labels: append([]string{}, labels...)}
	}
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
		gh.TypeIssueClosed: true, gh.TypePullMerged: true, gh.TypePullClosed: true, gh.TypeChecksPassed: true, gh.TypeChecksFailed: true,
		gh.TypeIssueOpen: false, gh.TypePullOpen: false, gh.TypeChecksPending: false, gh.TypeDelivery: false,
	} {
		if got := match(t, scope, event(typ, "issue/5", "", struct{}{}), work).Match; got != want {
			t.Fatalf("%s on existing Work: match=%v want %v", typ, got, want)
		}
	}
}
