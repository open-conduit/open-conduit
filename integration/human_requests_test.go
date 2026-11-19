package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	hu "github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// announce runs the extension's announcing pass, accepting as the host
// does for its own families.
func (h *humanHarness) announce(t *testing.T) int {
	t.Helper()
	n, err := hu.Announce(context.Background(), h.f.Store, h.f.Clock.Now(), runtime.AcceptInTx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *humanHarness) sync(t *testing.T) {
	t.Helper()
	if _, err := hu.Sync(context.Background(), h.f.Store, h.f.Clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func requestPayload(t *testing.T, a domain.AcceptedEvent) hu.RequestPayload {
	t.Helper()
	var p hu.RequestPayload
	if a.Event.Data.Schema != hu.SchemaRequest || json.Unmarshal(a.Event.Data.Data, &p) != nil {
		t.Fatalf("request payload: %+v", a.Event)
	}
	return p
}

// Each change of a request is announced once, in order: opened with who
// may answer it and when it expires, escalated with the wider responders,
// and settled with how it ended and who settled it. The Events are on the
// request's own key, which the asking Work does not own, so they never
// reach the asker; announcing again adds nothing.
func TestHumanRequestEventsAnnounceEachChange(t *testing.T) {
	h := newHumanHarness(t, "persistent", escalating)
	out, err := h.deploy(t, `{"env":"prod"}`)
	if err != nil {
		t.Fatal(err)
	}
	h.park(t, out)
	if n := h.announce(t); n != 1 {
		t.Fatalf("announced %d on open", n)
	}
	opened := h.eventsOfType(t, hu.TypeRequestOpened)
	if len(opened) != 1 {
		t.Fatalf("opened: %+v", opened)
	}
	p := requestPayload(t, opened[0])
	if p.Work != "work/item" || p.Provider != "decisions" || p.Kind != hu.KindApproval || p.Operation != "fixture.deploy" || p.Action != hu.ActionAnswer ||
		p.State != extension.RequestOpen || p.By != "" || p.Binding != "fixture" || p.Execution == "" || !slices.Equal(p.Responders, []string{"operator:local"}) ||
		!p.ExpiresAt.Equal(p.AskedAt.Add(2*time.Hour)) || !slices.Equal(p.Options, []string{hu.OptionApprove, hu.OptionRefuse}) || p.Label == "" {
		t.Fatalf("opened payload: %+v", p)
	}
	e := opened[0].Event
	if e.Source != hu.Type || e.WorkID != "" || e.CorrelationKey == nil || *e.CorrelationKey != hu.RequestKey(p.Request) {
		t.Fatalf("opened envelope: %+v", e)
	}
	if n := h.announce(t); n != 0 {
		t.Fatalf("announced %d again", n)
	}

	h.f.Clock.Advance(90 * time.Minute)
	h.sync(t)
	if n := h.announce(t); n != 1 {
		t.Fatalf("announced %d on escalation", n)
	}
	escalated := h.eventsOfType(t, hu.TypeRequestEscalated)
	if len(escalated) != 1 {
		t.Fatalf("escalated: %+v", escalated)
	}
	if p := requestPayload(t, escalated[0]); p.State != extension.RequestOpen || !slices.Contains(p.Responders, "github:octocat") || p.EscalatedAt.IsZero() {
		t.Fatalf("escalated payload: %+v", p)
	}

	if _, err = h.answer(t, extension.Caller{Kind: "github", ID: "octocat"}, "refuse", "r1"); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.reconcile(t)
	h.sync(t)
	if n := h.announce(t); n != 1 {
		t.Fatalf("announced %d on settling", n)
	}
	settled := h.eventsOfType(t, hu.TypeRequestSettled)
	if len(settled) != 1 {
		t.Fatalf("settled: %+v", settled)
	}
	if p := requestPayload(t, settled[0]); p.State != extension.RequestRefused || p.By != "github:octocat" || p.SettledAt.IsZero() || p.Request != requestPayload(t, opened[0]).Request {
		t.Fatalf("settled payload: %+v", p)
	}
	if n := h.announce(t); n != 0 {
		t.Fatalf("announced %d after settling", n)
	}
	work := h.work(t)
	for _, k := range work.CorrelationKeys {
		if _, ok := hu.ParseRequestKey(k); ok {
			t.Fatalf("the asking Work owns its request's key: %+v", k)
		}
	}
}

// A request that settles before it is announced is still opened first, and
// one that expires is settled as expired with nobody as its settler.
func TestHumanRequestEventsOpenBeforeTheySettle(t *testing.T) {
	h := newHumanHarness(t, "persistent", gated)
	out, err := h.deploy(t, `{"env":"prod"}`)
	if err != nil {
		t.Fatal(err)
	}
	h.park(t, out)
	h.f.Clock.Advance(3 * time.Hour)
	h.sync(t)
	if n := h.announce(t); n != 2 {
		t.Fatalf("announced %d", n)
	}
	var types []string
	routingRead(t, h.f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if slices.Contains(hu.RequestTypes, a.Event.Type) {
				types = append(types, a.Event.Type)
				if a.Event.Type == hu.TypeRequestSettled {
					var p hu.RequestPayload
					if json.Unmarshal(a.Event.Data.Data, &p) != nil || p.State != extension.RequestExpired || p.By != "" {
						t.Errorf("settled payload: %s", a.Event.Data.Data)
					}
				}
			}
		}
		return err
	})
	if !slices.Equal(types, []string{hu.TypeRequestOpened, hu.TypeRequestSettled}) {
		t.Fatalf("order: %v", types)
	}
}

// An ask recorded before requests were announced is never announced, so
// upgrading doesn't announce a history of settled requests.
func TestHumanRequestEventsSkipEarlierAsks(t *testing.T) {
	h := newHumanHarness(t, "persistent", gated)
	out, err := h.deploy(t, `{"env":"prod"}`)
	if err != nil {
		t.Fatal(err)
	}
	h.park(t, out)
	if err := h.f.Store.Transact(context.Background(), storage.Guard{Now: h.f.Clock.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List(hu.Namespace, "ask/")
		for _, row := range rows {
			var a map[string]any
			if err := json.Unmarshal(row.Value, &a); err != nil {
				return err
			}
			delete(a, "announce")
			raw, _ := json.Marshal(a)
			if err := tx.Put(hu.Namespace, row.Key, raw); err != nil {
				return err
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := h.announce(t); n != 0 || len(h.eventsOfType(t, hu.TypeRequestOpened)) != 0 {
		t.Fatalf("an earlier ask was announced: %d", n)
	}
}
