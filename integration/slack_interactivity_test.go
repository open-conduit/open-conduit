package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// slPresses is the Slack extension with one receiver http entry that has
// its own signing_secret, on a connection that names one too, so presses
// reach it over both interactivity Request URLs. Act and CancelWork stand
// in for the host's, keyed as the host's are: the first request with a key
// acts, a repeat answers what it did.
type slPresses struct {
	t     *testing.T
	fake  *slacktest.Server
	store *sqlite.Store
	x     *sl.Extension

	mu      sync.Mutex
	acts    []extension.ActRequest
	acted   map[string]bool
	cancels []string
}

const slPressEntry = `{"namespace":"slack/support","scope":"support","team":"` + slTeam + `","channels":["` + slChannel + `"],"bot_user_id":"UBOT","claim_from":"all",` +
	`"receiver":"http","signing_secret":"signing","progress":{"enabled":true,"stop":["slack:` + slAlice + `"]},"feedback":{"enabled":true}}`

func newSlackPresses(t *testing.T) *slPresses {
	t.Helper()
	fake := slacktest.New(slTeam, "xoxb-test")
	t.Cleanup(fake.Close)
	fake.Join(slChannel, slBob)
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := &slPresses{t: t, fake: fake, store: store, x: sl.New(), acted: map[string]bool{}}
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "helpdesk", "type": "slack", "connection": "app", "config": json.RawMessage(slPressEntry), "secret_names": []string{"signing"}},
	}})
	if err := store.Transact(ctx, storage.Guard{Now: fake.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "support", Name: "support", Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.x.Start(ctx, inproc.Host{Store: store, Now: fake.Now,
		Secrets: func(_ context.Context, _ domain.ID, conn string, names []string) (map[string]string, error) {
			values := map[string]string{sl.SecretBotToken: "xoxb-test", sl.SecretSigning: slSecret}
			if conn == "" {
				values = map[string]string{"signing": "entry-secret"}
			}
			out := map[string]string{}
			for _, n := range names {
				if values[n] == "" {
					return nil, extension.ErrUnauthorized
				}
				out[n] = values[n]
			}
			return out, nil
		},
		Act: func(_ context.Context, r extension.ActRequest) (extension.ActResult, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.acts = append(p.acts, r)
			if p.acted[r.Key] {
				return extension.ActResult{Message: "answered: " + r.Input}, nil
			}
			p.acted[r.Key] = true
			return extension.ActResult{Added: true, Message: "answered: " + r.Input}, nil
		},
		CancelWork: func(_ context.Context, work domain.ID, _ domain.Revision, reason, key string) (domain.ID, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.cancels = append(p.cancels, key)
			return "ev-cancel", nil
		},
		AcceptInTx: func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
			e.ID = domain.ID(protocol.EmittedEventID(e.Source, e.DeliveryID))
			return runtime.AcceptInTx(tx, scope, e)
		}}); err != nil {
		t.Fatal(err)
	}
	conn := protocol.ConnectionEntry{ID: "app", Type: sl.Type, Config: json.RawMessage(`{"api_url":"` + fake.URL + `"}`), Secrets: []string{sl.SecretBotToken, sl.SecretSigning}}
	p.x.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "support", Type: sl.Type, Name: "helpdesk", Config: json.RawMessage(slPressEntry), Secrets: []string{"signing"}, Connection: &conn}}})
	if len(p.x.Sources()) != 1 {
		t.Fatalf("sources %+v", p.x.Sources())
	}
	return p
}

// press posts payload to an interactivity Request URL path (below /slack/),
// signed with secret, and waits for what it started to finish.
func (p *slPresses) press(path, secret string, payload []byte, at time.Time) *httptest.ResponseRecorder {
	p.t.Helper()
	h := p.x.Webhook(path)
	if h == nil {
		p.t.Fatalf("%s is not served", path)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, slacktest.InteractionRequest("/slack/"+path, secret, payload, at))
	return w
}

// settle waits for the presses taken over HTTP to be acted on: they run
// detached from their requests.
func (p *slPresses) settle(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *slPresses) actCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.acts) }

