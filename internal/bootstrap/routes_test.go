package bootstrap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func TestManualRouteSamples(t *testing.T) {
	samples := manualRoute{}.Samples(context.Background(), extension.SampleInput{Scope: domain.Scope{}})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	d, err := manualRoute{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}})
	if err != nil || !d.Match || d.Kind != "manual.work" {
		t.Fatalf("the keyed sample must create manual work: %+v, %v", d, err)
	}
}

type familyIngress struct{ families []string }

func (f familyIngress) Spec() extension.IngressSpec {
	var spec extension.IngressSpec
	for _, t := range f.families {
		spec.Families = append(spec.Families, extension.EventFamily{Type: t, ReDerivable: true})
	}
	return spec
}
func (familyIngress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (familyIngress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{}, nil
}

// A trace refuses what acceptance would from the Event's source: any type
// from an operator; a registered source in its own scope; else a type an
// in-process extension owns; nothing from a source nobody knows.
func TestAdmitTrace(t *testing.T) {
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC) }
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.Transact(ctx, storage.Guard{Now: clock()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	ingress, err := runtime.NewIngress(store, clock, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = ingress.Register(ctx, "chat/eng", extension.Source{Namespace: "chat/eng", ScopeID: "eng"}, familyIngress{[]string{"chat.message"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	d := &daemon{ingress: ingress, inproc: []inproc.Extension{introspection.New()}}
	for name, c := range map[string]struct {
		scope domain.ID
		e     domain.Event
		ok    bool
	}{
		"operator":            {"eng", domain.Event{Type: "anything.at.all", Source: runtime.ManualSource}, true},
		"source family":       {"eng", domain.Event{Type: "chat.message", Source: "chat/eng"}, true},
		"derived type":        {"eng", domain.Event{Type: "chat.reminder", Source: "chat/eng"}, true},
		"other scope":         {"ops", domain.Event{Type: "chat.message", Source: "chat/eng"}, false},
		"extension family":    {"eng", domain.Event{Type: introspection.TypeReported, Source: introspection.SourceNamespace("eng")}, true},
		"unknown source":      {"eng", domain.Event{Type: "nobody.thing", Source: "nobody/x"}, false},
		"unknown source type": {"eng", domain.Event{Type: "chat.message", Source: "chat/ops"}, false},
	} {
		if err := d.admitTrace(c.scope, c.e); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// samplesConfig is a scope per built-in route handler that offers samples,
// each with the entry its samples are built from.
func samplesConfig() []byte {
	shell := func(ops ...string) []any {
		return []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": append([]string{"read_context"}, ops...),
			"secret_names": []any{}, "config": map[string]any{"command": "true"}}}
	}
	route := func(name, handler string, config map[string]any) map[string]any {
		return map[string]any{"name": name, "handler": handler, "binding": "run", "active_policy": "queue", "config": config}
	}
	entry := func(name, typ, connection string, config map[string]any) map[string]any {
		x := map[string]any{"name": name, "type": typ, "secret_names": []any{}, "config": config}
		if connection != "" {
			x["connection"] = connection
		}
		return x
	}
	scope := func(id string, connections []string, bindings []any, routes, entries []any) map[string]any {
		ops := bindings[0].(map[string]any)["operations"]
		return map[string]any{"id": id, "name": id, "authorization": map[string]any{"connections": connections, "operations": ops, "read_scopes": []any{}},
			"bindings": bindings, "routes": routes, "extensions": entries, "secret_refs": map[string]any{}}
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{},
		"connections": []any{
			map[string]any{"id": "github", "type": "github", "config": map[string]any{}, "secret_refs": map[string]any{"token": "env:OC_TEST_GITHUB_TOKEN"}},
			map[string]any{"id": "slack", "type": "slack", "config": map[string]any{}, "secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}},
			map[string]any{"id": "linear", "type": "linear", "config": map[string]any{}, "secret_refs": map[string]any{"token": "env:OC_TEST_LINEAR_TOKEN"}},
			map[string]any{"id": "jira", "type": "jira", "config": map[string]any{"base_url": "https://acme.atlassian.net", "email": "me@example.com"}, "secret_refs": map[string]any{"token": "env:OC_TEST_JIRA_TOKEN"}},
			map[string]any{"id": "bitbucket", "type": "bitbucket", "config": map[string]any{"username": "me@example.com"}, "secret_refs": map[string]any{"token": "env:OC_TEST_BB_TOKEN"}},
		},
		"scopes": []any{
			scope("manual", []string{}, shell(), []any{route("manual", "manual", map[string]any{})}, []any{}),
			scope("gh", []string{"github"}, shell(), []any{route("held", "github-hold", map[string]any{}), route("issues", "github", map[string]any{})},
				[]any{entry("widgets", "github", "github", map[string]any{"namespace": "github/github.com/octo/widgets", "scope": "gh", "repo": "octo/widgets"})}),
			scope("chat", []string{"slack"}, shell(), []any{route("conversations", "slack", map[string]any{})},
				[]any{entry("support", "slack", "slack", map[string]any{"namespace": "slack/T0TEAM", "scope": "chat", "team": "T0TEAM", "channels": []string{"C0SUPPORT"}, "bot_user_id": "UBOT", "receiver": "socket"})}),
			scope("ops", []string{}, shell(), []any{route("jobs", "schedule", map[string]any{})},
				[]any{entry("jobs", "schedule", "", map[string]any{"namespace": "schedule/ops", "scope": "ops", "jobs": []any{map[string]any{"name": "tick", "every_minutes": 60}, map[string]any{"name": "nightly"}}})}),
			scope("eng", []string{"linear"}, shell(), []any{route("issues", "linear", map[string]any{})},
				[]any{entry("eng", "linear", "linear", map[string]any{"namespace": "linear/eng", "scope": "eng", "teams": []string{"ENG"}, "transitions": map[string]any{"claimed": "In Progress"}})}),
			scope("proj", []string{"jira", "bitbucket"}, shell(), []any{route("issues", "jira", map[string]any{}), route("prs", "bitbucket", map[string]any{})},
				[]any{entry("proj", "jira", "jira", map[string]any{"namespace": "jira/acme/proj", "scope": "proj", "projects": []string{"PROJ"}, "transitions": map[string]any{"done": "Done"}}),
					entry("app", "bitbucket", "bitbucket", map[string]any{"namespace": "bitbucket/acme/app", "scope": "proj", "repo": "acme/app", "tracker": map[string]any{"namespace": "jira/acme/proj", "keys": []string{"PROJ"}}})}),
			scope("self", []string{}, shell(introspection.OpReport), []any{route("reports", introspection.Type, map[string]any{"work": introspection.WorkReport})},
				[]any{entry("introspection", introspection.Type, "", map[string]any{})}),
		},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// Every sample a built-in route handler offers can be traced: acceptance
// would take it, so the trace explains it rather than refusing it (#145).
// Each handler that samples must offer at least one here, so a new handler
// joins the check.
func TestEveryBuiltInSampleTraces(t *testing.T) {
	ctx := context.Background()
	snap, err := config.Validate(samplesConfig(), registry(offlineExecutors()))
	if err != nil {
		t.Fatal(err)
	}
	all := handlers()
	inprocHandlers(all, firstParty(nil))
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ingress, err := runtime.NewIngress(store, time.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The host's own sources need no ingress, so their samples are admitted
	// here too; a registered source's are TestAdmitTrace's.
	d := &daemon{ingress: ingress, inproc: firstParty(nil)}
	offered := map[string]int{}
	for _, scope := range snap.DomainScopes() {
		for _, route := range scope.Routes {
			sampler, ok := all[route.Handler].(extension.RouteSampler)
			if !ok {
				continue
			}
			for _, s := range sampler.Samples(ctx, extension.SampleInput{Scope: scope, Route: route, Now: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}) {
				offered[route.Handler]++
				if err := runtime.CheckTraceEvent(s.Event); err != nil {
					t.Errorf("%s %s sample %s: %v", scope.ID, route.Handler, s.ID, err)
				} else if strings.HasPrefix(s.Event.Source, "core.") {
					if err := d.admitTrace(scope.ID, s.Event); err != nil {
						t.Errorf("%s %s sample %s: %v", scope.ID, route.Handler, s.ID, err)
					}
				}
			}
		}
	}
	for name, h := range all {
		if _, ok := h.(extension.RouteSampler); ok && offered[name] == 0 {
			t.Errorf("handler %s offered no samples: give it a scope in samplesConfig", name)
		}
	}
}
