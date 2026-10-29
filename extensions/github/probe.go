package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"

	"github.com/open-conduit/open-conduit/extension"
)

// Probes are the extension's probes for setup sessions: `github.accounts`
// (the accounts gh is signed in to) and `github.repos` (an owner's
// repositories). Gh runs gh with args and returns its standard output
// (exec, unless set in tests).
type Probes struct {
	Gh func(ctx context.Context, args ...string) ([]byte, error)
}

var _ extension.SetupProbes = Probes{}

var ownerName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// Account is one gh login, with no token.
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
		return nil, fmt.Errorf("gh %s failed: install gh and sign in with gh auth login", args[0])
	}
	return out.Bytes(), nil
}

func (p Probes) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{
		{Name: "accounts", Summary: "the GitHub accounts gh is signed in to: host, login, whether active, the token's source and state (never the token)",
			Run: func(ctx context.Context, _ map[string]string) (extension.ProbeResult, error) {
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
			}},
		{Name: "repos", Summary: "an owner's repositories, by name (up to 200)", Args: []extension.ProbeArg{{Name: "owner", Summary: "a user or organisation login", Required: true}},
			Run: func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
				owner := args["owner"]
				if !ownerName.MatchString(owner) {
					return extension.ProbeResult{}, fmt.Errorf("%w: owner is a GitHub login", extension.ErrProbeInput)
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
			}},
	}
}
