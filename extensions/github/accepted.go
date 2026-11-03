package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// What GitHub permissions a write needs, how GitHub says so when it refuses
// one, and how a refusal is reported: GitHub's own account of what is
// missing, never a guess. Doctor uses the same table to flag a binding
// granted an operation its connection can't carry out.

// acceptedHeader is the header GitHub answers a refused REST call (and
// sometimes GraphQL) with: the permissions the call would have needed.
const acceptedHeader = "X-Accepted-GitHub-Permissions"

// Permission is one GitHub App or fine-grained token permission at a level:
// pull_requests at write.
type Permission struct {
	Name, Level string
}

func (p Permission) String() string { return permissionLabel(p.Name) + " " + p.Level }

// permissionLabel is a permission's name as GitHub's settings show it.
func permissionLabel(name string) string {
	if label := permissionLabels[name]; label != "" {
		return label
	}
	label := strings.ReplaceAll(name, "_", " ")
	if label != "" {
		label = strings.ToUpper(label[:1]) + label[1:]
	}
	return label
}

// permissionLabels are GitHub's names for the permissions as its settings
// pages show them.
var permissionLabels = map[string]string{
	"actions":        "Actions",
	"administration": "Administration",
	"checks":         "Checks",
	"contents":       "Contents",
	"issues":         "Issues",
	"metadata":       "Metadata",
	"pull_requests":  "Pull requests",
	"statuses":       "Commit statuses",
	"workflows":      "Workflows",
}

// levels orders permission levels: write includes read, admin both.
var levels = map[string]int{"read": 1, "write": 2, "admin": 3}

// covers reports whether a level held meets the level needed.
func covers(have, need string) bool {
	return levels[have] >= levels[need] && levels[need] > 0
}

// ParseAccepted reads an X-Accepted-GitHub-Permissions value: sets of
// permissions separated by semicolons, any one of which is enough, each
// a comma-separated list that is needed together
// ("pull_requests=write,contents=read; issues=write").
func ParseAccepted(v string) [][]Permission {
	var out [][]Permission
	for _, set := range strings.Split(v, ";") {
		var perms []Permission
		for _, item := range strings.Split(set, ",") {
			name, level, ok := strings.Cut(strings.TrimSpace(item), "=")
			name, level = strings.ToLower(strings.TrimSpace(name)), strings.ToLower(strings.TrimSpace(level))
			if ok && name != "" && level != "" {
				perms = append(perms, Permission{name, level})
			}
		}
		if len(perms) > 0 {
			out = append(out, perms)
		}
	}
	return out
}

// opPermissions are the permissions each write operation needs of the
// connection that carries it out, on the code repository. Resolving a
// review thread needs Contents write as well as Pull requests write: GitHub
// refuses resolveReviewThread with Contents read
// (github.com/orgs/community/discussions/204269).
var opPermissions = map[string][]Permission{
	OpPRCreate:  {{"pull_requests", "write"}},
	OpPREdit:    {{"pull_requests", "write"}},
	OpPRReply:   {{"pull_requests", "write"}},
	OpPRReview:  {{"pull_requests", "write"}},
	OpPRResolve: {{"pull_requests", "write"}, {"contents", "write"}},
}

// bindingWriterOps are the operations opPermissions lists that go out as
// the binding's own GitHub connection when it names one (bindingClient):
// a reply, a review, a resolve. The rest (opening and editing a PR) always
// go out as the entry's connection.
var bindingWriterOps = map[string]bool{OpPRReply: true, OpPRReview: true, OpPRResolve: true}

// lacking is what of need a connection holding have doesn't.
func lacking(have map[string]string, need []Permission) []Permission {
	var out []Permission
	for _, p := range need {
		if !covers(have[p.Name], p.Level) {
			out = append(out, p)
		}
	}
	return out
}

// acceptedNeeds puts an X-Accepted-GitHub-Permissions value in words
// ("Pull requests write and Contents write, or Issues write"), "" when
// there is none.
func acceptedNeeds(v string) string {
	sets := ParseAccepted(v)
	alts := make([]string, len(sets))
	for i, set := range sets {
		alts[i] = joinPermissions(set)
	}
	return strings.Join(alts, ", or ")
}

func joinPermissions(ps []Permission) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, " and ")
}

// WriterPermissions are the permissions the client's writes go out with on
// repo, read afresh, when its writer is a GitHub App (the bot App, or an App
// connection's own installation). known is false for a token, whose
// permissions GitHub doesn't report.
func (c *Client) WriterPermissions(ctx context.Context, repo string) (perms map[string]string, known bool, err error) {
	app, ok := c.who.Load().write.(*appToken)
	if !ok {
		return nil, false, nil
	}
	perms, err = app.permissions(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	return perms, true, nil
}

// forbidden reports a 403 from GitHub on op as the refusal the agent sees:
// what GitHub says is missing (its X-Accepted-GitHub-Permissions header),
// else what op needs that the connection's App installation lacks, else
// what GitHub said, with what op needs. Any other error passes through.
func forbidden(ctx context.Context, c *Client, repo, op, doing string, err error) error {
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden {
		return err
	}
	said := se.Message
	if se.Type != "" && said != "" {
		said = se.Type + ": " + said
	}
	if said != "" {
		said = fmt.Sprintf(" (GitHub said %q)", said)
	}
	if needs := acceptedNeeds(se.Accepted); needs != "" {
		return refuse("github_forbidden", "this connection can't %s on %s: GitHub says it needs %s%s; ask the owner to grant it to the connection", doing, repo, needs, said)
	}
	need := opPermissions[op]
	if c != nil && len(need) > 0 {
		if have, known, perr := c.WriterPermissions(ctx, repo); perr == nil && known {
			if miss := lacking(have, need); len(miss) > 0 {
				return refuse("github_forbidden", "this connection can't %s on %s: its GitHub App lacks %s%s; ask the owner to grant it to the App", doing, repo, joinPermissions(miss), said)
			}
		}
	}
	if len(need) > 0 {
		return refuse("github_forbidden", "this connection can't %s on %s%s; it needs %s, or GitHub refused for another reason", doing, repo, said, joinPermissions(need))
	}
	return refuse("github_forbidden", "this connection can't %s on %s%s", doing, repo, said)
}

// grantedNeeds are the permissions the operations granted need, by the
// connection each operation writes as (own, the binding's own connection,
// for bindingWriterOps when own isn't ""; entry otherwise), with the
// operations that need each, sorted.
func grantedNeeds(ops []string, own, entry string) map[string]map[Permission][]string {
	out := map[string]map[Permission][]string{}
	for _, op := range ops {
		conn := entry
		if own != "" && bindingWriterOps[op] {
			conn = own
		}
		for _, p := range opPermissions[op] {
			if out[conn] == nil {
				out[conn] = map[Permission][]string{}
			}
			out[conn][p] = append(out[conn][p], op)
		}
	}
	for _, needs := range out {
		for p := range needs {
			sort.Strings(needs[p])
			needs[p] = slices.Compact(needs[p])
		}
	}
	return out
}
