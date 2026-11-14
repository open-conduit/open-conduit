package github

import (
	"context"
	"fmt"
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
	// Known is true when Pull, Push and Admin are the token's user's role
	// on the repository. It is false for a GitHub App sign-in: GitHub
	// answers an installation token with a permissions object too, all
	// false, since it describes a user's role, not the installation's.
	Known             bool
	Pull, Push, Admin bool
	// Writer is true when a separate bot identity performs writes and it
	// can read the repository too.
	Writer bool
	// App is true when the connection signs in as a GitHub App, whose
	// installation's permissions ReaderPermissions reads.
	App bool
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
	who := c.who.Load()
	a := Access{DefaultBranch: out.DefaultBranch, Public: out.Private != nil && !*out.Private}
	if _, ok := who.read.(*appToken); ok {
		a.App = true
	} else if out.Permissions != nil {
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
// hook administration, so it uses the reader identity.
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

// Delivery is what GitHub recorded for a hook's last delivery (the hook's
// last_response).
type Delivery struct {
	// Code is the HTTP status the hook's URL answered (0 when there was
	// no answer, or no delivery yet).
	Code int
	// Status is GitHub's word for it ("unused" before the first delivery).
	Status string
	// Message is GitHub's description ("failed to connect to host").
	Message string
}

// Failing says whether the last delivery did not get through.
func (d Delivery) Failing() bool {
	if d.Code != 0 {
		return d.Code < 200 || d.Code >= 300
	}
	return d.Status != "" && d.Status != "unused" && d.Status != "active"
}

// Describe is the delivery for doctor.
func (d Delivery) Describe() string {
	switch {
	case d.Code == 0 && (d.Status == "unused" || d.Status == ""):
		return "no delivery yet"
	case d.Code == 0:
		return "last delivery " + d.Status + describeMessage(d.Message)
	}
	return fmt.Sprintf("last delivery %d%s", d.Code, describeMessage(d.Message))
}

func describeMessage(m string) string {
	m = strings.TrimSpace(m)
	if m == "" || strings.EqualFold(m, "OK") {
		return ""
	}
	if len(m) > 120 {
		m = m[:120] + "…"
	}
	return " " + m
}

// HookDelivery reads how the hook's last delivery went. It needs hook
// administration, so it uses the reader identity, as EnsureWebhook does.
func (c *Client) HookDelivery(ctx context.Context, repo, id string) (Delivery, error) {
	var out struct {
		LastResponse struct {
			Code    *int    `json:"code"`
			Status  string  `json:"status"`
			Message *string `json:"message"`
		} `json:"last_response"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/hooks/%s", url.PathEscape(id)), nil, &out); err != nil {
		return Delivery{}, err
	}
	d := Delivery{Status: out.LastResponse.Status}
	if out.LastResponse.Code != nil {
		d.Code = *out.LastResponse.Code
	}
	if out.LastResponse.Message != nil {
		d.Message = *out.LastResponse.Message
	}
	return d, nil
}

// PingWebhook has GitHub send the hook a ping, so its last delivery is one
// to the URL it points at now.
func (c *Client) PingWebhook(ctx context.Context, repo, id string) error {
	_, err := c.call(ctx, false, repo, http.MethodPost, repoPath(repo, "/hooks/%s/pings", url.PathEscape(id)), nil, nil)
	return err
}
