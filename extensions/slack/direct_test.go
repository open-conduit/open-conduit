package slack_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
)

const (
	dmAlice = "D0ALICE"
	dmBob   = "D0BOB"
)

func TestDirectMessagesConfig(t *testing.T) {
	base := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `"`
	for cfg, ok := range map[string]bool{
		base + `,"channels":["` + chanA + `"],"direct_messages":{"users":["` + alice + `"]}}`: true,
		base + `,"direct_messages":{"anyone":true}}`:                                          true,
		base + `,"direct_messages":{"users":["W0ENTERPRISE"]}}`:                               true,
		base + `}`:                      false, // neither channels nor direct messages
		base + `,"direct_messages":{}}`: false, // says nobody: fails closed
		base + `,"direct_messages":{"anyone":true,"users":["` + alice + `"]}}`:                                           false,
		base + `,"direct_messages":{"users":["` + alice + `","` + alice + `"]}}`:                                         false,
		base + `,"direct_messages":{"users":["C0NOTAUSER"]}}`:                                                            false,
		base + `,"channels":["` + dmAlice + `"]}`:                                                                        false, // a DM is not a watched channel
		base + `,"channels":["` + chanA + `"],"direct_messages":true}`:                                                   false,
		base + `,"direct_messages":{"users":["` + alice + `"]},"post":[{"binding":"b","channels":["` + dmAlice + `"]}]}`: false,
	} {
		if _, err := slack.ParseExtension([]byte(cfg), nil); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok %v", cfg, err, ok)
		}
	}
	if _, err := slack.ParseRoute([]byte(`{"channels":["` + dmAlice + `"]}`)); err == nil {
		t.Error("a route listed a DM as a channel")
	}
	s, err := slack.ParseExtension([]byte(base+`,"direct_messages":{"users":["`+bob+`","`+alice+`"]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Observes(dmAlice) || s.Observes(chanA) || !s.AdmitsDirect(alice) || !s.AdmitsDirect(bob) || s.AdmitsDirect("UCAROL") {
		t.Fatalf("settings: %+v", s)
	}
	if off := settings(t, ""); off.Observes(dmAlice) || off.AdmitsDirect(alice) {
		t.Fatal("direct messages observed without direct_messages")
	}
}

func dmEvent(t *testing.T, channel string, p slack.MessagePayload, received time.Time) domain.AcceptedEvent {
	t.Helper()
	e := event(t, slack.TypeMessageHuman, p, received)
	var q slack.MessagePayload
	_ = json.Unmarshal(e.Event.Data.Data, &q)
	q.Channel = channel
	e.Event.Data.Data, _ = json.Marshal(q)
	e.Event.CorrelationKey.ExternalID = slack.ThreadKey(channel, q.Thread)
	return e
}

func TestRouteStartsDirectMessagesForAdmittedPeople(t *testing.T) {
	sc := scopeWith(t, `,"start":"mention","direct_messages":{"users":["`+alice+`"]}`)
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	root := slack.MessagePayload{TS: slack.TS(written), User: alice, Text: "a private question"}
	d := match(t, sc, dmEvent(t, dmAlice, root, written.Add(time.Second)), nil)
	if !d.Match || d.Kind != slack.WorkKind || d.Reason != "a direct message to the app" {
		t.Fatalf("an admitted person's DM must start a conversation without a mention: %+v", d)
	}
	stranger := root
	stranger.User = bob
	if d := match(t, sc, dmEvent(t, dmBob, stranger, written.Add(time.Second)), nil); d.Match || !strings.Contains(d.Reason, bob) || !strings.Contains(d.Reason, "direct_messages") {
		t.Fatalf("a DM from someone the limit does not admit must start nothing, with a reason: %+v", d)
	}
	if d := match(t, scopeWith(t, ""), dmEvent(t, dmAlice, root, written.Add(time.Second)), nil); d.Match {
		t.Fatalf("a DM started Work on an entry without direct_messages: %+v", d)
	}
	if d := match(t, sc, dmEvent(t, dmAlice, root, written.Add(25*time.Hour)), nil); d.Match {
		t.Fatal("an old DM started a conversation")
	}
	reply := slack.MessagePayload{TS: slack.TS(written.Add(time.Minute)), Thread: root.TS, User: alice, Text: "and another thing"}
	if d := match(t, sc, dmEvent(t, dmAlice, reply, written.Add(time.Minute)), nil); d.Match {
		t.Fatal("a reply in an unowned DM thread started a conversation")
	}
	w := &domain.Work{ID: "w1", Kind: slack.WorkKind, ScopeID: scope}
	if d := match(t, sc, dmEvent(t, dmAlice, reply, written.Add(time.Minute)), w); !d.Match || d.Kind != "" {
		t.Fatalf("a second message in the DM thread must reach its Work: %+v", d)
	}
}

func TestRouteSelectsDirectMessages(t *testing.T) {
	sc := scopeWith(t, `,"direct_messages":{"anyone":true}`)
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := slack.MessagePayload{TS: slack.TS(written), User: alice, Text: "help"}
	dm, inChannel := dmEvent(t, dmAlice, p, written), event(t, slack.TypeMessageHuman, p, written)
	route := func(cfg string) domain.Route {
		return domain.Route{Name: "r", Handler: "slack", Config: domain.Payload{Data: json.RawMessage(cfg)}}
	}
	for cfg, want := range map[string][2]bool{ // {DM, channel}
		``:                               {true, true},
		`{"direct_messages":true}`:       {true, false},
		`{"channels":["` + chanA + `"]}`: {false, true},
		`{"channels":["` + chanA + `"],"direct_messages":true}`: {true, true},
	} {
		for i, e := range []domain.AcceptedEvent{dm, inChannel} {
			d, err := slack.Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: sc, Route: route(cfg)})
			if err != nil || d.Match != want[i] {
				t.Fatalf("config %q, event %d: %+v, want %v (%v)", cfg, i, d, want[i], err)
			}
		}
	}
}

func TestDirectMessagesPushAndReconcile(t *testing.T) {
	f := newFixture(t, `,"direct_messages":{"users":["`+alice+`","`+bob+`"]}`, slack.IngressOptions{PageSize: 1})
	f.fake.AddMember(dmAlice, alice)
	f.fake.AddMember(dmBob, bob)
	f.fake.AddMember(chanA, "support")
	first := f.fake.Post(dmAlice, alice, "can I ask you something?")
	answer := f.fake.Reply(dmAlice, first, alice, "it's about my invoice")
	f.fake.Post(dmAlice, alice, "another question")
	missed := f.fake.Post(dmBob, bob, "a DM push missed")
	f.sync()
	res := f.push(f.fake.MessageEvent(dmAlice, first))
	if len(res.Observations) != 1 || res.Observations[0].Event.CorrelationKey.ExternalID != slack.ThreadKey(dmAlice, first) {
		t.Fatalf("a pushed DM: %+v", res)
	}
	scanned := byItem(f.scan())
	sameIdentity(t, "dm", res.Observations[0], scanned[item(dmAlice, first, first)])
	for _, it := range []string{item(dmAlice, first, answer), item(dmBob, missed, missed)} {
		if _, ok := scanned[it]; !ok {
			t.Fatalf("reconcile missed %s: %v", it, keys(scanned))
		}
	}
	if f.fake.Calls("users.conversations") != 1 {
		t.Fatalf("the app's DMs were listed %d times in one generation", f.fake.Calls("users.conversations"))
	}
	// Negative control: without direct_messages nothing in a DM is seen.
	off := newFixture(t, "", slack.IngressOptions{})
	off.fake.AddMember(dmAlice, alice)
	ts := off.fake.Post(dmAlice, alice, "hello?")
	off.sync()
	if res := off.push(off.fake.MessageEvent(dmAlice, ts)); len(res.Observations) != 0 {
		t.Fatal("a DM was pushed without direct_messages")
	}
	if _, ok := byItem(off.scan())[item(dmAlice, ts, ts)]; ok || off.fake.Calls("users.conversations") != 0 {
		t.Fatal("DMs were reconciled without direct_messages")
	}
}

// TestReconcileReadsOnlyDMsThatCanMatter: reconcile reads the DMs of the
// people direct_messages admits and the DMs live Work owns, not every
// person's who ever wrote to the app; a listing past its bound keeps what
// it read, so the channels and live conversations are still reconciled.
func TestReconcileReadsOnlyDMsThatCanMatter(t *testing.T) {
	f := newFixture(t, `,"direct_messages":{"users":["`+alice+`"]}`, slack.IngressOptions{})
	f.fake.AddMember(dmAlice, alice)
	f.fake.AddMember(dmBob, bob)
	mine := f.fake.Post(dmAlice, alice, "admitted")
	other := f.fake.Post(dmBob, bob, "not admitted")
	f.sync()
	got := byItem(f.scan())
	if _, ok := got[item(dmAlice, mine, mine)]; !ok {
		t.Fatal("an admitted person's DM was not reconciled")
	}
	if _, ok := got[item(dmBob, other, other)]; ok {
		t.Fatal("a DM with someone direct_messages does not admit was read")
	}
	// A DM live Work owns is read whoever it is with.
	f.active[dmBob] = []string{other}
	if _, ok := byItem(f.scan())[item(dmBob, other, other)]; !ok {
		t.Fatal("a DM live Work owns was not reconciled")
	}

	// Past the listing's bound: the channels, the DMs listed and the DMs
	// live Work owns are still read, and the generation completes.
	g := newFixture(t, `,"direct_messages":{"anyone":true}`, slack.IngressOptions{DirectPages: 1})
	g.fake.SetListLimit(1)
	const dmCarol = "D0CAROL"
	for ch, user := range map[string]string{dmAlice: alice, dmBob: bob, dmCarol: "UCAROL"} {
		g.fake.AddMember(ch, user)
	}
	inChannel := g.fake.Post(chanA, alice, "a channel message push missed")
	first := g.fake.Post(dmAlice, alice, "listed")
	unlisted := g.fake.Post(dmBob, bob, "past the bound")
	owned := g.fake.Post(dmCarol, "UCAROL", "past the bound, but live")
	g.active[dmCarol] = []string{owned}
	g.sync()
	got = byItem(g.scan())
	for _, it := range []string{item(chanA, inChannel, inChannel), item(dmAlice, first, first), item(dmCarol, owned, owned)} {
		if _, ok := got[it]; !ok {
			t.Fatalf("a truncated DM listing lost %s: %v", it, keys(got))
		}
	}
	if _, ok := got[item(dmBob, unlisted, unlisted)]; ok {
		t.Fatal("a DM past the listing's bound was read")
	}
}

func TestDirectMessagesOnlyEntryReconciles(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	cfg := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","direct_messages":{"anyone":true}}`
	s, err := slack.ParseExtension([]byte(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := slack.NewIngress(s, f.client, f.clock.Now, slack.IngressOptions{})
	page, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil || !page.Complete || len(page.Observations) != 0 {
		t.Fatalf("no DMs: %+v %v", page, err)
	}
	f.fake.AddMember(dmAlice, alice)
	ts := f.fake.Post(dmAlice, alice, "hi")
	f.sync()
	f.in = in
	if _, ok := byItem(f.scan())[item(dmAlice, ts, ts)]; !ok {
		t.Fatal("a DMs-only entry missed a DM")
	}
}

func TestDoctorRequiresDirectScopesOnlyWhenOn(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	f.fake.SetScopes(slack.BotScopes)
	ctx := context.Background()
	if r := slack.CheckAuth(ctx, f.client, settings(t, "")); r.Severity != slack.DoctorOK {
		t.Fatalf("an entry without DMs needs no im scopes: %+v", r)
	}
	r := slack.CheckAuth(ctx, f.client, settings(t, `,"direct_messages":{"anyone":true}`))
	if r.Severity != slack.DoctorFail || !strings.Contains(r.Item, "im:history") || !strings.Contains(r.Item, "direct_messages") {
		t.Fatalf("an entry with DMs and no im scopes: %+v", r)
	}
	f.fake.SetScopes(append(slices.Clone(slack.BotScopes), slack.DirectScopes...))
	if r := slack.CheckAuth(ctx, f.client, settings(t, `,"direct_messages":{"anyone":true}`)); r.Severity != slack.DoctorOK {
		t.Fatalf("granted: %+v", r)
	}
}

func TestManifestLetsPeopleMessageTheApp(t *testing.T) {
	var m struct {
		Features struct {
			AppHome struct {
				Messages bool `json:"messages_tab_enabled"`
				ReadOnly bool `json:"messages_tab_read_only_enabled"`
			} `json:"app_home"`
		} `json:"features"`
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(slack.ManifestJSON(""), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Features.AppHome.Messages || m.Features.AppHome.ReadOnly || !slices.Contains(m.Settings.Events.Bot, "message.im") {
		t.Fatalf("manifest: %+v", m)
	}
	for _, sc := range slack.DirectScopes {
		if !slices.Contains(m.OAuth.Scopes.Bot, sc) {
			t.Fatalf("manifest lacks %s", sc)
		}
	}
}

func keys(m map[string]extension.Observation) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
