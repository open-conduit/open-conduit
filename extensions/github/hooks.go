package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Operator support (03-05): access verification for doctor and webhook
// management for the host's listener. Both use the connection's own
// identities and never return credentials or reference text.

// HookEvents are the deliveries the normalizer understands, so the events a
// managed webhook subscribes to.
var HookEvents = []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "check_suite", "check_run", "status"}

// Access is what the connection's identities may do on a repository.
type Access struct {
	// Known is false when GitHub reported no permissions (App installation
	// tokens carry them on the installation instead).
	Known             bool
	Pull, Push, Admin bool
	// Writer is true when a separate bot identity performs writes and it
	// can read the repository too.
	Writer bool
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	// Public is true when the repository is public: anyone can open an
	// issue or a PR on it.
	Public bool
	// Checks is how CI results can be read: "check_runs", "workflow_runs"
	// (check runs refused, as for a fine-grained token, which cannot hold the
	// Checks permission; Actions workflow runs stand in) or "unreadable"
	// (only commit statuses). Empty when the probe could not tell.
	Checks string
}

// Verify authenticates both identities and reads the repository: the doctor
// check that credentials resolve, are accepted and reach repo.
func (c *Client) Verify(ctx context.Context, repo string) (Access, error) {
	var out struct {
		DefaultBranch string           `json:"default_branch"`
		Private       *bool            `json:"private"`
		Permissions   *map[string]bool `json:"permissions"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodGet, "/repos/"+repo, nil, &out); err != nil {
		return Access{}, err
	}
	a := Access{DefaultBranch: out.DefaultBranch, Public: out.Private != nil && !*out.Private}
	if out.Permissions != nil {
		p := *out.Permissions
		a.Known, a.Pull, a.Push, a.Admin = true, p["pull"], p["push"] || p["maintain"] || p["admin"], p["admin"]
	}
	if out.DefaultBranch != "" {
		_, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/commits/%s/check-runs?per_page=1", url.PathEscape(out.DefaultBranch)), nil, nil)
		switch {
		case err == nil:
			a.Checks = "check_runs"
		case IsStatus(err, http.StatusForbidden):
			_, err = c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/actions/runs?per_page=1"), nil, nil)
			switch {
			case err == nil:
				a.Checks = "workflow_runs"
			case IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusNotFound):
				a.Checks = "unreadable"
			}
		}
	}
	if c.hasBot() {
		if _, err := c.call(ctx, true, repo, http.MethodGet, "/repos/"+repo, nil, nil); err != nil {
			return a, err
		}
		a.Writer = true
	}
	return a, nil
}

type rawHook struct {
	ID     int64 `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// EnsureWebhook points the repository webhook whose URL ends in suffix (or
// the hook with existing ID) at url, creating one when none exists. It needs
// hook administration, so it uses the reader identity (v1 EnsureWebhook).
// dryRun reports what it would do and writes nothing. It returns the hook ID
// and whether a write was made.
func (c *Client) EnsureWebhook(ctx context.Context, repo, url, suffix, secret, existing string, dryRun bool) (string, bool, error) {
	hooks, err := all[rawHook](ctx, c, repo, repoPath(repo, "/hooks"))
	if err != nil {
		return "", false, err
	}
	body := map[string]any{
		"active": true,
		"events": HookEvents,
		"config": map[string]string{"url": url, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
	}
	for _, h := range hooks {
		id := strconv.FormatInt(h.ID, 10)
		if id != existing && (suffix == "" || !strings.HasSuffix(h.Config.URL, suffix)) {
			continue
		}
		if h.Config.URL == url || dryRun {
			return id, false, nil
		}
		if _, err := c.call(ctx, false, repo, http.MethodPatch, repoPath(repo, "/hooks/%s", id), body, nil); err != nil {
			return "", false, err
		}
		return id, true, nil
	}
	if dryRun {
		return "", false, nil
	}
	body["name"] = "web"
	var created struct {
		ID int64 `json:"id"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodPost, repoPath(repo, "/hooks"), body, &created); err != nil {
		return "", false, err
	}
	return strconv.FormatInt(created.ID, 10), true, nil
}
