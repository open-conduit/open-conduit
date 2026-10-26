package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// scriptedHost answers prompts and choices in order and records what the
// setup did.
type scriptedHost struct {
	answers []string
	choices []int
	opened  []string
	stored  map[string]string
	paths   map[string]string
	ran     [][]string
	state   string
	dry     bool
	out     strings.Builder
}

func (h *scriptedHost) Printf(format string, args ...any) { fmt.Fprintf(&h.out, format, args...) }
func (h *scriptedHost) Prompt(q string, p extension.Prompt) (string, error) {
	if len(h.answers) == 0 {
		return "", extension.ErrSetupNoInput
	}
	a := h.answers[0]
	h.answers = h.answers[1:]
	return a, nil
}
func (h *scriptedHost) Choose(q string, options []string) (int, error) {
	fmt.Fprintf(&h.out, "%s %v\n", q, options)
	if len(h.choices) == 0 {
		return -1, extension.ErrSetupNoInput
	}
	c := h.choices[0]
	h.choices = h.choices[1:]
	return c, nil
}
func (h *scriptedHost) OpenURL(u string) error { h.opened = append(h.opened, u); return nil }
func (h *scriptedHost) StoreSecret(name, value string) (string, error) {
	if h.stored == nil {
		h.stored = map[string]string{}
	}
	h.stored[name] = value
	return "keyring:" + name, nil
}
func (h *scriptedHost) LookPath(name string) (string, bool) { p, ok := h.paths[name]; return p, ok }
func (h *scriptedHost) Run(_ context.Context, dir, name string, args ...string) error {
	h.ran = append(h.ran, append([]string{dir, name}, args...))
	return nil
}
func (h *scriptedHost) Config() []byte   { return nil }
func (h *scriptedHost) Cwd() string      { return h.state }
func (h *scriptedHost) StateDir() string { return h.state }
func (h *scriptedHost) DryRun() bool     { return h.dry }

type supportScope struct {
	ID       string `json:"id"`
	Bindings []struct {
		Executor   string          `json:"executor"`
		Resource   json.RawMessage `json:"resource"`
		Requires   map[string]any  `json:"requires"`
		Operations []string        `json:"operations"`
		Config     map[string]any  `json:"config"`
	} `json:"bindings"`
	Routes []struct {
		Handler string `json:"handler"`
		Policy  string `json:"active_policy"`
	} `json:"routes"`
	Extensions []struct {
		Type   string                `json:"type"`
		Config slack.ExtensionConfig `json:"config"`
	} `json:"extensions"`
}

func TestSlackSetupCreatesVerifiesAndPicksAChannel(t *testing.T) {
	fake := slacktest.New("T0123", "xoxb-good")
	defer fake.Close()
	fake.AddAppToken("xapp-good")
	fake.AddMember("C0SUPPORT", "support")
	fake.AddMember("C0OTHER", "random")
	h := &scriptedHost{state: t.TempDir(), answers: []string{"not-a-token", "xoxb-good", "xapp-good"}, choices: []int{0}}
	r, err := slack.Setup{}.Setup(context.Background(), h, []string{"--api-url", fake.URL, "--name", "Support Bot"})
	if err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if len(h.opened) != 1 || !strings.Contains(h.opened[0], "manifest_json=") || !strings.Contains(h.out.String(), "connections:write") {
		t.Fatalf("create-app guidance: %v\n%s", h.opened, h.out.String())
	}
	if h.stored["slack-bot-token"] != "xoxb-good" || h.stored["slack-app-token"] != "xapp-good" {
		t.Fatalf("stored: %v", h.stored)
	}
	for _, method := range []string{"auth.test", "apps.connections.open", "users.conversations"} {
		if fake.Calls(method) == 0 {
			t.Fatalf("%s not called", method)
		}
	}
	var conn struct {
		Config     map[string]string `json:"config"`
		SecretRefs map[string]string `json:"secret_refs"`
	}
	if err = json.Unmarshal(r.Connections[0], &conn); err != nil || conn.SecretRefs["bot_token"] != "keyring:slack-bot-token" || conn.SecretRefs["app_token"] != "keyring:slack-app-token" {
		t.Fatalf("connection: %v %s", err, r.Connections[0])
	}
	if strings.Contains(string(r.Connections[0])+string(r.Scope), "xoxb-good") {
		t.Fatal("a token leaked into the config")
	}
	var s supportScope
	if err = json.Unmarshal(r.Scope, &s); err != nil {
		t.Fatal(err)
	}
	b := s.Bindings[0]
	if s.ID != "support" || b.Executor != "claude-code" || b.Resource != nil || b.Requires["workspace"] != false || b.Requires["inject"] != true || b.Config["playbook"] != slack.SupportPlaybook {
		t.Fatalf("binding: %s", r.Scope)
	}
	if len(s.Routes) != 1 || s.Routes[0].Handler != "slack" || s.Routes[0].Policy != "inject" {
		t.Fatalf("routes: %s", r.Scope)
	}
	x := s.Extensions[0].Config
	if x.Team != "T0123" || x.Namespace != "slack/T0123" || x.BotUserID != fake.BotUser() || len(x.Channels) != 1 || x.Channels[0] != "C0OTHER" || x.Receiver != "socket" {
		// Channels list sorted by ID: C0OTHER, C0SUPPORT; choice 0.
		t.Fatalf("entry: %+v", x)
	}
	if strings.Contains(string(r.Scope), "git") {
		t.Fatalf("a Git concept leaked into the support scope: %s", r.Scope)
	}
}

