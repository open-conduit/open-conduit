package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"

	"github.com/open-conduit/open-conduit/extension"
)

// maxProbeRepos is how many repositories the repos probe returns.
const maxProbeRepos = 200

// Probes are the extension's probes for setup sessions: `github.accounts`
// (who gh, or a connection, is signed in as) and `github.repos` (an owner's
// repositories). Without a `connection` argument they run gh on the daemon's
// machine. With one they read through that applied connection, which the
// host grants only to a scope authorized for it, so a daemon with no gh can
// answer. Gh runs gh with args and returns its standard output (exec, unless
// set in tests); HTTP is the connection client's transport (tests).
type Probes struct {
	Gh   func(ctx context.Context, args ...string) ([]byte, error)
	HTTP *http.Client
}

var _ extension.SetupProbes = Probes{}

var ownerName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// Account is one GitHub sign-in, with no token. TokenSource is
// "connection" for one read through a connection.
type Account struct {
	Host        string `json:"host"`
	Login       string `json:"login"`
	Active      bool   `json:"active"`
	TokenSource string `json:"token_source"`
	State       string `json:"state"`
}

func (p Probes) gh(ctx context.Context, args ...string) ([]byte, error) {
	if p.Gh != nil {
		return p.Gh(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %s failed: install gh and sign in with gh auth login, or name a connection this scope is authorized for", args[0])
	}
	return out.Bytes(), nil
}

// connectionArg is the argument that reads through a connection.
var connectionArg = extension.ProbeArg{Name: "connection", Summary: "an applied github connection this session's scope is authorized for, to read through it instead of gh", Connection: true}

// client is the GitHub client of the connection the host granted this run.
// A run that names a connection the host didn't grant is refused rather
// than falling back to gh, which would answer as a different account.
func (p Probes) client(ctx context.Context, args map[string]string) (*Client, string, bool, error) {
	if args[connectionArg.Name] == "" {
		return nil, "", false, nil
	}
	c, ok := extension.ProbeConnectionFrom(ctx)
	if !ok || c.ID != args[connectionArg.Name] {
		return nil, "", false, errors.New("this probe can't read through that connection here")
	}
	cfg, err := ParseConnection(c.Config)
	if err != nil {
		return nil, "", false, errors.New("the connection's config can't be read")
	}
	client, err := NewClient(cfg, SecretFunc(c.Secret), ClientOptions{HTTP: p.HTTP})
	if err != nil {
		return nil, "", false, errors.New("the connection's config can't be read")
	}
	return client, c.ID, true, nil
}

// probeError is a fixed reason for a failed read through a connection: a
// probe's error reaches the agent, so it carries no text from GitHub.
func probeError(err error) error {
	switch {
	case errors.Is(err, ErrBrokeredRepos):
		return errors.New("an App behind a broker can't list repositories: ask which ones by owner/name")
	case errors.Is(err, ErrCredential):
		return errors.New("the connection's credential isn't available: store it, or check its reference")
	case IsStatus(err, http.StatusUnauthorized):
		return errors.New("GitHub refused the connection's credential")
	case IsStatus(err, http.StatusForbidden):
		return errors.New("the connection's credential may not read this")
	case IsStatus(err, http.StatusNotFound):
		return errors.New("GitHub found nothing for this owner through the connection: an App may not be installed there")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	}
	return errors.New("GitHub couldn't be reached through the connection")
}

func (p Probes) accounts(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
	client, id, ok, err := p.client(ctx, args)
	if err != nil {
		return extension.ProbeResult{}, err
	}
	if ok {
		login, err := client.Identity(ctx)
		if err != nil {
			return extension.ProbeResult{}, probeError(err)
		}
		accounts := []Account{{Host: client.Host(), Login: login, Active: true, TokenSource: "connection", State: "success"}}
		return extension.ProbeResult{Command: "GitHub API through connection " + id, Output: map[string]any{"accounts": accounts}}, nil
	}
	out, err := p.gh(ctx, "auth", "status", "--json", "hosts")
	if err != nil {
		return extension.ProbeResult{}, err
	}
	var status struct {
		Hosts map[string][]struct {
			Host        string `json:"host"`
			Login       string `json:"login"`
			Active      bool   `json:"active"`
			TokenSource string `json:"tokenSource"`
			State       string `json:"state"`
		} `json:"hosts"`
	}
	if err = json.Unmarshal(out, &status); err != nil {
		return extension.ProbeResult{}, fmt.Errorf("gh auth status gave output this probe can't read: upgrade gh")
	}
	accounts := []Account{}
	for _, list := range status.Hosts {
		for _, a := range list {
			accounts = append(accounts, Account{Host: a.Host, Login: a.Login, Active: a.Active, TokenSource: a.TokenSource, State: a.State})
		}
	}
	return extension.ProbeResult{Command: "gh auth status", Output: map[string]any{"accounts": accounts}}, nil
}

func (p Probes) repos(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
	owner := args["owner"]
	if !ownerName.MatchString(owner) {
		return extension.ProbeResult{}, fmt.Errorf("%w: owner is a GitHub login", extension.ErrProbeInput)
	}
	client, id, ok, err := p.client(ctx, args)
	if err != nil {
		return extension.ProbeResult{}, err
	}
	if ok {
		names, more, err := client.OwnerRepos(ctx, owner, maxProbeRepos)
		if err != nil {
			return extension.ProbeResult{}, probeError(err)
		}
		output := map[string]any{"repos": names}
		if more {
			output["truncated"] = true
		}
		return extension.ProbeResult{Command: "GitHub API through connection " + id + ": repositories of " + owner, Output: output}, nil
	}
	out, err := p.gh(ctx, "repo", "list", owner, "--limit", "200", "--json", "nameWithOwner")
	if err != nil {
		return extension.ProbeResult{}, err
	}
	var repos []struct {
		Name string `json:"nameWithOwner"`
	}
	if err = json.Unmarshal(out, &repos); err != nil {
		return extension.ProbeResult{}, fmt.Errorf("gh repo list gave output this probe can't read: upgrade gh")
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return extension.ProbeResult{Command: "gh repo list " + owner, Output: map[string]any{"repos": names}}, nil
}

func (p Probes) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{
		{Name: "accounts", Summary: "who GitHub knows this daemon as, through gh or a named connection: host, login, whether active, the token's source and state (never the token)",
			Args: []extension.ProbeArg{connectionArg}, Run: p.accounts},
		{Name: "repos", Summary: "an owner's repositories, by name (up to 200), through gh or a named connection",
			Args: []extension.ProbeArg{{Name: "owner", Summary: "a user or organisation login", Required: true}, connectionArg}, Run: p.repos},
	}
}
