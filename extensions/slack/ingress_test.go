package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

const (
	team    = "T0TEAM"
	chanA   = "C0SUPPORT"
	chanB   = "C0OTHER"
	ns      = "slack/T0TEAM"
	scope   = "support"
	secret  = "signing-secret"
	alice   = "UALICE"
	bob     = "UBOB"
	botUser = "UBOT"
)

var src = extension.Source{Namespace: ns, ScopeID: scope}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fixture struct {
	t      *testing.T
	fake   *slacktest.Server
	clock  *clock
	client *slack.Client
	s      slack.Settings
	in     *slack.Ingress
	active map[string][]string
}

func settings(t *testing.T, extra string) slack.Settings {
	t.Helper()
	cfg := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `","` + chanB + `"],"bot_user_id":"` + botUser + `","signing_secret":"signing"` + extra + `}`
	s, err := slack.ParseExtension([]byte(cfg), []string{"signing"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newFixture(t *testing.T, extra string, o slack.IngressOptions) *fixture {
	t.Helper()
	fake := slacktest.New(team, "xoxb-test")
	t.Cleanup(fake.Close)
	f := &fixture{t: t, fake: fake, clock: &clock{t: fake.Now()}, s: settings(t, extra), active: map[string][]string{}}
	cfg, err := slack.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.client, err = slack.NewClient(cfg, func(context.Context, string) (string, error) { return "xoxb-test", nil }, slack.ClientOptions{Now: f.clock.Now}); err != nil {
		t.Fatal(err)
	}
	o.ActiveThreads = func(context.Context) (map[string][]string, error) { return f.active, nil }
	f.in = slack.NewIngress(f.s, f.client, f.clock.Now, o)
	// Every channel exists in the fake.
	f.fake.Join(chanA, bob)
	f.fake.Join(chanB, bob)
	return f
}

// sync moves the extension's clock to the fake's.
func (f *fixture) sync() { f.clock.Set(f.fake.Now()) }

func (f *fixture) push(body []byte) extension.PushResult {
	f.t.Helper()
	v, err := slack.VerifyDelivery(secret, slacktest.Headers(secret, body, f.clock.Now()), body, f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.in.Push(context.Background(), src, v.Delivery)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) scan() []extension.Observation {
	f.t.Helper()
	obs, err := f.scanErr()
	if err != nil {
		f.t.Fatal(err)
	}
	return obs
}

func (f *fixture) scanErr() ([]extension.Observation, error) {
	scan := extension.Scan{Source: src}
	var all []extension.Observation
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		page, err := f.in.Reconcile(context.Background(), scan)
		if err != nil {
			return all, err
		}
		all = append(all, page.Observations...)
		if page.Complete {
			return all, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			f.t.Fatalf("scan stalled at %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	f.t.Fatal("scan did not complete")
	return nil, nil
}

// untilDeferred scans to the first page Slack defers, and returns the scan
// that asked for it.
func (f *fixture) untilDeferred() (extension.Scan, extension.Snapshot) {
	f.t.Helper()
	scan := extension.Scan{Source: src}
	for i := 0; i < 100; i++ {
		page, err := f.in.Reconcile(context.Background(), scan)
		if err != nil {
			f.t.Fatal(err)
		}
		if !page.NotBefore.IsZero() {
			scan.ScanID = page.ScanID
			return scan, page
		}
		if page.Complete {
			f.t.Fatal("scan completed without a deferral")
		}
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	f.t.Fatal("no deferral")
	return scan, extension.Snapshot{}
}

func byItem(obs []extension.Observation) map[string]extension.Observation {
	out := map[string]extension.Observation{}
	for _, o := range obs {
		out[o.ID] = o
	}
	return out
}

func payload(t *testing.T, o extension.Observation) slack.MessagePayload {
	t.Helper()
	var p slack.MessagePayload
	if err := json.Unmarshal(o.Event.Data.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func item(channel, thread, ts string) string {
	return slack.ThreadKey(channel, thread) + "/message/" + ts
}

// sameIdentity requires push and reconcile to agree on everything except the
// actual receipt time.
func sameIdentity(t *testing.T, label string, pushed, scanned extension.Observation) {
	t.Helper()
	pushed.Event.ReceivedAt, scanned.Event.ReceivedAt = time.Time{}, time.Time{}
	a, _ := json.Marshal(pushed)
	b, _ := json.Marshal(scanned)
	if string(a) != string(b) {
		t.Fatalf("%s: push and reconcile differ:\npush %s\nscan %s", label, a, b)
	}
}

func TestIngressConformance(t *testing.T) {
	conformance.RunIngress(t, func() conformance.IngressFixture {
		f := newFixture(t, "", slack.IngressOptions{PageSize: 2})
		root := f.fake.Post(chanA, alice, "the export is broken")
		for i := 0; i < 3; i++ {
			f.fake.Post(chanA, bob, "filler "+strconv.Itoa(i))
		}
		gone := f.fake.Post(chanA, bob, "oops")
		prev := f.fake.Delete(chanA, gone)
		f.sync()
		body := f.fake.MessageEvent(chanA, root)
		v, _ := slack.VerifyDelivery(secret, slacktest.Headers(secret, body, f.clock.Now()), body, f.clock.Now())
		res, err := f.in.Push(context.Background(), src, v.Delivery)
		if err != nil || len(res.Observations) != 1 {
			t.Fatalf("push: %v %+v", err, res)
		}
		del := f.fake.DeletedEvent(chanA, prev)
		dv, _ := slack.VerifyDelivery(secret, slacktest.Headers(secret, del, f.clock.Now()), del, f.clock.Now())
		return conformance.IngressFixture{Ingress: f.in, Source: src, Delivery: v.Delivery, Canonical: res.Observations[0], OneShotType: slack.TypeMessageRemoved, OneShot: &dv.Delivery}
	})
}

func TestPushAndReconcileShareIdentity(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "Hi <@UBOT>, invoices fail")
	reply := f.fake.Reply(chanA, root, bob, "same for me <@UALICE|alice>")
	bot := f.fake.BotReply(chanA, root, "BOTHER", "ticket #42 opened")
	f.sync()
	pushed := map[string]extension.Observation{}
	for _, ts := range []string{root, reply, bot} {
		res := f.push(f.fake.MessageEvent(chanA, ts))
		if len(res.Observations) != 1 || len(res.Events) != 0 {
			t.Fatalf("push %s: %+v", ts, res)
		}
		pushed[ts] = res.Observations[0]
	}
	scanned := byItem(f.scan())
	for ts, label := range map[string]string{root: "root", reply: "reply", bot: "bot"} {
		o, ok := scanned[item(chanA, root, ts)]
		if !ok {
			t.Fatalf("%s not reconciled", label)
		}
		sameIdentity(t, label, pushed[ts], o)
	}
	r := scanned[item(chanA, root, root)]
	if r.Event.Type != slack.TypeMessageHuman || r.Event.CorrelationKey.ExternalID != slack.ThreadKey(chanA, root) {
		t.Fatalf("root: %+v", r.Event)
	}
	if p := payload(t, r); !p.Root || p.Thread != root || len(p.Mentions) != 1 || p.Mentions[0] != botUser {
		t.Fatalf("root payload: %+v", p)
	}
	if p := payload(t, scanned[item(chanA, root, reply)]); p.Root || p.Thread != root || p.Mentions[0] != alice {
		t.Fatalf("reply payload: %+v", p)
	}
	if scanned[item(chanA, root, bot)].Event.Type != slack.TypeMessageBot {
		t.Fatal("bot message class")
	}
	if !strings.HasSuffix(r.Event.DeliveryID, "@"+r.Version) || !strings.HasPrefix(r.Event.DeliveryID, r.ID) {
		t.Fatalf("delivery identity %q", r.Event.DeliveryID)
	}
}

func TestEditsDeletionsAndIgnoredSubtypes(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "question")
	reply := f.fake.Reply(chanA, root, alice, "typo")
	lone := f.fake.Post(chanA, bob, "never mind")
	f.sync()
	before := byItem(f.scan())
	f.fake.Edit(chanA, reply, "fixed typo")
	f.sync()
	edited := f.push(f.fake.ChangedEvent(chanA, reply))
	if len(edited.Observations) != 1 {
		t.Fatalf("edit push: %+v", edited)
	}
	e := edited.Observations[0]
	if e.ID != item(chanA, root, reply) || e.Version == before[e.ID].Version || e.Event.DeliveryID == before[e.ID].Event.DeliveryID {
		t.Fatal("an edit is a new version of the same item")
	}
	if p := payload(t, e); p.Edited == "" || p.Text != "fixed typo" {
		t.Fatalf("edit payload %+v", p)
	}
	// A parent with replies becomes a tombstone; a lone message disappears.
	f.fake.Delete(chanA, root)
	prev := f.fake.Delete(chanA, lone)
	f.sync()
	tomb := f.push(f.fake.ChangedEvent(chanA, root))
	if len(tomb.Observations) != 1 || tomb.Observations[0].Event.Type != slack.TypeMessageDeleted {
		t.Fatalf("tombstone push: %+v", tomb)
	}
	removed := f.push(f.fake.DeletedEvent(chanA, prev))
	if len(removed.Observations) != 0 || len(removed.Events) != 1 || removed.Events[0].Type != slack.TypeMessageRemoved ||
		removed.Events[0].CorrelationKey.ExternalID != slack.ThreadKey(chanA, lone) {
		t.Fatalf("removal push: %+v", removed)
	}
	// Reply edits leave the thread summary unchanged: seen after refresh.
	f.clock.Add(61 * time.Minute)
	after := byItem(f.scan())
	sameIdentity(t, "edit", e, after[e.ID])
	sameIdentity(t, "tombstone", tomb.Observations[0], after[item(chanA, root, root)])
	if _, ok := after[item(chanA, lone, lone)]; ok {
		t.Fatal("a removed message is still observed")
	}
	for _, o := range after {
		if p := payload(t, o); p.User == bob && p.Text != "" && strings.Contains(p.Text, "joined") {
			t.Fatal("channel_join observed")
		}
	}
	// A join pushed live is ignored too.
	join := f.fake.Join(chanA, alice)
	if res := f.push(f.fake.MessageEvent(chanA, join)); len(res.Observations)+len(res.Events) != 0 {
		t.Fatalf("join push: %+v", res)
	}
}

func TestPushIgnoresForeignTeamsAndChannels(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	f.fake.Join("C0ELSEWHERE", bob)
	ts := f.fake.Post("C0ELSEWHERE", alice, "not ours")
	f.sync()
	if res := f.push(f.fake.MessageEvent("C0ELSEWHERE", ts)); len(res.Observations) != 0 {
		t.Fatal("an unwatched channel was observed")
	}
	other := slacktest.New("T0OTHER", "xoxb-test")
	defer other.Close()
	other.Join(chanA, bob)
	ts = other.Post(chanA, alice, "other workspace")
	if res := f.push(other.MessageEvent(chanA, ts)); len(res.Observations) != 0 {
		t.Fatal("another team's delivery was observed")
	}
	if _, err := f.in.Push(context.Background(), extension.Source{Namespace: "x", ScopeID: scope}, extension.Delivery{}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("wrong source accepted")
	}
}

// TestHundredsOfMessagesOneThread paginates history and a long thread under a
// tight call budget: every message is one distinct observation of one thread.
func TestHundredsOfMessagesOneThread(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{PageSize: 7, BatchUnits: 2})
	root := f.fake.Post(chanA, alice, "long running incident")
	for i := 0; i < 450; i++ {
		f.fake.Reply(chanA, root, []string{alice, bob}[i%2], "update "+strconv.Itoa(i))
	}
	for i := 0; i < 30; i++ {
		f.fake.Post(chanA, bob, "other topic "+strconv.Itoa(i))
	}
	f.sync()
	obs := f.scan()
	items := map[string]bool{}
	keys := map[string]int{}
	for _, o := range obs {
		if items[o.ID] {
			t.Fatalf("item %s observed twice", o.ID)
		}
		items[o.ID] = true
		keys[o.Event.CorrelationKey.ExternalID]++
	}
	if keys[slack.ThreadKey(chanA, root)] != 451 || len(keys) != 31 {
		t.Fatalf("thread messages %d, threads %d", keys[slack.ThreadKey(chanA, root)], len(keys))
	}
}

func TestReconcileCachesQuietThreadsAndRefreshes(t *testing.T) {
	f := newFixture(t, `,"refresh_minutes":10`, slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "q")
	reply := f.fake.Reply(chanA, root, bob, "a")
	f.sync()
	f.scan()
	calls := f.fake.Calls("conversations.replies")
	f.scan()
	if f.fake.Calls("conversations.replies") != calls {
		t.Fatal("an unchanged thread was re-read")
	}
	// A new reply changes the parent's summary: re-read at once.
	f.fake.Reply(chanA, root, alice, "thanks")
	f.sync()
	if obs := f.scan(); len(obs) != 3 || f.fake.Calls("conversations.replies") != calls+1 {
		t.Fatalf("new reply: %d observations, %d calls", len(obs), f.fake.Calls("conversations.replies"))
	}
	// An edited reply leaves the summary unchanged: seen after refresh.
	f.fake.Edit(chanA, reply, "a (edited)")
	f.sync()
	if p := payload(t, byItem(f.scan())[item(chanA, root, reply)]); p.Edited != "" {
		t.Fatal("refresh window not honoured")
	}
	f.clock.Add(11 * time.Minute)
	if p := payload(t, byItem(f.scan())[item(chanA, root, reply)]); p.Edited == "" {
		t.Fatal("edited reply not seen after refresh")
	}
}

// TestOwnedThreadsOlderThanHistory reconciles replies of a thread live Work
// owns whose parent left the history window (a multi-day wait).
func TestOwnedThreadsOlderThanHistory(t *testing.T) {
	f := newFixture(t, `,"history_hours":24`, slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "can you check tomorrow?")
	f.fake.Advance(72 * time.Hour)
	late := f.fake.Reply(chanA, root, alice, "any news?")
	f.sync()
	if _, ok := byItem(f.scan())[item(chanA, root, late)]; ok {
		t.Fatal("an unowned old thread was read")
	}
	f.active[chanA] = []string{root}
	got := byItem(f.scan())
	if _, ok := got[item(chanA, root, late)]; !ok {
		t.Fatal("owned old thread reply missed")
	}
	if _, ok := got[item(chanA, root, root)]; !ok {
		t.Fatal("owned old thread parent missed")
	}
}

func TestRateLimitedScanInfersNothingThenResumes(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "q")
	f.fake.Reply(chanA, root, bob, "a")
	f.sync()
	f.fake.RateLimit("conversations.replies", 30, 1)
	scan, page := f.untilDeferred()
	if page.Complete || len(page.Observations) != 0 || page.NextCursor != scan.Cursor || !page.NotBefore.Equal(f.clock.Now().Add(30*time.Second)) {
		t.Fatalf("expected a deferred page, got %+v", page)
	}
	if until, paused := f.client.PausedUntil(); !paused || until.Sub(f.clock.Now()) != 30*time.Second {
		t.Fatalf("pause %v %v", until, paused)
	}
	before := f.fake.Calls("conversations.replies")
	again, err := f.in.Reconcile(context.Background(), scan)
	if err != nil || again.NotBefore.IsZero() || f.fake.Calls("conversations.replies") != before {
		t.Fatalf("a paused method reached Slack: %+v %v", again, err)
	}
	f.clock.Add(31 * time.Second)
	if obs := f.scan(); len(obs) != 2 {
		t.Fatalf("resumed scan: %d", len(obs))
	}
}

// perMinute is Slack's limit for a non-Marketplace app: each method answers
// once a minute and says when to come back.
type perMinute struct {
	base  http.RoundTripper
	now   func() time.Time
	mu    sync.Mutex
	next  map[string]time.Time
	calls int
}

func (l *perMinute) RoundTrip(r *http.Request) (*http.Response, error) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	l.mu.Lock()
	wait := l.next[method].Sub(l.now())
	if wait <= 0 {
		l.next[method] = l.now().Add(time.Minute)
		l.calls++
	}
	l.mu.Unlock()
	if wait > 0 {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: r,
			Header: http.Header{"Retry-After": {strconv.Itoa(int(wait.Seconds()) + 1)}}, Body: http.NoBody}, nil
	}
	return l.base.RoundTrip(r)
}

// A scan under one request a minute per method finishes, with every message
// of a thread longer than a page, however long that takes.
func TestScanCompletesUnderOneRequestAMinute(t *testing.T) {
	f := newFixture(t, `,"history_page_size":15`, slack.IngressOptions{})
	limiter := &perMinute{base: http.DefaultTransport, now: f.clock.Now, next: map[string]time.Time{}}
	cfg, err := slack.ParseConnection([]byte(`{"api_url":"` + f.fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := slack.NewClient(cfg, func(context.Context, string) (string, error) { return "xoxb-test", nil },
		slack.ClientOptions{Now: f.clock.Now, HTTP: &http.Client{Transport: limiter}})
	if err != nil {
		t.Fatal(err)
	}
	in := slack.NewIngress(f.s, client, f.clock.Now, slack.IngressOptions{})
	want := 0
	for i := 0; i < 40; i++ {
		root := f.fake.Post(chanA, alice, "q"+strconv.Itoa(i))
		want++
		if i%8 == 0 {
			for r := 0; r < 40; r++ {
				f.fake.Reply(chanA, root, bob, "a"+strconv.Itoa(r))
				want++
			}
		}
	}
	f.sync()
	started := f.clock.Now()
	scan := extension.Scan{Source: src}
	got := map[string]bool{}
	deferred := 0
	for i := 0; ; i++ {
		if i > 5000 {
			t.Fatal("scan never completed")
		}
		page, err := in.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if page.ScanID == "" || (scan.ScanID != "" && page.ScanID != scan.ScanID) {
			t.Fatalf("generation restarted at page %d: %+v", i, page)
		}
		for _, o := range page.Observations {
			got[o.ID] = true
		}
		if page.Complete {
			break
		}
		if !page.NotBefore.IsZero() {
			deferred++
			f.clock.Set(page.NotBefore)
		}
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	if len(got) != want {
		t.Fatalf("observed %d of %d messages", len(got), want)
	}
	if deferred == 0 {
		t.Fatal("the limit never applied")
	}
	// Three history pages and, for five long threads, three replies pages
	// each, plus one for each of the channel's other pages' threads: no read
	// is paid for twice.
	if took := f.clock.Now().Sub(started); limiter.calls != f.fake.Calls("conversations.history")+f.fake.Calls("conversations.replies") || took < time.Duration(limiter.calls/2-1)*time.Minute {
		t.Fatalf("%d calls over %s", limiter.calls, took)
	}
	if replies := f.fake.Calls("conversations.replies"); replies != 5*3 {
		t.Fatalf("replies calls = %d, want 15: a read was repeated", replies)
	}
}

// A paced generation is old because it is waiting, so the age bound that
// restarts a stuck one leaves it alone.
func TestPacedGenerationOutlastsTheAgeBound(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	root := f.fake.Post(chanA, alice, "q")
	f.fake.Reply(chanA, root, bob, "a")
	f.sync()
	f.fake.RateLimit("conversations.replies", 30, 1)
	scan, _ := f.untilDeferred()
	f.clock.Add(7 * time.Hour)
	next, err := f.in.Reconcile(context.Background(), scan)
	if err != nil || next.ScanID != scan.ScanID || (next.NextCursor == "" && !next.Complete) {
		t.Fatalf("paced generation was restarted: %+v %v", next, err)
	}
}

func TestReconcileSecondsMustFitTheWindow(t *testing.T) {
	base := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `"],"bot_user_id":"` + botUser + `","signing_secret":"signing","history_hours":1,`
	for extra, ok := range map[string]bool{`"reconcile_seconds":3600}`: true, `"reconcile_seconds":3601}`: false, `"reconcile_seconds":-1}`: false, `"history_page_size":15}`: true, `"history_page_size":201}`: false} {
		if _, err := slack.ParseExtension([]byte(base+extra), []string{"signing"}); (err == nil) != ok {
			t.Fatalf("%s: %v", extra, err)
		}
	}
}

func TestStaleAndForeignGenerationsRestart(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	f.fake.Post(chanA, alice, "q")
	f.sync()
	page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: "foreign", Cursor: "0:h:0:"})
	if err != nil || page.Complete || page.NextCursor != "" {
		t.Fatalf("foreign generation: %+v %v", page, err)
	}
	first, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	f.clock.Add(7 * time.Hour)
	stale, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: first.ScanID, Cursor: first.NextCursor})
	if err != nil || stale.Complete || stale.NextCursor != "" {
		t.Fatalf("stale generation: %+v %v", stale, err)
	}
	bad, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: first.ScanID, Cursor: "9:h:0:"})
	if err != nil || bad.NextCursor != "" || bad.Complete {
		t.Fatalf("bad cursor: %+v %v", bad, err)
	}
}

