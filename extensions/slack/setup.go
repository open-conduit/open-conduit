package slack

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Setup (04-02) is the Slack extension's `open-conduit init slack`, behind
// the generic extension.Setup hook: create the app from the manifest (Slack's
// create-app page, or the slack CLI when it is on PATH and asked for), take
// the bot and app-level tokens, verify them (auth.test, granted scopes,
// apps.connections.open), pick a channel the bot is in and return a
// workspace-free support scope: a claude-code binding on the
// support-conversation playbook with the slack.* tools, the slack route with
// active policy inject, and the Socket Mode ingress entry.
type Setup struct{}

// SupportOperations are the toolbelt operations of the support binding.
var SupportOperations = []string{"read_context", "wait", "declare_outcome", OpContext, OpThread, OpReply}

// SupportWaitSeconds is the support playbook's default wait on the thread
// (seven days): long enough that a conversation stays parked over a weekend,
// bounded so an abandoned one is handed back.
const SupportWaitSeconds = 7 * 24 * 60 * 60

// SupportPlaybook is the skill the support binding starts its turns on.
const SupportPlaybook = "support-conversation"

var (
	refPattern     = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)
	channelPattern = regexp.MustCompile(`^[CG][A-Z0-9]{2,31}$`)
)

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Type, Summary: "add a workspace-free support scope that answers Slack threads",
		Usage: "[--scope ID] [--name APP] [--channel C…] [--start all|mention] [--bot-token REF --app-token REF --team T…] [--via slack-cli] [--no-open] [--connection ID]"}
}

func setupUsage(msg string) error { return fmt.Errorf("%w: %s", extension.ErrSetupUsage, msg) }

type setupFlags struct {
	scope, name, connection, channel, start, apiURL, botRef, appRef, team, botUser, via string
	noOpen                                                                              bool
}

