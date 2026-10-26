package github

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Required checks (08-01, v1 c37edf1). GitHub already names the checks a PR
// needs before it can merge, in the base branch's protection and in the
// rulesets that apply to it, so no setting repeats them. A head missing one
// is held (observed as pending) until no_checks_minutes, then observed as
// github.checks.missing so the agent hears which never reported rather than
// a pass.

// Sources of required checks.
const (
	RequiredFromProtection = "branch_protection"
	RequiredFromRulesets   = "rulesets"
)

// How a commit's runs were read (checkState). Required checks name check
// runs and statuses, so they are only verified when check runs are readable:
// Actions workflow runs carry workflow names, not job names, and would make
// every required job look missing.
const (
	RunsFromCheckRuns = "check_runs"
	RunsFromWorkflows = "workflow_runs"
	RunsUnreadable    = "unreadable"
)

// RequiredChecks are the checks a branch requires and where they were read.
type RequiredChecks struct {
	// Names are the required check names (contexts), in source order and
	// deduplicated.
	Names []string `json:"names"`
	// From are the sources that answered.
	From []string `json:"from"`
	// Refused are the sources GitHub would not let this connection read
	// (403 or 404), with no names from them: a fine-grained token without
	// the permission, or rulesets on a plan without them.
	Refused []string `json:"refused,omitempty"`
}

// RequiredChecks reads the checks a PR into branch needs from the branch's
// protection and the rulesets that apply to it. Both reads are conditional
// GETs under the client's rate-limit pause, so an unchanged answer costs a
// 304. A refused source degrades to Refused, never to an error that would
// fail a scan; rate limits and other failures are errors, so no absence of a
// requirement is inferred from them.
func (c *Client) RequiredChecks(ctx context.Context, repo, branch string) (RequiredChecks, error) {
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	name := strings.Join(segs, "/")
	type check struct {
		Context string `json:"context"`
	}
	var out RequiredChecks
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out.Names = append(out.Names, name)
		}
	}

	// The branch read (Contents/Metadata: read) carries the protection's
	// required status checks without the Administration permission that
	// the protection endpoint itself needs. 404 is a branch that does not
	// exist (yet) or one this identity cannot see.
	var b struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []check  `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	_, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/branches/%s", name), nil, &b)
	switch {
	case err == nil:
		out.From = append(out.From, RequiredFromProtection)
		for _, x := range b.Protection.RequiredStatusChecks.Contexts {
			add(x)
		}
		for _, x := range b.Protection.RequiredStatusChecks.Checks {
			add(x.Context)
		}
	case refusedRead(err):
		out.Refused = append(out.Refused, RequiredFromProtection)
	default:
		return RequiredChecks{}, err
	}

	// Rulesets are not on every plan: a private repository on GitHub Free
	// answers 403.
	rules, err := all[struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []check `json:"required_status_checks"`
		} `json:"parameters"`
	}](ctx, c, repo, repoPath(repo, "/rules/branches/%s", name))
	switch {
	case err == nil:
		out.From = append(out.From, RequiredFromRulesets)
		for _, r := range rules {
			if r.Type == "required_status_checks" {
				for _, x := range r.Parameters.RequiredStatusChecks {
					add(x.Context)
				}
			}
		}
	case refusedRead(err):
		out.Refused = append(out.Refused, RequiredFromRulesets)
	default:
		return RequiredChecks{}, err
	}
	return out, nil
}

func refusedRead(err error) bool {
	return IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusNotFound)
}

// requiredChecks reads (once per normalizer) the checks base requires.
func (n *normalizer) requiredChecks(ctx context.Context, base string) (RequiredChecks, error) {
	if base == "" {
		return RequiredChecks{}, nil
	}
	n.m.mu.Lock()
	r, ok := n.m.required[base]
	n.m.mu.Unlock()
	if ok {
		return r, nil
	}
	r, err := n.c.RequiredChecks(ctx, n.s.Repo, base)
	if err != nil {
		return RequiredChecks{}, err
	}
	n.m.mu.Lock()
	n.m.required[base] = r
	n.m.mu.Unlock()
	return r, nil
}

// missingChecks are the required checks that have not reported on the head
// at all (v1 core.Checks.Missing), in required order.
func missingChecks(runs []CheckRun, required []string) []string {
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.Name] = true
	}
	var out []string
	for _, name := range required {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// RequiredNote explains a degraded required-checks read for the github.checks
// tool and doctor, or is empty when every source was read with check runs.
func RequiredNote(r RequiredChecks, runsFrom string) string {
	var notes []string
	if len(r.Refused) > 0 {
		notes = append(notes, "required checks were read from "+orNone(r.From)+" only: GitHub refused "+strings.Join(r.Refused, " and ")+" (a fine-grained token without Administration: read, or rulesets the plan lacks)")
	}
	if runsFrom != "" && runsFrom != RunsFromCheckRuns && len(r.Names) > 0 {
		notes = append(notes, "check runs are not readable with this connection, so required checks cannot be verified and a missing one is not reported")
	}
	return strings.Join(notes, "; ")
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "no source"
	}
	return strings.Join(s, " and ")
}