func TestCredentialRefusalAndRotation(t *testing.T) {
	fake := slacktest.New(team, "old")
	defer fake.Close()
	fake.Join(chanA, bob)
	token := "old"
	var mu sync.Mutex
	cfg, _ := slack.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, err := slack.NewClient(cfg, func(context.Context, string) (string, error) { mu.Lock(); defer mu.Unlock(); return token, nil }, slack.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.RevokeToken("old")
	fake.AddToken("new")
	mu.Lock()
	token = "new"
	mu.Unlock()
	id, err := client.AuthTest(context.Background())
	if err != nil || id.Team != team || id.User != botUser {
		t.Fatalf("rotated token not re-resolved: %+v %v", id, err)
	}
	fake.RevokeToken("new")
	if _, err := client.AuthTest(context.Background()); !errors.Is(err, slack.ErrCredential) {
		t.Fatalf("revoked token: %v", err)
	}
	if _, _, err := client.History(context.Background(), "C0MISSING", "", "", "", 10); err == nil {
		t.Fatal("missing channel")
	}
	fake.AddToken("new")
	if _, _, err := client.History(context.Background(), "C0MISSING", "", "", "", 10); !slack.IsCode(err, "channel_not_found") {
		t.Fatalf("refusal code: %v", err)
	}
}

func TestEventIdentityIsNamespacedByChannelAndThread(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	a := f.fake.Post(chanA, alice, "same text")
	b := f.fake.Post(chanB, alice, "same text")
	f.sync()
	got := byItem(f.scan())
	oa, ob := got[item(chanA, a, a)], got[item(chanB, b, b)]
	if oa.Event.CorrelationKey.ExternalID == ob.Event.CorrelationKey.ExternalID || oa.Event.DeliveryID == ob.Event.DeliveryID || oa.Event.ID == ob.Event.ID {
		t.Fatal("channels share identity")
	}
	if oa.Event.CorrelationKey.Source != ns || oa.Event.Source != ns {
		t.Fatal("namespace")
	}
	var zero domain.Event
	if oa.Event.OccurredAt == zero.OccurredAt {
		t.Fatal("occurrence time")
	}
}
