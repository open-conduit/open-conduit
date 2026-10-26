package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/jira/jiratest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

const entry = `{"namespace":"jira/acme/proj","scope":"proj","projects":["PROJ"],"webhook_secret":"hook","fields":["customfield_10010"],"transitions":{"claimed":"In Progress","review":"In Review"}}`

func newClient(t *testing.T, f *jiratest.Fake, cfg string) (*jira.Client, jira.Settings) {
	t.Helper()
	conn, err := jira.ParseConnection([]byte(`{"base_url":"` + f.Server.URL + `/","email":"me@example.com","agent_account":"acc-robot"}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := jira.ParseExtension([]byte(cfg), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := jira.NewClient(conn, s, func(context.Context, string) (string, error) { return "tok", nil }, jira.ClientOptions{Now: f.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestWikiConversion(t *testing.T) {
	md := "## Done\n\nFixed **the bug** in `main.go`, see [the PR](https://x/pr/1).\n\n- one\n  - nested\n1. first\n\n```go\nfmt.Println(\"**not bold**\")\n```\n![shot](https://img/a.png)"
	wiki := jira.ToWiki(md)
	for _, want := range []string{"h2. Done", "*the bug*", "{{main.go}}", "[the PR|https://x/pr/1]", "* one", "** nested", "# first", "{code:go}", "fmt.Println(\"**not bold**\")", "{code}", "!https://img/a.png!"} {
		if !strings.Contains(wiki, want) {
			t.Errorf("wiki lacks %q:\n%s", want, wiki)
		}
	}
	back := jira.ToMarkdown(wiki)
	for _, want := range []string{"## Done", "**the bug**", "`main.go`", "[the PR](https://x/pr/1)", "- one", "  - nested", "1. first", "```go", "```"} {
		if !strings.Contains(back, want) {
			t.Errorf("markdown lacks %q:\n%s", want, back)
		}
	}
	if got := jira.ToMarkdown("*a* and *b* but not 2*3*4"); got != "**a** and **b** but not 2*3*4" {
		t.Errorf("bold: %q", got)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{`{}`, `{"base_url":"https://x"}`, `{"base_url":"https://x","auth":"oauth"}`, `{"base_url":"ftp://x","auth":"bearer"}`} {
		if jira.ValidateConnection(json.RawMessage(bad)) == nil {
			t.Errorf("connection %s accepted", bad)
		}
	}
	if jira.ValidateConnection(json.RawMessage(`{"base_url":"https://dc.example","auth":"bearer"}`)) != nil {
		t.Error("bearer needs no email")
	}
	for _, bad := range []string{
		`{"namespace":"jira/x","scope":"proj","projects":["bad key"]}`,
		`{"namespace":"jira/x","scope":"proj","projects":["PROJ"],"labels":{"claimed":"in progress"}}`,
		`{"namespace":"jira/x","scope":"proj","projects":["PROJ"],"fields":["no spaces allowed"]}`,
		`{"namespace":"jira/x","scope":"proj","projects":["PROJ"],"jql":"a\nb"}`,
	} {
		if jira.ValidateExtension(json.RawMessage(bad), nil) == nil {
			t.Errorf("entry %s accepted", bad)
		}
	}
}

func TestListPaginationFallbackAndRouteAttrs(t *testing.T) {
	f := jiratest.New("me@example.com", "tok")
	defer f.Close()
	for i, s := range []string{"To Do", "In Progress", "Done"} {
		f.AddIssue("PROJ-"+string(rune('1'+i)), "issue", s, "web")
	}
	f.AddIssue("WEB-1", "another project", "To Do")
	f.Issues["PROJ-1"].Custom["customfield_10010"] = map[string]any{"value": "Team Rocket"}
	c, s := newClient(t, f, entry)
	cs := s
	_ = cs
	list := func(c *jira.Client) []tracker.Issue {
		var all []tracker.Issue
		cursor := ""
		for {
			page, err := c.ListIssues(context.Background(), f.Now().Add(-time.Hour), cursor)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, page.Issues...)
			if page.Next == "" {
				return all
			}
			cursor = page.Next
		}
	}
	got := list(c)
	if len(got) != 3 || got[0].Key != "PROJ-1" || got[2].Open {
		t.Fatalf("enhanced search: %+v", got)
	}
	i := got[0]
	if i.Status != "To Do" || i.Body != "Fix **this**" || i.Attrs["customfield_10010"][0] != "Team Rocket" || i.Attrs["components"][0] != "Web" || i.Attrs["project"][0] != "PROJ" {
		t.Fatalf("attrs: %+v", i)
	}
	f.NoEnhancedSearch = true
	if got := list(c); len(got) != 3 || f.Calls["search"] == 0 {
		t.Fatalf("classic fallback: %+v %v", got, f.Calls)
	}
	several, _ := newClient(t, f, `{"namespace":"jira/acme/x","scope":"proj","projects":["PROJ","WEB"]}`)
	if got := list(several); len(got) != 4 || several.ProjectJQL() != `project in ("PROJ", "WEB")` || !several.Owns("web-1") {
		t.Fatalf("several projects: %d %s", len(got), several.ProjectJQL())
	}
	if refs := c.RefsIn("fixes proj-12 and PROJ-3, not XPROJ-4"); strings.Join(refs, ",") != "PROJ-12,PROJ-3" || c.RefFor("PROJ-2") != "PROJ-2" {
		t.Fatalf("refs %v", refs)
	}
}

func TestCommentsAndMarks(t *testing.T) {
	f := jiratest.New("me@example.com", "tok")
	defer f.Close()
	f.AddIssue("PROJ-1", "x", "To Do")
	f.AddComment("PROJ-1", "Ada", "please *add* tests")
	robot := f.AddComment("PROJ-1", "Robot", "by the agent account")
	_ = robot
	app := f.AddComment("PROJ-1", "CI", "deployed")
	app.App = true
	c, _ := newClient(t, f, entry)
	ctx := context.Background()
	posted, err := c.PostComment(ctx, "PROJ-1", "**Done**", tracker.Mark{Work: "w-1", Intent: "iabc", From: "PROJ-1"})
	if err != nil || posted.ID == "" {
		t.Fatal(posted, err)
	}
	comments, err := c.Comments(ctx, "PROJ-1")
	if err != nil || len(comments) != 4 {
		t.Fatal(comments, err)
	}
	if comments[0].Agent != nil || comments[0].Body != "please **add** tests" {
		t.Fatalf("human: %+v", comments[0])
	}
	if comments[1].Agent == nil {
		t.Fatal("the agent account's comment isn't marked")
	}
	if !comments[2].Bot {
		t.Fatal("an app's comment is a bot's")
	}
	a := comments[3].Agent
	if a == nil || a.Work != "w-1" || a.Intent != "iabc" || a.From != "PROJ-1" || comments[3].Body != "**Done**" {
		t.Fatalf("marked: %+v %+v", comments[3], a)
	}
	_, _, raw := f.Snapshot("PROJ-1")
	var v map[string]any
	_ = json.Unmarshal(raw[3].Properties[jira.MarkProperty], &v)
	if v["agent"] != true || v["from"] != "PROJ-1" || raw[3].Body != "*Done*" {
		t.Fatalf("Open Supervisor reads agent and from: %v %q", v, raw[3].Body)
	}
	key, one, err := c.ReadComment(ctx, tracker.CommentRef{Issue: "PROJ-1", ID: posted.ID})
	if err != nil || key != "PROJ-1" || one.Agent == nil || one.Agent.Intent != "iabc" {
		t.Fatalf("read back with properties: %v %+v", err, one)
	}
}

func TestLabelsTransitionAndReopens(t *testing.T) {
	f := jiratest.New("me@example.com", "tok")
	defer f.Close()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return now }
	f.AddIssue("PROJ-1", "x", "To Do", "web", "agent-needs-help")
	c, _ := newClient(t, f, entry)
	ctx := context.Background()
	if err := c.EditLabels(ctx, "PROJ-1", []string{"ready-for-review"}, []string{"agent-needs-help"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditLabels(ctx, "PROJ-1", []string{"has space"}, nil); !errors.Is(err, jira.ErrLabelSpace) {
		t.Fatalf("space: %v", err)
	}
	if err := c.Transition(ctx, "PROJ-1", "in review"); err != nil {
		t.Fatal(err)
	}
	labels, status, _ := f.Snapshot("PROJ-1")
	if strings.Join(labels, ",") != "ready-for-review,web" || status != "In Review" {
		t.Fatalf("%v %s", labels, status)
	}
	if err := c.Transition(ctx, "PROJ-1", "Move to Done"); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(ctx, "PROJ-1", "Nowhere"); !errors.Is(err, jira.ErrNoTransition) {
		t.Fatalf("no transition: %v", err)
	}
	now = now.Add(time.Hour)
	f.Move("PROJ-1", "To Do")
	reopens, err := c.Reopens(ctx, "PROJ-1")
	if err != nil || len(reopens) != 1 || !reopens[0].Equal(now) {
		t.Fatalf("reopens: %v %v", reopens, err)
	}
}

func signed(event, issue, comment string) (http.Header, []byte) {
	p := map[string]any{"webhookEvent": event, "issue": map[string]any{"key": issue}}
	if comment != "" {
		p["comment"] = map[string]any{"id": comment}
	}
	body, _ := json.Marshal(p)
	h := http.Header{}
	h.Set("X-Hub-Signature", "sha256="+tracker.SignHMAC("s3cret", body))
	h.Set("X-Atlassian-Webhook-Identifier", "wh-"+tracker.Digest(string(body))[:8])
	return h, body
}

func TestParseHookAndEnsureWebhook(t *testing.T) {
	f := jiratest.New("me@example.com", "tok")
	defer f.Close()
	c, _ := newClient(t, f, entry)
	h, body := signed("jira:issue_updated", "PROJ-5", "")
	hook, err := c.ParseHook(h, body, "s3cret", time.Now())
	if err != nil || len(hook.Issues) != 1 || hook.Issues[0] != "PROJ-5" || !strings.HasPrefix(hook.ID, "wh-") {
		t.Fatalf("issue: %+v %v", hook, err)
	}
	h, body = signed("comment_created", "PROJ-5", "10001")
	if hook, _ := c.ParseHook(h, body, "s3cret", time.Now()); len(hook.Comments) != 1 || hook.Comments[0].Issue != "PROJ-5" {
		t.Fatalf("comment: %+v", hook)
	}
	h, body = signed("jira:issue_created", "WEB-1", "")
	if hook, _ := c.ParseHook(h, body, "s3cret", time.Now()); len(hook.Issues) != 0 {
		t.Fatalf("another project: %+v", hook)
	}
	h.Del("X-Hub-Signature")
	if _, err := c.ParseHook(h, body, "s3cret", time.Now()); !errors.Is(err, tracker.ErrSignature) {
		t.Fatal("an unsigned delivery was taken")
	}
	id, err := c.EnsureWebhook(context.Background(), "https://example.test/jira", "s3cret", "")
	if err != nil || id == "" || f.Webhooks[id]["url"] != "https://example.test/jira" {
		t.Fatal(id, err)
	}
	if again, err := c.EnsureWebhook(context.Background(), "https://example.test/new", "s3cret", id); err != nil || again != id {
		t.Fatal(again, err)
	}
	f.NotAdmin = true
	if _, err := c.EnsureWebhook(context.Background(), "https://example.test/new", "s3cret", ""); !errors.Is(err, jira.ErrHookPermission) {
		t.Fatalf("needs admin: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	f := jiratest.New("me@example.com", "tok")
	defer f.Close()
	f.AddIssue("PROJ-1", "x", "To Do")
	c, _ := newClient(t, f, entry)
	f.TooMany = 1
	var limit *tracker.RateLimitError
	if _, err := c.Issue(context.Background(), "PROJ-1"); !errors.As(err, &limit) {
		t.Fatalf("429: %v", err)
	}
	if _, err := c.Issue(context.Background(), "PROJ-1"); !errors.As(err, &limit) {
		t.Fatalf("paused: %v", err)
	}
}

func TestIngressConformance(t *testing.T) {
	conformance.RunIngress(t, func() conformance.IngressFixture {
		f := jiratest.New("me@example.com", "tok")
		t.Cleanup(f.Close)
		fixed := time.Now().UTC()
		f.Now = func() time.Time { return fixed }
		f.AddIssue("PROJ-1", "first", "To Do")
		f.AddIssue("PROJ-2", "second", "To Do")
		f.AddComment("PROJ-2", "Ada", "please")
		c, s := newClient(t, f, entry)
		in := tracker.NewIngress(s.Settings, c, f.Now, tracker.IngressOptions{BatchUnits: 2})
		src := extension.Source{Namespace: "jira/acme/proj", ScopeID: "proj"}
		h, body := signed("jira:issue_updated", "PROJ-1", "")
		d, err := tracker.VerifyDelivery(jira.Type, c, "s3cret", h, body, f.Now())
		if err != nil {
			t.Fatal(err)
		}
		snap, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
		if err != nil || len(snap.Observations) == 0 {
			t.Fatal(snap, err)
		}
		return conformance.IngressFixture{Ingress: in, Source: src, Delivery: d, Canonical: snap.Observations[0], OneShotType: "jira.delivery"}
	})
}
