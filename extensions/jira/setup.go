package jira

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

// Setup is the Jira extension's `init jira`: one scope working one or more
// projects' issues (v1 wizard outcomes: the site and its authentication, the
// project keys, extra JQL and route fields, an optional git work resource,
// the agent binding with the tracker's tools and the route).
type Setup struct{}

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Type, Summary: "add a scope that works Jira projects' issues",
		Usage: "[--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --site https://acme.atlassian.net --projects PROJ[,WEB] [--email E | --auth bearer] [--jql JQL] [--fields customfield_1,…] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace]"}
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
	fs := flag.NewFlagSet("init jira", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	site := fs.String("site", "", "")
	projects := fs.String("projects", "", "")
	email := fs.String("email", "", "")
	auth := fs.String("auth", "basic", "")
	jql := fs.String("jql", "", "")
	fields := fs.String("fields", "", "")
	scope := fs.String("scope", "", "")
	token := fs.String("token", "", "")
	connection := fs.String("connection", "", "")
	checkout := fs.String("checkout", "", "")
	noWorkspace := fs.Bool("no-workspace", false, "")
	bbRepo := fs.String("bitbucket", "", "")
	bbUser := fs.String("bitbucket-username", "", "")
	bbToken := fs.String("bitbucket-token", "", "")
	bbConn := fs.String("bitbucket-connection", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if errors.Is(err, flag.ErrHelp) {
			return extension.SetupResult{}, tracker.SetupUsage("init jira " + Setup{}.SetupInfo().Usage)
		}
		return extension.SetupResult{}, tracker.SetupUsage("invalid init jira flags")
	}
	ask := func(v *string, question string) error {
		if *v != "" {
			return nil
		}
		answer, err := host.Prompt(question, extension.Prompt{})
		if err != nil {
			return err
		}
		*v = strings.TrimSpace(answer)
		return nil
	}
	if ask(site, "Jira site URL (https://acme.atlassian.net)") != nil || ask(projects, "Jira project keys (comma-separated, e.g. PROJ)") != nil {
		return extension.SetupResult{}, tracker.SetupUsage("init jira needs --site URL and --projects PROJ")
	}
	if *auth == "basic" && ask(email, "Atlassian account email") != nil {
		return extension.SetupResult{}, tracker.SetupUsage("basic auth needs --email (or use --auth bearer)")
	}
	cfg := map[string]any{"base_url": strings.TrimRight(*site, "/"), "auth": *auth}
	if *auth == "basic" {
		cfg["email"] = *email
	}
	conn := ConnectionConfig{BaseURL: strings.TrimRight(*site, "/"), Auth: *auth, Email: *email}
	if *auth != "basic" {
		conn.Email = ""
	}
	if _, err := ParseConnection(mustJSON(conn)); err != nil {
		return extension.SetupResult{}, tracker.SetupUsage(err.Error())
	}
	keys, err := tracker.SplitKeys(split(*projects), projectPattern)
	if err != nil {
		return extension.SetupResult{}, tracker.SetupUsage("--projects takes Jira project keys (PROJ,WEB)")
	}
	if *scope == "" {
		*scope = strings.ToLower(strings.ReplaceAll(keys[0], "_", "-"))
	}
	if *connection == "" {
		*connection = Type
	}
	if !tracker.IDPattern.MatchString(*scope) || !tracker.IDPattern.MatchString(*connection) {
		return extension.SetupResult{}, tracker.SetupUsage("--scope and --connection must match [a-z][a-z0-9_-]*")
	}
	if *token == "" {
		host.Printf("Create an API token for your Atlassian account (or a personal access token on Data Center).\n")
		if *auth == "basic" {
			_ = host.OpenURL("https://id.atlassian.com/manage-profile/security/api-tokens")
		}
		value, err := host.Prompt("Jira token", extension.Prompt{Secret: true})
		if err != nil || strings.TrimSpace(value) == "" {
			return extension.SetupResult{}, tracker.SetupUsage("init jira needs --token REF (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND) or an interactive terminal")
		}
		if *token, err = host.StoreSecret(*connection+"-token", strings.TrimSpace(value)); err != nil {
			return extension.SetupResult{}, err
		}
	}
	if !tracker.RefPattern.MatchString(*token) {
		return extension.SetupResult{}, tracker.SetupUsage("--token takes a reference (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND), never a token")
	}
	dir := *checkout
	if dir != "" && !strings.HasPrefix(dir, "/") {
		dir = strings.TrimSuffix(host.Cwd(), "/") + "/" + dir
	}
	if *noWorkspace {
		dir = ""
	}
	entry := map[string]any{"projects": keys}
	if *jql != "" {
		entry["jql"] = *jql
	}
	if f := split(*fields); len(f) > 0 {
		entry["fields"] = f
	}
	host2 := strings.TrimPrefix(strings.TrimPrefix(conn.BaseURL, "https://"), "http://")
	spec := tracker.ScopeSpec{Type: Type, Scope: *scope, Name: "Jira " + strings.Join(keys, ","), Connection: *connection,
		ConnectionConfig: cfg, Secrets: map[string]string{SecretToken: *token}, Entry: entry,
		Namespace: "jira/" + host2 + "/" + strings.ToLower(strings.Join(keys, "+")), Checkout: dir,
		Summary: []string{"site       " + conn.BaseURL, "projects   " + strings.Join(keys, ", ")}}
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
		spec.CodeHost = tracker.BitbucketCodeHost(*bbRepo, *bbUser, *bbToken, *bbConn, spec.Namespace, keys, "{key}")
	}
	return spec.Result()
}

var bitbucketRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