func TestSlackSetupRefusesBadTokensBeforeStoringAnything(t *testing.T) {
	fake := slacktest.New("T0123", "xoxb-good")
	defer fake.Close()
	h := &scriptedHost{state: t.TempDir(), answers: []string{"xoxb-bad", "xapp-bad"}}
	if _, err := (slack.Setup{}).Setup(context.Background(), h, []string{"--api-url", fake.URL, "--no-open"}); err == nil || !strings.Contains(err.Error(), "auth.test") {
		t.Fatalf("bad bot token: %v", err)
	}
	h = &scriptedHost{state: t.TempDir(), answers: []string{"xoxb-good", "xapp-bad"}}
	if _, err := (slack.Setup{}).Setup(context.Background(), h, []string{"--api-url", fake.URL, "--no-open"}); err == nil || !strings.Contains(err.Error(), "apps.connections.open") {
		t.Fatalf("bad app token: %v", err)
	}
	if len(h.stored) != 0 || len(h.opened) != 0 {
		t.Fatalf("stored %v, opened %v", h.stored, h.opened)
	}
}

func TestSlackSetupWarnsAboutMissingScopesAndTakesAnInvitedChannel(t *testing.T) {
	fake := slacktest.New("T0123", "xoxb-good")
	defer fake.Close()
	fake.AddAppToken("xapp-good")
	fake.SetScopes([]string{"channels:history", "chat:write"})
	h := &scriptedHost{state: t.TempDir(), answers: []string{"xoxb-good", "xapp-good", "C0PASTED"}}
	r, err := slack.Setup{}.Setup(context.Background(), h, []string{"--api-url", fake.URL, "--no-open", "--start", "mention"})
	if err != nil {
		t.Fatal(err, h.out.String())
	}
	if !strings.Contains(h.out.String(), slack.MarkerScope) || !strings.Contains(strings.Join(r.Summary, "\n"), "lacks") {
		t.Fatalf("missing-scope warning: %s %v", h.out.String(), r.Summary)
	}
	var s supportScope
	if err = json.Unmarshal(r.Scope, &s); err != nil || s.Extensions[0].Config.Channels[0] != "C0PASTED" || s.Extensions[0].Config.Start != "mention" {
		t.Fatalf("%v %s", err, r.Scope)
	}
}

