package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// recordingProvider offers one action and records who invoked it.
type recordingProvider struct {
	action string
	calls  *[]extension.ActionCall
}

func (p recordingProvider) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return []extension.Action{{ID: p.action, Label: p.action}}, nil
}

func (p recordingProvider) Invoke(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	*p.calls = append(*p.calls, c)
	return extension.ActionPlan{Event: domain.Event{Type: "fixture.done"}, Message: "done"}, nil
}

// Host.Act calls the provider as a caller of the requesting extension's own
// kind, whatever the request says, finds the one provider offering the
// action when none is named, and keeps each extension's keys apart.
func TestActPinsTheCallerToTheExtension(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := func() time.Time { return time.Unix(1700000000, 0) }
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	var calls []extension.ActionCall
	var keys, names []string
	providers := map[string]extension.ActionProvider{
		"decisions": recordingProvider{action: "answer", calls: &calls},
		"tracker":   recordingProvider{action: "claim", calls: &calls},
		"other":     recordingProvider{action: "claim", calls: &calls},
	}
	h := actHost{store: store, now: now,
		providers: func(context.Context, domain.ID) map[string]extension.ActionProvider { return providers },
		submit: func(ctx context.Context, name string, p extension.ActionProvider, call extension.ActionCall, key string) (domain.AcceptedEvent, bool, string, error) {
			plan, err := p.Invoke(ctx, call)
			keys, names = append(keys, key), append(names, name)
			return domain.AcceptedEvent{Event: plan.Event}, true, plan.Message, err
		}}

	res, err := h.act("github")(ctx, extension.ActRequest{Scope: "eng", Action: "answer", Input: "approve", Caller: "octocat", Key: "issue/1"})
	if err != nil || !res.Added || res.Message != "done" {
		t.Fatalf("act: %+v %v", res, err)
	}
	if len(calls) != 1 || calls[0].Caller != (extension.Caller{Kind: "github", ID: "octocat"}) || calls[0].Input != "approve" || names[0] != "decisions" {
		t.Fatalf("calls: %+v names %v", calls, names)
	}
	// Another extension's identical key is another request.
	if _, err = h.act("slack")(ctx, extension.ActRequest{Scope: "eng", Action: "answer", Caller: "U1", Key: "issue/1"}); err != nil {
		t.Fatal(err)
	}
	if keys[0] == keys[1] || calls[1].Caller.Kind != "slack" {
		t.Fatalf("keys %v calls %+v", keys, calls)
	}
	// A caller ID can't smuggle in another kind: it is only ever an ID.
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Action: "answer", Caller: "operator:local", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if c := calls[2].Caller; c.Kind != "github" || c.ID != "operator:local" {
		t.Fatalf("caller %+v", c)
	}

	var oe *extension.OperationError
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Action: "deploy", Caller: "octocat", Key: "k2"}); !errors.As(err, &oe) || oe.Code != extension.CodeNoAction {
		t.Fatalf("an action nothing offers: %v", err)
	}
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Action: "claim", Caller: "octocat", Key: "k3"}); !errors.As(err, &oe) || oe.Code != "ambiguous" {
		t.Fatalf("an action two providers offer: %v", err)
	}
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Provider: "tracker", Action: "claim", Caller: "octocat", Key: "k4"}); err != nil || names[len(names)-1] != "tracker" {
		t.Fatalf("a named provider: %v %v", err, names)
	}
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Provider: "nobody", Action: "claim", Caller: "octocat", Key: "k5"}); !errors.As(err, &oe) || oe.Code != extension.CodeNoAction {
		t.Fatalf("an unknown provider: %v", err)
	}
	for _, r := range []extension.ActRequest{
		{Scope: "eng", Action: "answer", Key: "k"},
		{Scope: "eng", Action: "answer", Caller: "octocat"},
		{Scope: "eng", Action: "answer", Caller: "octo\ncat", Key: "k"},
		{Action: "answer", Caller: "octocat", Key: "k"},
	} {
		if _, err = h.act("github")(ctx, r); !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	if _, err = h.act("github")(ctx, extension.ActRequest{Scope: "eng", Work: "work/elsewhere", Action: "answer", Caller: "octocat", Key: "k6"}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unknown Work: %v", err)
	}
	if len(calls) != 4 {
		t.Fatalf("a refused request reached a provider: %d calls", len(calls))
	}
}
