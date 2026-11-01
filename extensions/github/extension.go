package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// Extension is the GitHub extension on the in-process contract: its config
// types and its doctor checks. The host still registers each entry's
// ingress and projector; it hands the extension a read-only view of those
// registrations (View), and the extension resolves the credentials it
// verifies through the host's scoped resolver (inproc.Host.Secrets), for
// the entry's connection and for each `github` connection a binding in the
// scope names.
type Extension struct {
	view ViewFunc

	mu      sync.Mutex
	host    inproc.Host
	started bool
	entries []protocol.ConfigEntry
	clients *Writers
}

// ViewFunc reports the host's GitHub registrations for the doctor checks.
type ViewFunc func(context.Context) View

// View is the host's GitHub registrations: the registered entries, and the
// failure code of each entry that could not be registered, by namespace.
type View struct {
	Entries []ViewEntry
	Failed  map[string]string
}

// ViewEntry is one registered entry as the doctor reports it.
type ViewEntry struct {
	// Name is the entry's ingress registration name.
	Name       string
	Scope      domain.ID
	Connection string
	Settings   Settings
	// Client is the client the entry reads and writes with, built when it
	// was registered. Doctor verifies through it, so it reports what the
	// daemon does rather than what the config says; nil verifies a client
	// built from the current config.
	Client *Client
	// PausedUntil is set while the entry's client is rate limited.
	PausedUntil time.Time
	// Scan is the entry's last reconcile, when the ingress reports one.
	Scan *Scan
	// Projection is the code of the entry's last failing projection pass.
	Projection string
}

// Scan is an entry's reconcile state.
type Scan struct {
	LastError    string
	Failures     uint32
	LastComplete time.Time
}

// NewExtension returns the extension, unstarted. A nil view reports no
// entries (offline validation needs only the types).
func NewExtension(view ViewFunc) *Extension {
	x := &Extension{view: view}
	x.clients = NewWriters(x.connection, x.now)
	return x
}

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

// Types are the `github` connection and ingress entry types.
func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateConnection(raw) }},
		{Kind: protocol.ValidateExtension, Type: Type, Ingress: true, Validate: ValidateExtension, Settings: ExtensionSettings},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateRoute(raw) }},
	}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.mu.Lock()
	x.host, x.started = h, true
	x.mu.Unlock()
	return nil
}

// Configure keeps the generation's entries, with the connection each names
// and those its scope's bindings name.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	x.mu.Lock()
	x.entries = p.Entries
	x.mu.Unlock()
}

func (x *Extension) now() time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.host.Now != nil {
		return x.host.Now()
	}
	return time.Now()
}

// connection resolves a connection the extension may act as in scope (an
// entry's own, or one a binding there names) from the configured entries,
// with its secrets through the host's scoped resolver.
func (x *Extension) connection(scope domain.ID, id string) (json.RawMessage, SecretFunc, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.started || x.host.Secrets == nil {
		return nil, nil, false
	}
	resolve := x.host.Secrets
	for _, e := range x.entries {
		if e.ScopeID != string(scope) {
			continue
		}
		conns := append([]protocol.ConnectionEntry(nil), e.BindingConnections...)
		if e.Connection != nil {
			conns = append(conns, *e.Connection)
		}
		for _, c := range conns {
			if c.ID == id && c.Type == Type {
				return c.Config, func(ctx context.Context, name string) (string, error) {
					v, err := resolve(ctx, scope, id, []string{name})
					if err != nil {
						return "", ErrCredential
					}
					return v[name], nil
				}, true
			}
		}
	}
	return nil, nil, false
}

// bindingConnections are the `github` connections the bindings in an
// entry's scope name.
func (x *Extension) bindingConnections(scope domain.ID) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range x.entries {
		if e.ScopeID != string(scope) {
			continue
		}
		for _, c := range e.BindingConnections {
			if c.Type == Type && !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (x *Extension) dryRun() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.host.DryRun
}

func (x *Extension) registrations(ctx context.Context) View {
	if x.view == nil {
		return View{}
	}
	return x.view(ctx)
}

// Checks are `github` (registration, rate-limit, reconcile and projection
// state per entry, with no service calls), the configuration checks
// `github_capability` and `git_bot` (doctor.go, offline too) and, under
// check_secrets, `github_auth` (each entry's credentials and those of the
// connections its scope's bindings name, by reading its repository).
func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Offline: true, Run: capabilityCheck}, {Offline: true, Run: gitBotCheck}, {Deep: true, Run: x.authCheck}}
}