func TestSlackSetupFlagsAndSlackCLI(t *testing.T) {
	h := &scriptedHost{state: t.TempDir()}
	for _, args := range [][]string{{"--channel", "general"}, {"--start", "never"}, {"--bot-token", "keyring:a"}, {"--bot-token", "xoxb-raw", "--app-token", "xapp-raw", "--team", "T1", "--channel", "C1"},
		{"--bot-token", "keyring:a", "--app-token", "keyring:b"}, {"--via", "slack-cli"}, {"--via", "other"}, {"extra"}} {
		if _, err := (slack.Setup{}).Setup(context.Background(), h, args); !errors.Is(err, extension.ErrSetupUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
	// References skip prompting and verification.
	r, err := slack.Setup{}.Setup(context.Background(), h, []string{"--bot-token", "env:BOT", "--app-token", "env:APP", "--team", "T0123", "--channel", "C0SUPPORT", "--scope", "help"})
	if err != nil || len(h.opened) != 0 || len(h.stored) != 0 || !strings.Contains(strings.Join(r.Summary, "\n"), "doctor --check-secrets") {
		t.Fatalf("references: %v %v %v", err, h.opened, r.Summary)
	}
	// --via slack-cli prepares a local-manifest project and runs the CLI.
	fake := slacktest.New("T0123", "xoxb-good")
	defer fake.Close()
	fake.AddAppToken("xapp-good")
	h = &scriptedHost{state: t.TempDir(), paths: map[string]string{"slack": "/usr/local/bin/slack"}, answers: []string{"xoxb-good", "xapp-good"}}
	if _, err = (slack.Setup{}).Setup(context.Background(), h, []string{"--api-url", fake.URL, "--via", "slack-cli", "--channel", "C0SUPPORT"}); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 || strings.Join(h.ran[0][1:], " ") != "slack app install --environment deployed" || len(h.opened) != 0 {
		t.Fatalf("slack CLI: %v %v", h.ran, h.opened)
	}
	manifest, err := os.ReadFile(filepath.Join(h.ran[0][0], "manifest.json"))
	if err != nil || string(manifest) != string(slack.ManifestJSON("Open Conduit")) {
		t.Fatalf("manifest: %v", err)
	}
	if hooks, _ := os.ReadFile(filepath.Join(h.ran[0][0], ".slack", "hooks.json")); !strings.Contains(string(hooks), "get-manifest") {
		t.Fatal("no get-manifest hook")
	}
}

func TestCheckAuthComparesGrantedScopesWithTheManifest(t *testing.T) {
	fake := slacktest.New("T0123", "xoxb-good")
	defer fake.Close()
	cfg, _ := slack.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, err := slack.NewClient(cfg, func(context.Context, string) (string, error) { return "xoxb-good", nil }, slack.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	settings := func(team string) slack.Settings {
		s, err := slack.ParseExtension([]byte(`{"namespace":"slack/`+team+`","scope":"support","team":"`+team+`","channels":["C0SUPPORT"],"bot_user_id":"`+fake.BotUser()+`"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ctx := context.Background()
	if r := slack.CheckAuth(ctx, client, settings("T0123")); r.Severity != slack.DoctorOK || r.Code != "verified" {
		t.Fatalf("full scopes: %+v", r)
	}
	fake.SetScopes([]string{"channels:history", "groups:history", "chat:write"})
	if r := slack.CheckAuth(ctx, client, settings("T0123")); r.Severity != slack.DoctorWarn || r.Code != "scopes_missing" || !strings.Contains(r.Item, slack.MarkerScope) || !strings.Contains(r.Item, "text fallback") {
		t.Fatalf("missing optional scopes: %+v", r)
	}
	fake.SetScopes([]string{"channels:history"})
	if r := slack.CheckAuth(ctx, client, settings("T0123")); r.Severity != slack.DoctorFail || !strings.Contains(r.Item, "chat:write") {
		t.Fatalf("missing chat:write: %+v", r)
	}
	if r := slack.CheckAuth(ctx, client, settings("T9999")); r.Severity != slack.DoctorFail || r.Code != "team_mismatch" {
		t.Fatalf("team: %+v", r)
	}
	fake.RevokeToken("xoxb-good")
	if r := slack.CheckAuth(ctx, client, settings("T0123")); r.Severity != slack.DoctorFail || strings.Contains(r.Item, "xoxb") {
		t.Fatalf("revoked: %+v", r)
	}
}
