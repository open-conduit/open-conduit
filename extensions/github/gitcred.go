package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// OpGitCredential is git's credential for the Work's repository, minted
// for the execution that asks: the operation behind `os git-credential`,
// the git credential helper a container launch sets. It is not an agent's
// tool; agents run git and never call it themselves.
const OpGitCredential = "github.git.credential"

// gitReuse is how long before it expires a minted token stops being handed
// out again, as the connection's own installation tokens are.
const gitReuse = 5 * time.Minute

// gitGrant is one token minted for an execution.
type gitGrant struct {
	repo    string
	client  *Client
	token   string
	expires time.Time
}

// gitGrants are the tokens minted for each execution, kept until the host
// revokes them. They are in memory only: a daemon restart forgets them, and
// they expire within the hour.
type gitGrants struct {
	mu   sync.Mutex
	byID map[domain.ID][]gitGrant
}

type gitCredentialInput struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Path     string `json:"path,omitempty"`
}

type gitCredentialOutput struct {
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// gitCredential answers git's `get` for https on the code repository's web
// host (and, when git sends one, the repository's path) with a token
// limited to that repository and contents: write. Any other URL gets no
// credential, so git goes on to its next helper. The token is minted by
// the connection the calling binding acts as (its own `github` connection,
// else the entry's), the one RunEnvironment took the identity from. A
// token connection has nothing to mint: its token crosses into the
// container as it is.
func (t *Tools) gitCredential(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	// A call with a request ID would keep its result, the token, for
	// replay: `os git-credential` never sends one.
	if extension.CallRequestID(ctx) != "" {
		return nil, refuse("request_id_refused", "%s takes no request_id: its result is a credential and is never kept", OpGitCredential)
	}
	var in gitCredentialInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, err := t.pullSource(b, "")
	if err != nil {
		return nil, err
	}
	_, client, err := t.bindingClient(b, src)
	if err != nil {
		return nil, err
	}
	repo := src.Settings.Repo
	path := strings.TrimSuffix(strings.Trim(in.Path, "/"), ".git")
	if in.Protocol != "https" || !strings.EqualFold(in.Host, client.Host()) || path != "" && !strings.EqualFold(path, repo) {
		return gitCredentialOutput{}, nil
	}
	now := t.o.Now()
	exec := b.a.ExecutionID
	t.git.mu.Lock()
	for _, g := range t.git.byID[exec] {
		if g.repo == repo && g.client == client && g.expires.Sub(now) > gitReuse {
			t.git.mu.Unlock()
			return gitCredentialOutput{Username: "x-access-token", Password: g.token, ExpiresAt: g.expires.UTC().Format(time.RFC3339)}, nil
		}
	}
	t.git.mu.Unlock()
	tok, expires, err := client.GitToken(ctx, repo)
	if errors.Is(err, ErrStaticToken) {
		return nil, refuse("github_static_token", "this GitHub connection signs in with a token, not an App, so there is no token to mint for git")
	}
	if err != nil {
		return nil, err
	}
	t.git.mu.Lock()
	if t.git.byID == nil {
		t.git.byID = map[domain.ID][]gitGrant{}
	}
	t.git.byID[exec] = append(t.git.byID[exec], gitGrant{repo: repo, client: client, token: tok, expires: expires})
	t.git.mu.Unlock()
	return gitCredentialOutput{Username: "x-access-token", Password: tok, ExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
}

// RunEnvironment is the run environment layer (extension.RunEnvironment)
// for a Work in a scope with a repository this extension knows. The
// connection is the binding's own `github` connection (r.Connection) when
// it names one, else the entry's, so its git setting and its App decide.
//
// With git "bot": the toolbelt's `os git-credential` as git's only
// credential helper (the operator's keychain or gh helper, which git would
// otherwise ask first, cleared), signing off, and the App's bot user as
// author and committer.
//
// Otherwise: a credential helper for the GitHub web host that hands git a
// static token the run has in GH_TOKEN or GITHUB_TOKEN (a binding's
// personal access token), and says nothing when it has neither, so git goes
// on to its next helper. Git asks the operator's own helpers first, so on
// the host it is a fallback; in a container, which has none of them, it is
// how git reaches the repository with that token.
//
// Every other run gets nothing, and so does a Work whose scope has no
// single repository this extension knows.
func (t *Tools) RunEnvironment(ctx context.Context, r extension.RunEnvironmentRequest) (map[string]string, error) {
	b, err := t.bindWork(ctx, extension.Authority{ScopeID: r.Owner.ScopeID, WorkID: r.Owner.WorkID, ExecutionID: r.Owner.ExecutionID, Connection: r.Connection})
	if err != nil {
		return nil, nil
	}
	src, err := t.pullSource(b, "")
	if err != nil || src.Client == nil {
		return nil, nil
	}
	_, client, err := t.bindingClient(b, src)
	if err != nil {
		return nil, fmt.Errorf("github git: binding connection %s: %w", r.Connection, err)
	}
	if !client.GitBot() {
		return map[string]string{
			"GIT_CONFIG_COUNT": "1",
			"GIT_CONFIG_KEY_0": "credential.https://" + client.Host() + ".helper", "GIT_CONFIG_VALUE_0": TokenHelper,
		}, nil
	}
	id, err := client.GitIdentity(ctx, src.Settings.Repo)
	if err != nil {
		return nil, fmt.Errorf("github git bot identity: %w", err)
	}
	return map[string]string{
		"GIT_CONFIG_COUNT": "4",
		"GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "",
		"GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!os git-credential",
		"GIT_CONFIG_KEY_2": "commit.gpgsign", "GIT_CONFIG_VALUE_2": "false",
		"GIT_CONFIG_KEY_3": "tag.gpgsign", "GIT_CONFIG_VALUE_3": "false",
		"GIT_AUTHOR_NAME": id.Name, "GIT_AUTHOR_EMAIL": id.Email,
		"GIT_COMMITTER_NAME": id.Name, "GIT_COMMITTER_EMAIL": id.Email,
	}, nil
}

// TokenHelper is the git credential helper RunEnvironment sets for the
// GitHub web host on a run without git "bot": it answers `get` with the
// run's GH_TOKEN (else GITHUB_TOKEN), and with nothing when it has neither.
const TokenHelper = `!f() { test "$1" = get || exit 0; t="${GH_TOKEN:-$GITHUB_TOKEN}"; test -n "$t" || exit 0; echo username=x-access-token; echo "password=$t"; }; f`

// RevokeGitTokens revokes every git token minted for the execution, once
// its segment has ended. One that GitHub couldn't be told about is kept,
// for the next call (or SweepGitTokens) to try again.
func (t *Tools) RevokeGitTokens(ctx context.Context, execution domain.ID) error {
	t.git.mu.Lock()
	grants := t.git.byID[execution]
	delete(t.git.byID, execution)
	t.git.mu.Unlock()
	var kept []gitGrant
	var errs []error
	for _, g := range grants {
		if err := g.client.RevokeToken(ctx, g.token); err != nil {
			if !g.expires.After(t.o.Now()) {
				continue
			}
			kept, errs = append(kept, g), append(errs, err)
		}
	}
	if len(kept) > 0 {
		t.git.mu.Lock()
		if t.git.byID == nil {
			t.git.byID = map[domain.ID][]gitGrant{}
		}
		t.git.byID[execution] = append(t.git.byID[execution], kept...)
		t.git.mu.Unlock()
	}
	return errors.Join(errs...)
}

// SweepGitTokens revokes the git tokens of every execution live doesn't
// report running: those whose end RevokeGitTokens missed.
func (t *Tools) SweepGitTokens(ctx context.Context, live func([]domain.ID) (map[domain.ID]bool, error)) error {
	t.git.mu.Lock()
	ids := make([]domain.ID, 0, len(t.git.byID))
	for id := range t.git.byID {
		ids = append(ids, id)
	}
	t.git.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	running, err := live(ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if !running[id] {
			errs = append(errs, t.RevokeGitTokens(ctx, id))
		}
	}
	return errors.Join(errs...)
}
