package slack_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/storage"
)

func TestClaimFromParses(t *testing.T) {
	for extra, want := range map[string]string{
		``:                                     slack.ClaimRegistration,
		`,"claim_from":"registration"`:         slack.ClaimRegistration,
		`,"claim_from":"all"`:                  slack.ClaimAll,
		`,"claim_from":"2026-11-13T00:00:00Z"`: "",
		`,"claim_from":"2026-11-13T01:00:00+01:00"`: "",
	} {
		s := settings(t, extra)
		if s.Claim != want {
			t.Fatalf("%s: claim %q, want %q", extra, s.Claim, want)
		}
		if want == "" && !s.ClaimSince.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s: since %v", extra, s.ClaimSince)
		}
	}
	for _, bad := range []string{`"yesterday"`, `"2026-11-13"`, `""x`, `3`} {
		cfg := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `"],"claim_from":` + bad + `}`
		if _, err := slack.ParseExtension([]byte(cfg), nil); err == nil {
			t.Fatalf("claim_from %s accepted", bad)
		}
	}
}

// A thread no Work owns is claimed only when it began at or after its
// channel's start point: the recorded first registration by default, a
// configured time, or any age with claim_from all. Without a recorded
// start point nothing is claimed, and a thread with replies marked by
// Work this daemon has no record of is never claimed.
func TestRouteClaimsOnlyThreadsFromTheStartPoint(t *testing.T) {
	registeredAt := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	book := slack.NewBook()
	book.SetStart(ns, chanA, registeredAt)
	r := slack.Route{Book: book}
	mention := func(written, thread time.Time) domain.AcceptedEvent {
		p := slack.MessagePayload{TS: slack.TS(written), User: alice, Text: "<@" + botUser + "> help", Mentions: []string{botUser}}
		if !thread.Equal(written) {
			p.Thread = slack.TS(thread)
		}
		return event(t, slack.TypeMessageHuman, p, registeredAt.Add(time.Hour))
	}
	try := func(r slack.Route, sc domain.Scope, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
		t.Helper()
		d, err := r.Match(context.Background(), extension.RouteInput{Event: e, Scope: sc, Work: w})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	sc := scopeWith(t, `,"start":"mention"`)
	old := registeredAt.Add(-5 * time.Hour)
	if d := try(r, sc, mention(old, old), nil); d.Match || !strings.Contains(d.Reason, "start point") {
		t.Fatalf("a thread from before registration was claimed: %+v", d)
	}
	fresh := registeredAt.Add(time.Minute)
	if d := try(r, sc, mention(fresh, fresh), nil); !d.Match || d.Kind != slack.WorkKind {
		t.Fatalf("a mention after registration did not start Work: %+v", d)
	}
	// The thread decides, not the message: a new mention in an old thread
	// starts nothing.
	if d := try(r, sc, mention(fresh, old), nil); d.Match {
		t.Fatalf("a new mention in a thread begun before registration was claimed: %+v", d)
	}
	// Work that owns an old thread still takes its input.
	if d := try(r, sc, mention(fresh, old), &domain.Work{ID: "w", Kind: slack.WorkKind}); !d.Match {
		t.Fatalf("an owned thread lost its input: %+v", d)
	}
	// A channel not registered yet (a config trace) answers for a thread
	// begun after it registers, and says so.
	if d := try(slack.Route{Book: slack.NewBook()}, sc, mention(fresh, fresh), nil); !d.Match || !strings.Contains(d.Reason, "no start point yet") {
		t.Fatalf("an unregistered channel's mention: %+v", d)
	}
	all := scopeWith(t, `,"start":"mention","claim_from":"all"`)
	if d := try(slack.Route{}, all, mention(old, old), nil); !d.Match {
		t.Fatalf("claim_from all refused an old thread: %+v", d)
	}
	since := scopeWith(t, `,"start":"mention","claim_from":"2026-11-13T06:00:00Z"`)
	if d := try(r, since, mention(old, old), nil); !d.Match {
		t.Fatalf("a thread after the configured start point was refused: %+v", d)
	}
	if d := try(r, since, mention(old.Add(-2*time.Hour), old.Add(-2*time.Hour)), nil); d.Match {
		t.Fatalf("a thread before the configured start point was claimed: %+v", d)
	}
	book.Remember(ns, chanA, []string{slack.TS(fresh)})
	if d := try(r, all, mention(fresh, fresh), nil); d.Match || !strings.Contains(d.Reason, "marked") {
		t.Fatalf("a thread with replies marked by unknown Work was claimed: %+v", d)
	}
}

// Reconcile asks which threads no Work here owns among those that began
// before the start point and ask for a conversation, and those with marked
// replies; it records the marked ones before returning the page and counts
// the rest for doctor once the generation completes.
func TestReconcileFindsUnclaimedThreads(t *testing.T) {
	var asked, recorded []string
	var works []string
	owned := map[string]bool{}
	f := newFixture(t, `,"start":"mention"`, slack.IngressOptions{})
	start := f.fake.Now().Add(time.Hour)
	o := slack.IngressOptions{
		StartPoint: func(string) (time.Time, bool) { return start, true },
		Unowned: func(_ context.Context, _ string, threads []string) (map[string]bool, error) {
			asked = append(asked, threads...)
			out := map[string]bool{}
			for _, t := range threads {
				out[t] = !owned[t]
			}
			return out, nil
		},
		Foreign: func(_ context.Context, _ string, marked map[string][]string) ([]string, error) {
			for t, ids := range marked {
				recorded = append(recorded, t)
				works = append(works, ids...)
			}
			return recorded, nil
		},
	}
	o.ActiveThreads = func(context.Context) (map[string][]string, error) { return nil, nil }
	f.in = slack.NewIngress(f.s, f.client, f.clock.Now, o)
	var old []string
	for i := 0; i < 10; i++ {
		old = append(old, f.fake.Post(chanA, alice, "<@"+botUser+"> question"))
	}
	f.fake.Post(chanA, bob, "chatter, no mention")
	marked := old[0]
	if _, err := f.client.PostMessage(context.Background(), slack.Post{Channel: chanA, ThreadTS: marked, Text: "answer", Metadata: slack.Marker("w-gone", "x", "i1")}); err != nil {
		t.Fatal(err)
	}
	mine := f.fake.Post(chanA, alice, "<@"+botUser+"> mine")
	owned[mine] = true
	if _, err := f.client.PostMessage(context.Background(), slack.Post{Channel: chanA, ThreadTS: mine, Text: "ours", Metadata: slack.Marker("w-here", "x", "i2")}); err != nil {
		t.Fatal(err)
	}
	f.fake.Advance(time.Minute)
	f.sync()
	f.scan()
	if len(recorded) != 1 || recorded[0] != marked || len(works) != 1 || works[0] != "w-gone" {
		t.Fatalf("offered %v naming %v, want only the marked thread no Work owns, naming its marker's Work", recorded, works)
	}
	got := f.in.Unclaimed()[chanA]
	if got.BeforeStart != 9 {
		t.Fatalf("unclaimed %+v, want 9 before the start point besides the marked one", got)
	}
	for _, a := range asked {
		if a != marked && a != mine && !contains(old, a) {
			t.Fatalf("asked about thread %s, which neither asks for a conversation nor holds marked replies", a)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// kvTx is the slice of a storage.Tx Book.Register and MarkForeign use.
type kvTx struct {
	storage.Tx
	kv    map[string][]byte
	works []domain.Work
}

func (t *kvTx) Get(ns, key string) ([]byte, error) {
	if v, ok := t.kv[ns+"|"+key]; ok {
		return v, nil
	}
	return nil, storage.ErrNotFound
}

func (t *kvTx) Put(ns, key string, v []byte) error { t.kv[ns+"|"+key] = v; return nil }

func (t *kvTx) List(ns, prefix string) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range t.kv {
		if rest, ok := strings.CutPrefix(k, ns+"|"); ok && strings.HasPrefix(rest, prefix) {
			out = append(out, storage.Entry{Key: rest, Value: v})
		}
	}
	return out, nil
}

func (t *kvTx) ListWorks() ([]domain.Work, error) { return t.works, nil }

// A channel's start point is recorded once: the first registration's time,
// or the creation of the earliest Work already holding one of its threads
// (an entry upgraded from before start points). Later registrations, in
// this daemon or after a restart, keep it, and load the marked threads.
func TestStartPointsAreRecordedOnce(t *testing.T) {
	first := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	earlier := first.Add(-48 * time.Hour)
	tx := &kvTx{kv: map[string][]byte{}, works: []domain.Work{{ID: "w", CreatedAt: earlier,
		CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: slack.ThreadKey(chanA, slack.TS(earlier))}}}}}
	b := slack.NewBook()
	if err := b.Register(tx, ns, []string{chanA, chanB}, first); err != nil {
		t.Fatal(err)
	}
	if at, ok := b.StartPoint(ns, chanA); !ok || !at.Equal(earlier) {
		t.Fatalf("upgraded channel's start point %v %v, want its earliest Work's creation %v", at, ok, earlier)
	}
	if at, ok := b.StartPoint(ns, chanB); !ok || !at.Equal(first) {
		t.Fatalf("new channel's start point %v %v, want %v", at, ok, first)
	}
	if err := slack.MarkForeign(tx, ns, chanB, []string{"1794597426.185892"}, first); err != nil {
		t.Fatal(err)
	}
	restarted := slack.NewBook()
	if err := restarted.Register(tx, ns, []string{chanB}, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if at, _ := restarted.StartPoint(ns, chanB); !at.Equal(first) {
		t.Fatalf("a restart moved the start point to %v", at)
	}
	if !restarted.Foreign(ns, chanB, "1794597426.185892") || restarted.ForeignCount(ns, chanB) != 1 {
		t.Fatal("a restart lost the marked thread")
	}
	if err := b.Register(tx, ns, []string{slack.DirectStart}, first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if at, ok := b.StartPoint(ns, "D0ANYONE"); !ok || !at.Equal(first.Add(time.Minute)) {
		t.Fatalf("a direct message's start point %v %v, want the entry's registration with direct messages", at, ok)
	}
	if _, ok := restarted.StartPoint("slack/T0OTHER", chanB); ok {
		t.Fatal("another namespace shares the start point")
	}
}