func (Setup) Setup(ctx context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	var f setupFlags
	fs := flag.NewFlagSet("init slack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.scope, "scope", "support", "")
	fs.StringVar(&f.name, "name", "Open Conduit", "")
	fs.StringVar(&f.connection, "connection", Type, "")
	fs.StringVar(&f.channel, "channel", "", "")
	fs.StringVar(&f.start, "start", StartAll, "")
	fs.StringVar(&f.apiURL, "api-url", "", "")
	fs.StringVar(&f.botRef, "bot-token", "", "")
	fs.StringVar(&f.appRef, "app-token", "", "")
	fs.StringVar(&f.team, "team", "", "")
	fs.StringVar(&f.botUser, "bot-user", "", "")
	fs.StringVar(&f.via, "via", "", "")
	fs.BoolVar(&f.noOpen, "no-open", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return extension.SetupResult{}, setupUsage("init slack " + Setup{}.SetupInfo().Usage)
	}
	switch {
	case !idPattern.MatchString(f.scope) || !idPattern.MatchString(f.connection):
		return extension.SetupResult{}, setupUsage("--scope and --connection must match [a-z][a-z0-9_-]*")
	case f.start != StartAll && f.start != StartMention:
		return extension.SetupResult{}, setupUsage("--start is all or mention")
	case f.channel != "" && !channelPattern.MatchString(f.channel):
		return extension.SetupResult{}, setupUsage("--channel takes a channel ID (C… or G…), not a name")
	case f.via != "" && f.via != "slack-cli":
		return extension.SetupResult{}, setupUsage("--via takes slack-cli")
	case (f.botRef == "") != (f.appRef == ""):
		return extension.SetupResult{}, setupUsage("give both --bot-token and --app-token references, or neither (to be prompted)")
	case f.botRef != "" && (!refPattern.MatchString(f.botRef) || !refPattern.MatchString(f.appRef)):
		return extension.SetupResult{}, setupUsage("--bot-token and --app-token take references (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND), never tokens")
	case f.botRef != "" && (!slackIDPattern.MatchString(f.team) || f.channel == ""):
		return extension.SetupResult{}, setupUsage("with token references, also give --team T… and --channel C… (nothing can be verified without the tokens)")
	case strings.TrimSpace(f.name) == "" || len(f.name) > 35:
		return extension.SetupResult{}, setupUsage("--name is the app's name (1-35 characters)")
	}
	conn := ConnectionConfig{APIURL: f.apiURL}
	if f.apiURL != "" {
		if _, err := ParseConnection([]byte(`{"api_url":` + quote(f.apiURL) + `}`)); err != nil {
			return extension.SetupResult{}, setupUsage("--api-url must be an http(s) URL")
		}
	}
	var summary []string
	if f.botRef == "" {
		if err := createApp(ctx, host, f); err != nil {
			return extension.SetupResult{}, err
		}
		v, err := verifyTokens(ctx, host, f, conn)
		if err != nil {
			return extension.SetupResult{}, err
		}
		f.team, f.botUser, f.channel = v.team, v.botUser, v.channel
		if f.botRef, err = host.StoreSecret(f.connection+"-bot-token", v.bot); err != nil {
			return extension.SetupResult{}, fmt.Errorf("storing the bot token: %w", err)
		}
		if f.appRef, err = host.StoreSecret(f.connection+"-app-token", v.app); err != nil {
			return extension.SetupResult{}, fmt.Errorf("storing the app token: %w", err)
		}
		summary = append(summary, "verified   auth.test as "+v.botUser+" in "+v.team+"; Socket Mode connection opened")
		if len(v.missing) > 0 {
			summary = append(summary, "warning    the bot token lacks "+strings.Join(v.missing, ", ")+": reinstall the app from the manifest")
		}
	} else {
		summary = append(summary, "verified   no (token references given): run open-conduit doctor --check-secrets after serve starts")
	}
	r, err := result(f, conn)
	if err != nil {
		return extension.SetupResult{}, err
	}
	r.Summary = append([]string{"scope      " + f.scope + " (workspace-free: no checkout, no Git provider)",
		"channel    " + f.channel + " in team " + f.team + " (receiver socket, start " + f.start + ")",
		"agent      claude-code on the " + SupportPlaybook + " playbook, active policy inject",
		"tokens     " + f.connection + " connection: bot " + refScheme(f.botRef) + ", app " + refScheme(f.appRef)}, summary...)
	r.Next = []string{"open-conduit serve                   start the daemon (Socket Mode needs no public endpoint)",
		"open-conduit doctor --check-secrets  check the tokens, granted scopes and the socket",
		"invite the app to more channels and add their IDs with: open-conduit config set /scopes/" + f.scope + "/extensions/" + f.scope + "/config/channels '[…]'"}
	return r, nil
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func refScheme(ref string) string {
	scheme, _, _ := strings.Cut(ref, ":")
	return scheme + ":…"
}

// createApp opens Slack's create-app page with the manifest, or creates the
// app with the slack CLI (--via slack-cli, only when it is on PATH).
func createApp(ctx context.Context, host extension.SetupHost, f setupFlags) error {
	_, haveCLI := host.LookPath("slack")
	if f.via == "slack-cli" {
		if !haveCLI {
			return setupUsage("--via slack-cli needs the slack CLI on PATH; leave --via out to use the create-app page")
		}
		dir := filepath.Join(host.StateDir(), "slack-app", f.scope)
		if err := writeCLIProject(dir, f.name); err != nil {
			return fmt.Errorf("preparing the slack CLI project: %w", err)
		}
		host.Printf("Creating the app %q with the slack CLI from %s (the manifest is the project's local manifest).\n", f.name, dir)
		if host.DryRun() {
			host.Printf("Dry run: would run `slack app install --environment deployed` in %s.\n", dir)
		} else if err := host.Run(ctx, dir, "slack", "app", "install", "--environment", "deployed"); err != nil {
			return fmt.Errorf("slack app install failed: %w", err)
		}
	} else {
		url := CreateAppURL(f.name)
		host.Printf("Step 1. Create the Slack app from Open Conduit's manifest (Socket Mode, a bot user, the scopes and events it needs).\n")
		if f.noOpen {
			host.Printf("Open: %s\n", url)
		} else if err := host.OpenURL(url); err != nil {
			host.Printf("Could not open a browser; open: %s\n", url)
		}
		host.Printf("Choose the workspace, check the manifest and press Create, then Install to Workspace.\n")
		if haveCLI {
			host.Printf("(The slack CLI is on PATH: init slack --via slack-cli creates the app with it instead.)\n")
		}
	}
	host.Printf("\nStep 2. Copy the tokens:\n" +
		"  - Bot token: OAuth & Permissions -> Bot User OAuth Token (xoxb-...).\n" +
		"  - App-level token: Basic Information -> App-Level Tokens -> Generate Token and Scopes,\n" +
		"    add the scope connections:write, generate, and copy it (xapp-...). Slack has no API for this step.\n" +
		"Then invite the app to the support channel: /invite @" + f.name + "\n\n")
	return nil
}

// writeCLIProject is a minimal slack CLI project whose manifest is local:
// the get-manifest hook prints manifest.json.
func writeCLIProject(dir, name string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".slack"), 0o700); err != nil {
		return err
	}
	files := map[string][]byte{
		"manifest.json":      ManifestJSON(name),
		".slack/hooks.json":  []byte(`{"hooks": {"get-manifest": "cat manifest.json"}}` + "\n"),
		".slack/config.json": []byte(`{"manifest": {"source": "local"}}` + "\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type verified struct {
	bot, app, team, botUser, channel string
	missing                          []string
}

// verifyTokens prompts for both tokens, checks them against Slack and picks
// the channel. Nothing is stored until both verify.
func verifyTokens(ctx context.Context, host extension.SetupHost, f setupFlags, conn ConnectionConfig) (verified, error) {
	var v verified
	ask := func(question, prefix string) (string, error) {
		for tries := 0; tries < 3; tries++ {
			value, err := host.Prompt(question, extension.Prompt{Secret: true})
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(value, prefix) {
				return value, nil
			}
			host.Printf("That does not look like a %s token; paste the token itself.\n", prefix+"…")
		}
		return "", errors.New("no valid token entered")
	}
	var err error
	if v.bot, err = ask("Bot token (xoxb-…)", "xoxb-"); err != nil {
		return v, err
	}
	if v.app, err = ask("App-level token (xapp-…, connections:write)", "xapp-"); err != nil {
		return v, err
	}
	cfg, err := ParseConnection(mustJSON(conn))
	if err != nil {
		return v, err
	}
	secrets := map[string]string{SecretBotToken: v.bot, SecretAppToken: v.app}
	client, err := NewClient(cfg, func(_ context.Context, name string) (string, error) { return secrets[name], nil }, ClientOptions{})
	if err != nil {
		return v, err
	}
	cx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := client.AuthTest(cx)
	if err != nil {
		return v, fmt.Errorf("the bot token was refused (auth.test %s)", ErrorCode(err))
	}
	v.team, v.botUser, v.missing = id.Team, id.User, MissingScopes(id.Scopes)
	if id.Scopes == nil {
		v.missing = nil
	}
	if len(v.missing) > 0 {
		host.Printf("Warning: the bot token lacks %s; reinstall the app from the manifest to grant them.\n", strings.Join(v.missing, ", "))
	}
	if _, err := client.OpenConnection(cx); err != nil {
		return v, fmt.Errorf("the app-level token cannot open Socket Mode (apps.connections.open %s): it needs connections:write and Socket Mode on", ErrorCode(err))
	}
	host.Printf("Verified: bot %s in workspace %s; Socket Mode connection allowed.\n", id.User, id.Team)
	v.channel, err = pickChannel(cx, host, client, f)
	return v, err
}

func pickChannel(ctx context.Context, host extension.SetupHost, client *Client, f setupFlags) (string, error) {
	for round := 0; round < 2; round++ {
		channels, err := client.Channels(ctx)
		if err != nil {
			host.Printf("Could not list the bot's channels (%s).\n", ErrorCode(err))
			channels = nil
		}
		if f.channel != "" {
			for _, c := range channels {
				if c.ID == f.channel {
					return f.channel, nil
				}
			}
			if err == nil {
				host.Printf("Warning: the bot is not a member of %s yet; invite it there (/invite @%s) or it reads nothing.\n", f.channel, f.name)
			}
			return f.channel, nil
		}
		if len(channels) > 0 {
			names := make([]string, len(channels))
			for i, c := range channels {
				names[i] = "#" + c.Name + " (" + c.ID + ")"
			}
			i, err := host.Choose("Which channel should the support agent answer in?", names)
			if err != nil {
				return "", err
			}
			return channels[i].ID, nil
		}
		if round == 0 {
			host.Printf("The bot is in no channel yet. Invite it (/invite @%s) in the support channel.\n", f.name)
			answer, err := host.Prompt("Press Enter once it is invited, or paste the channel ID", extension.Prompt{})
			if err != nil {
				return "", err
			}
			if channelPattern.MatchString(answer) {
				return answer, nil
			}
		}
	}
	return "", errors.New("no channel: invite the app to a channel and run init slack again (or pass --channel C…)")
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// result builds the connection and the workspace-free support scope.
func result(f setupFlags, conn ConnectionConfig) (extension.SetupResult, error) {
	connection := map[string]any{"id": f.connection, "type": Type, "config": conn,
		"secret_refs": map[string]any{SecretBotToken: f.botRef, SecretAppToken: f.appRef}}
	requires := map[string]any{"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false}
	support := map[string]any{"name": "support", "executor": "claude-code", "connection": f.connection, "requires": requires,
		"operations": SupportOperations, "secret_names": []string{},
		"config": map[string]any{"playbook": SupportPlaybook, "args": "{key}"}}
	entry := ExtensionConfig{Namespace: "slack/" + f.team, Scope: f.scope, Team: f.team, Channels: []string{f.channel}, Start: f.start, BotUserID: f.botUser, Receiver: ReceiverSocket}
	scope := map[string]any{"id": f.scope, "name": "Slack support",
		"authorization": map[string]any{"connections": []string{f.connection}, "read_scopes": []string{}, "operations": SupportOperations},
		"bindings":      []any{support},
		"routes":        []any{map[string]any{"name": "conversations", "handler": Type, "binding": "support", "active_policy": "inject", "config": map[string]any{}}},
		"extensions":    []any{map[string]any{"name": f.scope, "type": Type, "connection": f.connection, "secret_names": []string{}, "config": entry}},
		"secret_refs":   map[string]any{}}
	c, err := json.Marshal(connection)
	if err != nil {
		return extension.SetupResult{}, err
	}
	s, err := json.Marshal(scope)
	if err != nil {
		return extension.SetupResult{}, err
	}
	return extension.SetupResult{Connections: []json.RawMessage{c}, Scope: s}, nil
}