func rank(status string) int {
	switch status {
	case inproc.CheckFail:
		return 3
	case inproc.CheckWarn:
		return 2
	case inproc.CheckInfo:
		return 1
	}
	return 0
}

func (x *Extension) check(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "github", Status: inproc.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	v := x.registrations(ctx)
	if len(v.Entries) == 0 && len(v.Failed) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no GitHub entries are configured"
		return check
	}
	for ns, code := range v.Failed {
		raise(inproc.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	now, dry := x.now(), x.dryRun()
	for _, e := range v.Entries {
		var notes []string
		if !e.PausedUntil.IsZero() {
			raise(inproc.CheckWarn, "rate_limited")
			notes = append(notes, "rate limited until "+e.PausedUntil.UTC().Format(time.RFC3339))
		}
		if s := e.Scan; s != nil {
			switch {
			case s.LastError != "":
				raise(inproc.CheckWarn, "reconcile_failing")
				notes = append(notes, fmt.Sprintf("reconcile failing (%s, %d failures)", s.LastError, s.Failures))
			case s.LastComplete.IsZero():
				notes = append(notes, "first reconcile pending")
			default:
				notes = append(notes, "reconciled "+now.Sub(s.LastComplete).Truncate(time.Second).String()+" ago")
			}
		}
		if e.Projection != "" {
			raise(inproc.CheckWarn, "projection_failing")
			notes = append(notes, "projection "+e.Projection)
		}
		if dry || e.Settings.Projection.DryRun {
			notes = append(notes, "dry run (no GitHub writes)")
		}
		check.Items = append(check.Items, e.Settings.Namespace+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// CallCode is a safe diagnostic code for a failed GitHub call.
func CallCode(err error) string {
	var limit *RateLimitError
	switch {
	case errors.Is(err, ErrCredential):
		return "credential_unavailable"
	case errors.As(err, &limit):
		return "rate_limited"
	case IsStatus(err, http.StatusNotFound), IsStatus(err, http.StatusForbidden):
		return "hook_permission_denied"
	case IsStatus(err, http.StatusUnauthorized):
		return "credential_rejected"
	}
	return "hook_update_failed"
}

// verifyCode is CallCode as the connection test reports it.
func verifyCode(err error) string {
	switch code := CallCode(err); code {
	case "hook_permission_denied":
		return "repo_unreachable"
	case "hook_update_failed":
		return "verify_failed"
	default:
		return code
	}
}

// authCheck resolves each entry's credentials privately and reads its
// repository: the connection test. It reports codes and permissions, never
// a reference or a token.
func (x *Extension) authCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "github_auth", Status: inproc.CheckOK, Code: "verified"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	warn := func(code string) {
		if check.Status == inproc.CheckOK {
			check.Status, check.Code = inproc.CheckWarn, code
		}
	}
	v := x.registrations(ctx)
	if len(v.Entries) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
		return check
	}
	dry := x.dryRun()
	for _, e := range v.Entries {
		s := e.Settings
		item := s.Namespace + ": "
		configured, cerr := x.clients.Client(e.Scope, e.Connection, nil)
		client := e.Client
		if client == nil {
			if cerr != nil {
				raise(inproc.CheckFail, "credential_unavailable")
				check.Items = append(check.Items, item+"credential_unavailable")
				continue
			}
			client = configured
		}
		if cerr == nil {
			if drift := driftNotes(client, configured); len(drift) > 0 {
				raise(inproc.CheckWarn, "restart_required")
				check.Items = append(check.Items, item+"restart_required: "+strings.Join(drift, "; ")+"; restart the daemon to apply the config")
			}
		}
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		a, err := client.Verify(cx, s.TrackerRepo)
		cancel()
		if err != nil {
			code := verifyCode(err)
			raise(inproc.CheckFail, code)
			check.Items = append(check.Items, item+code)
			continue
		}
		var notes []string
		if a.Known {
			notes = append(notes, fmt.Sprintf("pull=%t push=%t admin=%t", a.Pull, a.Push, a.Admin))
			if !a.Push && !a.Writer && !dry && !s.Projection.DryRun {
				warn("read_only")
				notes = append(notes, "labels, comments and PRs need write access")
			}
			if s.ManageWebhook && !a.Admin {
				warn("hook_admin_missing")
				notes = append(notes, "manage_webhook needs admin (hooks) permission")
			}
		} else {
			notes = append(notes, "authenticated (installation permissions are not reported)")
		}
		if a.Writer {
			notes = append(notes, "bot writer authenticated")
		}
		switch a.Checks {
		case "workflow_runs":
			notes = append(notes, "check runs refused (a fine-grained token cannot hold Checks): CI is read from Actions workflow runs and commit statuses")
		case "unreadable":
			warn("checks_unreadable")
			notes = append(notes, "neither check runs nor Actions workflow runs are readable: only commit statuses are seen and no-checks is never reported; grant Actions: read or use a GitHub App")
		}
		if note, unreadable := requiredChecksNote(ctx, client, s, a); note != "" {
			if unreadable {
				warn("required_checks_unreadable")
			}
			notes = append(notes, note)
		}
		check.Items = append(check.Items, item+strings.Join(notes, "; "))
		for _, w := range x.writerItems(ctx, e, client) {
			raise(w.status, w.code)
			check.Items = append(check.Items, w.item)
		}
	}
	sort.Strings(check.Items)
	return check
}

// driftNotes says where the client an entry runs with differs from one
// built from the current connection config: who writes go out as, and
// whether git works as the bot.
func driftNotes(live, configured *Client) []string {
	var out []string
	if live.Writer() != configured.Writer() {
		out = append(out, "writes still go out as "+live.Writer()+", the config names "+configured.Writer())
	}
	if live.GitBot() != configured.GitBot() {
		who := func(bot bool) string {
			if bot {
				return "the bot"
			}
			return "the owner"
		}
		out = append(out, "git still works as "+who(live.GitBot())+", the config says "+who(configured.GitBot()))
	}
	return out
}

type writerItem struct{ status, code, item string }

// writerItems verifies, for an entry, each other `github` connection its
// scope's bindings name, which those bindings' comments and reviews go out
// as, the way the entry's own bot writer is verified: signed in as that
// connection, it reads the entry's repository.
func (x *Extension) writerItems(ctx context.Context, e ViewEntry, entry *Client) []writerItem {
	s := e.Settings
	var out []writerItem
	for _, id := range x.bindingConnections(e.Scope) {
		if id == e.Connection {
			continue
		}
		item := s.Namespace + ": binding connection " + id + ": "
		client, err := x.clients.Client(e.Scope, id, entry)
		if err != nil {
			out = append(out, writerItem{inproc.CheckFail, "writer_unavailable", item + "writer_unavailable"})
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		a, err := client.Verify(cx, s.Repo)
		cancel()
		if err != nil {
			code := verifyCode(err)
			out = append(out, writerItem{inproc.CheckFail, code, item + code})
			continue
		}
		note := "authenticated"
		if a.Writer {
			note = "bot writer authenticated"
		}
		if a.Known && !a.Push && !a.Writer {
			out = append(out, writerItem{inproc.CheckWarn, "read_only", item + note + "; reviews and comments need write access"})
			continue
		}
		out = append(out, writerItem{inproc.CheckOK, "verified", item + note})
	}
	return out
}

// requiredChecksNote reports where the checks the base branch requires
// (08-01) are read from: the branch protection and rulesets of the entry's
// base branch. A refused protection read warns, since a check it requires is
// then not waited for; refused rulesets (a plan without them) are a note.
func requiredChecksNote(ctx context.Context, c *Client, s Settings, a Access) (string, bool) {
	cx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base := s.BaseBranch
	if base == "" && s.Repo == s.TrackerRepo {
		base = a.DefaultBranch
	}
	if base == "" {
		var err error
		if base, err = c.DefaultBranch(cx, s.Repo); err != nil || base == "" {
			return "", false
		}
	}
	req, err := c.RequiredChecks(cx, s.Repo, base)
	if err != nil {
		return "required checks on " + base + " could not be read", false
	}
	var parts []string
	if len(req.Names) > 0 {
		parts = append(parts, fmt.Sprintf("%s requires %s (from %s)", base, strings.Join(req.Names, ", "), strings.Join(req.From, " and ")))
	}
	if note := RequiredNote(req, a.Checks); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "; "), slices.Contains(req.Refused, RequiredFromProtection)
}
