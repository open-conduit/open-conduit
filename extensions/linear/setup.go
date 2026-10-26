package linear

import (
	"context"
	"errors"
	"flag"
	"io"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// Setup is the Linear extension's `init linear`: one scope working one or
// more teams' issues (v1 wizard outcomes: the tracker binding with its team
// keys and claim states, an optional git work resource, the agent binding
// with the tracker's tools and the route).
type Setup struct{}

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Type, Summary: "add a scope that works Linear teams' issues",
		Usage: "[--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --teams ENG[,OPS] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace] [--states unstarted,started] [--agent-account NAME]"}
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (Setup) Setup(_ context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	fs := flag.NewFlagSet("init linear", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	teams := fs.String("teams", "", "")
	scope := fs.String("scope", "", "")
	token := fs.String("token", "", "")
	connection := fs.String("connection", "", "")
	checkout := fs.String("checkout", "", "")
	noWorkspace := fs.Bool("no-workspace", false, "")
	states := fs.String("states", "", "")
	agent := fs.String("agent-account", "", "")
	bbRepo := fs.String("bitbucket", "", "")
	bbUser := fs.String("bitbucket-username", "", "")
	bbToken := fs.String("bitbucket-token", "", "")
	bbConn := fs.String("bitbucket-connection", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if errors.Is(err, flag.ErrHelp) {
			return extension.SetupResult{}, tracker.SetupUsage("init linear " + Setup{}.SetupInfo().Usage)
		}
		return extension.SetupResult{}, tracker.SetupUsage("invalid init linear flags")
	}
	if *teams == "" {
		answer, err := host.Prompt("Linear team keys (comma-separated, e.g. ENG)", extension.Prompt{})
		if err != nil {
			return extension.SetupResult{}, tracker.SetupUsage("init linear needs --teams ENG[,OPS]")
		}
		*teams = answer
	}
	keys, err := tracker.SplitKeys(split(*teams), teamPattern)
	if err != nil {
		return extension.SetupResult{}, tracker.SetupUsage("--teams takes Linear team keys (ENG,OPS)")
	}
	if *scope == "" {
		*scope = strings.ToLower(strings.ReplaceAll(keys[0], "_", "-"))
		if *scope == "" || (*scope)[0] < 'a' || (*scope)[0] > 'z' {
			*scope = "linear-" + *scope
		}
	}
	if *connection == "" {
		*connection = Type
	}
	if !tracker.IDPattern.MatchString(*scope) || !tracker.IDPattern.MatchString(*connection) {
		return extension.SetupResult{}, tracker.SetupUsage("--scope and --connection must match [a-z][a-z0-9_-]*")
	}
	if *token == "" {
		host.Printf("Create a personal API key in Linear (Settings → Security & access → Personal API keys).\n")
		_ = host.OpenURL("https://linear.app/settings/account/security")
		value, err := host.Prompt("Linear API key", extension.Prompt{Secret: true})
		if err != nil || strings.TrimSpace(value) == "" {
			return extension.SetupResult{}, tracker.SetupUsage("init linear needs --token REF (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND) or an interactive terminal")
		}
		if *token, err = host.StoreSecret(*connection+"-token", strings.TrimSpace(value)); err != nil {
			return extension.SetupResult{}, err
		}
	}
	if !tracker.RefPattern.MatchString(*token) {
		return extension.SetupResult{}, tracker.SetupUsage("--token takes a reference (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND), never a key")
	}
	dir := *checkout
	if dir != "" && !strings.HasPrefix(dir, "/") {
		dir = strings.TrimSuffix(host.Cwd(), "/") + "/" + dir
	}
	if *noWorkspace {
		dir = ""
	}
	entry := map[string]any{"teams": keys}
	if s := split(*states); len(s) > 0 {
		entry["states"] = s
	}
	cfg := map[string]any{}
	if *agent != "" {
		cfg["agent_account"] = *agent
	}
	spec := tracker.ScopeSpec{Type: Type, Scope: *scope, Name: "Linear " + strings.Join(keys, ","), Connection: *connection,
		ConnectionConfig: cfg, Secrets: map[string]string{SecretToken: *token}, Entry: entry,
		Namespace: "linear/" + strings.ToLower(strings.Join(keys, "+")), Checkout: dir,
		Summary: []string{"teams      " + strings.Join(keys, ", ")}}
	if *bbRepo != "" {
		if !bitbucketRepo.MatchString(*bbRepo) {
			return extension.SetupResult{}, tracker.SetupUsage("--bitbucket takes workspace/slug")
		}
		if *bbToken == "" || !tracker.RefPattern.MatchString(*bbToken) {
			return extension.SetupResult{}, tracker.SetupUsage("--bitbucket needs --bitbucket-token REF (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND)")
		}
		if *bbConn != "" && !tracker.IDPattern.MatchString(*bbConn) {
			return extension.SetupResult{}, tracker.SetupUsage("--bitbucket-connection must match [a-z][a-z0-9_-]*")
		}
		spec.CodeHost = tracker.BitbucketCodeHost(*bbRepo, *bbUser, *bbToken, *bbConn, spec.Namespace, keys, "Closes {key}")
	}
	return spec.Result()
}

var bitbucketRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