// A press posted to the app's interactivity Request URL, the connection's
// or the entry's own, is answered 200 with an empty body before it is acted
// on, and then handed to Act as the presser with the key a retry repeats,
// so the same press delivered twice acts once. A bad signature is 401, a
// stale timestamp too, and a press from another workspace is acknowledged
// and ignored, and listed by the slack check. The Stop and feedback buttons
// work over it as over Socket Mode.
func TestSlackButtonPressesOverHTTP(t *testing.T) {
	p := newSlackPresses(t)
	t.Cleanup(p.x.Stop)
	now := p.fake.Now()
	root := p.fake.Post(slChannel, slAlice, "Please deploy 1.2")
	prompt := p.fake.BotReply(slChannel, root, "BAPP", "agent asks: Approve?")
	value, _ := json.Marshal(map[string]string{"w": "work-1", "p": "decisions", "a": "answer", "i": "approve"})
	payload := p.fake.Press(slChannel, root, prompt, slAlice, "oc_answer_0", string(value))

	for _, path := range []string{"app/interactivity", "support/helpdesk/interactivity"} {
		if p.x.Webhook(path) == nil {
			t.Fatalf("%s is not served", path)
		}
	}
	if p.x.Webhook("other/interactivity") != nil || p.x.Webhook("support/other/interactivity") != nil {
		t.Fatal("an interactivity URL is served for no entry")
	}

	w := p.press("app/interactivity", slSecret, payload, now)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("press: %d %q", w.Code, w.Body.String())
	}
	p.settle("the press", func() bool { return len(p.fake.Ephemerals()) == 1 })
	// Slack's retry of the same press, to the entry's own URL.
	if w := p.press("support/helpdesk/interactivity", "entry-secret", payload, now); w.Code != http.StatusOK {
		t.Fatalf("retried press: %d", w.Code)
	}
	p.settle("the retried press", func() bool { return p.actCount() == 2 })
	p.mu.Lock()
	acts, acted := append([]extension.ActRequest(nil), p.acts...), len(p.acted)
	p.mu.Unlock()
	if acts[0].Caller != slAlice || acts[0].Work != "work-1" || acts[0].Provider != "decisions" || acts[0].Action != "answer" || acts[0].Input != "approve" || acts[0].Scope != "support" ||
		!strings.HasPrefix(acts[0].Key, "block/"+slAlice+"/") || acts[1].Key != acts[0].Key || acted != 1 {
		t.Fatalf("act requests %+v (%d acted)", acts, acted)
	}
	if eph := p.fake.Ephemerals(); eph[0].User != slAlice || eph[0].Text != "answered: approve" || eph[0].ThreadTS != root {
		t.Fatalf("ephemerals %+v", eph)
	}

	// Refused before anything is parsed or acted on.
	if w := p.press("app/interactivity", "wrong", payload, now); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", w.Code)
	}
	if w := p.press("support/helpdesk/interactivity", slSecret, payload, now); w.Code != http.StatusUnauthorized {
		t.Fatalf("the connection's secret on the entry's URL: %d", w.Code)
	}
	if w := p.press("app/interactivity", slSecret, payload, now.Add(-10*time.Minute)); w.Code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: %d", w.Code)
	}
	// Another workspace's press is acknowledged, and nothing acts on it.
	other := strings.Replace(string(payload), `"id":"`+slTeam+`"`, `"id":"T0OTHER"`, 1)
	if w := p.press("app/interactivity", slSecret, []byte(other), now); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("another team's press: %d", w.Code)
	}
	if p.actCount() != 2 {
		t.Fatalf("another team's press was acted on: %d", p.actCount())
	}
	var check inproc.CheckResult
	for _, c := range p.x.Checks() {
		if !c.Deep {
			check = c.Run(context.Background(), protocol.CheckConfig{})
		}
	}
	joined := strings.Join(check.Items, "\n")
	if !strings.Contains(joined, "POST /slack/app/interactivity: 1 button presses for no configured entry (acknowledged and ignored), the last from team T0OTHER") ||
		!strings.Contains(joined, "POST /slack/app/interactivity (set it as the app's Interactivity Request URL)") {
		t.Fatalf("check items:\n%s", joined)
	}

	// The Stop button: Bob may not stop the run, Alice may.
	if err := p.store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "work-1", ScopeID: "support", Kind: sl.WorkKind, State: domain.WorkReady, CreatedAt: now, UpdatedAt: now,
			CorrelationKeys: []domain.CorrelationKey{{Source: "slack/support", ExternalID: sl.ThreadKey(slChannel, root)}}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	stop, _ := json.Marshal(map[string]string{"w": "work-1"})
	seen := len(p.fake.Ephemerals())
	p.press("app/interactivity", slSecret, p.fake.Press(slChannel, root, prompt, slBob, "oc_stop", string(stop)), now)
	p.settle("bob's stop", func() bool { return len(p.fake.Ephemerals()) == seen+1 })
	if eph := p.fake.Ephemerals(); eph[seen].User != slBob || !strings.Contains(eph[seen].Text, "not one of the people who may stop") {
		t.Fatalf("bob's stop: %+v", eph[seen])
	}
	p.press("app/interactivity", slSecret, p.fake.Press(slChannel, root, prompt, slAlice, "oc_stop", string(stop)), now)
	p.settle("alice's stop", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.cancels) == 1 })

	// A feedback button is recorded on the Work as slack.feedback.
	fb, _ := json.Marshal(map[string]string{"w": "work-1", "v": sl.FeedbackPositive})
	p.press("app/interactivity", slSecret, p.fake.Press(slChannel, root, prompt, slBob, "oc_feedback", string(fb)), now)
	p.settle("the feedback", func() bool {
		n := 0
		_ = p.store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
			all, err := tx.Events(0, 0)
			for _, a := range all {
				if a.Event.Type == sl.TypeFeedback {
					n++
				}
			}
			return err
		})
		return n == 1
	})
}

// An entry on receiver http needs no app_token: it opens no Socket Mode
// connection, and its check is ok.
func TestSlackHTTPReceiverNeedsNoAppToken(t *testing.T) {
	p := newSlackPresses(t)
	t.Cleanup(p.x.Stop)
	for _, task := range p.x.Tasks() {
		if task.Name == "sockets" {
			if err := task.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if p.fake.SocketConnects() != 0 || p.fake.Calls("apps.connections.open") != 0 {
		t.Fatalf("a socket was opened: %d connects", p.fake.SocketConnects())
	}
	for _, c := range p.x.Checks() {
		r := c.Run(context.Background(), protocol.CheckConfig{})
		if r.Status != inproc.CheckOK {
			t.Fatalf("%s: %s %s %v", r.Name, r.Status, r.Code, r.Items)
		}
	}
}
