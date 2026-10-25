package slack_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/slack"
)

// TestManifestCoversWhatTheExtensionUses exercises every Slack call the
// extension makes (reconcile, push, reply, marker search, tools' reads,
// auth.test and Socket Mode) against the fake and checks each method is
// permitted by a manifest scope, and that the manifest subscribes to the
// events push observes.
func TestManifestCoversWhatTheExtensionUses(t *testing.T) {
	var m struct {
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			Socket bool `json:"socket_mode_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(slack.ManifestJSON(""), &m); err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, s := range m.OAuth.Scopes.Bot {
		granted[s] = true
	}
	if !m.Settings.Socket || len(slack.MissingScopes(m.OAuth.Scopes.Bot)) != 0 {
		t.Fatalf("manifest: %+v", m)
	}
	events := strings.Join(m.Settings.Events.Bot, ",")
	for _, e := range []string{"message.channels", "message.groups"} {
		if !strings.Contains(events, e) {
			t.Fatalf("manifest lacks event %s", e)
		}
	}

	f := newFixture(t, "", slack.IngressOptions{})
	f.fake.AddAppToken("xapp-test")
	root := f.fake.Post(chanA, alice, "q")
	f.fake.Reply(chanA, root, bob, "a")
	f.sync()
	f.scan()
	f.push(f.fake.MessageEvent(chanA, root))
	if id, err := f.client.AuthTest(context.Background()); err != nil || len(slack.MissingScopes(id.Scopes)) != 0 {
		t.Fatal(err, id.Scopes)
	}
	if _, err := f.client.Channels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.PostMessage(context.Background(), slack.Post{Channel: chanA, ThreadTS: root, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := slack.ParseConnection([]byte(`{"api_url":"` + f.fake.URL + `"}`))
	app, _ := slack.NewClient(cfg, func(context.Context, string) (string, error) { return "xapp-test", nil }, slack.ClientOptions{})
	sock := slack.NewSocket(app, slack.SocketOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sock.Run(ctx, func(context.Context, extension.Delivery) error { return nil }); close(done) }()
	until(t, "socket", func() bool { return sock.Status().Connected })
	cancel()
	<-done
	for _, method := range f.fake.Methods() {
		if scopes, ok := slack.AppMethods[method]; ok {
			if scopes[0] != slack.AppTokenScopes[0] {
				t.Fatalf("%s needs an app-token scope outside the manifest flow", method)
			}
			continue
		}
		scopes, ok := slack.MethodScopes[method]
		if !ok {
			t.Fatalf("the extension calls %s, which MethodScopes does not declare", method)
		}
		permitted := len(scopes) == 0
		for _, s := range scopes {
			permitted = permitted || granted[s]
		}
		if !permitted {
			t.Fatalf("%s needs one of %v; the manifest grants %v", method, scopes, m.OAuth.Scopes.Bot)
		}
	}
	if len(f.fake.Methods()) < 5 {
		t.Fatalf("scenario exercised too little: %v", f.fake.Methods())
	}
	u, err := url.Parse(slack.CreateAppURL("Support Bot"))
	if err != nil || u.Query().Get("new_app") != "1" || !strings.Contains(u.Query().Get("manifest_json"), "Support Bot") {
		t.Fatalf("create-app URL: %v", u)
	}
	if got := slack.MissingScopes([]string{"chat:write"}); len(got) != len(slack.BotScopes)-1 {
		t.Fatalf("missing scopes: %v", got)
	}
}
