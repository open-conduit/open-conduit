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
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// Extension is the GitHub extension on the in-process contract: its config
// types, entries, ingress, webhooks, projection, tools, actions,
// annotations and doctor checks (host.go). It resolves the credentials it
// uses through the host's scoped resolver (inproc.Host.Secrets), for the
// entry's connection and for each `github` connection a binding in the
// scope names. NewExtension with a view reports a host's registrations
// instead of its own, for tests of the checks.
type Extension struct {
	view ViewFunc

	mu      sync.Mutex
	host    inproc.Host
	started bool
	entries []protocol.ConfigEntry
	clients *Writers

	// own is the entries the extension registers itself (New); nil when
	// view reports a host's (tests).
	own   *registry
	tools *Tools
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

// SetupFlow is `open-conduit init github` (inproc.SetupFlower).
func (*Extension) SetupFlow() extension.Setup { return Setup{} }

// Types are the `github` connection and ingress entry types.
func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateConnection(raw) }, Fields: connectionFields()},
		{Kind: protocol.ValidateExtension, Type: Type, Ingress: true, Validate: ValidateExtension, Settings: ExtensionSettings, Fields: entryFields()},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateRoute(raw) }},
	}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.mu.Lock()
	x.host, x.started = h, true
	x.mu.Unlock()
	return x.startTools(h)
}

// Configure keeps the generation's entries, with the connection each names
// and those its scope's bindings name.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	x.mu.Lock()
	x.entries = p.Entries
	x.mu.Unlock()
	if x.own != nil {
		x.register(p)
	}
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
	if x.own != nil {
		return x.ownView()
	}
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
	hooks := map[string]string{}
	if x.own != nil {
		hooks = x.hookItems(raise)
	}
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
		if item, ok := hooks[e.Settings.Namespace]; ok {
			notes = append(notes, item)
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
func (x *Extension) authCheck(ctx context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
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
		live := !dry && !s.Projection.DryRun
		switch {
		case a.Known:
			notes = append(notes, fmt.Sprintf("pull=%t push=%t admin=%t", a.Pull, a.Push, a.Admin))
			if !a.Push && !a.Writer && live {
				warn("read_only")
				notes = append(notes, "labels, comments and PRs need write access")
			}
			if s.ManageWebhook && !a.Admin {
				warn("hook_admin_missing")
				notes = append(notes, "manage_webhook needs admin (hooks) permission")
			}
		case !a.App:
			notes = append(notes, "authenticated (the token's permissions are not reported)")
		}
		for _, n := range installationNotes(s, a, client, live) {
			if n.code != "" {
				warn(n.code)
			}
			notes = append(notes, n.note)
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
		note, unreadable, unguarded := requiredChecksNote(ctx, client, s, a)
		if note != "" {
			if unreadable {
				warn("required_checks_unreadable")
			}
			notes = append(notes, note)
		}
		if unguarded != "" {
			warn("base_unprotected")
			notes = append(notes, unguarded+" has no branch protection or ruleset, so nothing on GitHub stops a run's credential (contents: write) pushing straight to it; protect it or add a ruleset that requires a pull request")
		}
		if a.Public && len(s.Labels.Require) == 0 {
			if open := unconfinedAgents(cfg, string(e.Scope)); len(open) > 0 {
				warn("public_unconfined")
				notes = append(notes, fmt.Sprintf("%s is public, no labels.require is set and %s run unsandboxed on this machine: an issue or comment is untrusted text an agent acts on with your access; set labels.require, or run the binding in a sandbox or container", s.TrackerRepo, strings.Join(open, ", ")))
			}
		}
		check.Items = append(check.Items, item+strings.Join(notes, "; "))
		for _, w := range append(x.writerItems(ctx, e, client), x.permissionItems(ctx, cfg, e, client)...) {
			raise(w.status, w.code)
			check.Items = append(check.Items, w.item)
		}
	}
	sort.Strings(check.Items)
	return check
}

// reportedPermissions are the App installation permissions doctor lists,
// in the order GitHub's settings show them: what the minimum permissions
// in the operator docs name.
var reportedPermissions = []string{"contents", "issues", "pull_requests", "checks", "statuses", "repository_hooks"}

type installNote struct{ code, note string }

// installationNotes reports, for an entry whose sign-in or writer is a
// GitHub App, what each App's installation holds on the repository
// (reportedPermissions) and each permission a configured feature needs
// that it lacks: reads by the sign-in App for reconcile, Webhooks write
// by it for manage_webhook (hook_admin_missing), and by the App writes go
// out as, Issues, Pull requests and Contents write for live projection
// (labels, comments, PRs) and Contents write for git as the bot (permission_missing). An
// installation GitHub doesn't answer for warns installation_unreadable.
// A token sign-in with no bot App adds nothing: GitHub reports a token's
// role on the repository instead (Access.Known).
func installationNotes(s Settings, a Access, c *Client, live bool) []installNote {
	type app struct {
		who   string
		perms map[string]string
		needs map[string]map[string][]string // permission → level → features
	}
	need := func(x *app, name, level, feature string) {
		if x.needs[name] == nil {
			x.needs[name] = map[string][]string{}
		}
		x.needs[name][level] = append(x.needs[name][level], feature)
	}
	var apps []*app
	var reader, writer *app
	if a.App {
		reader = &app{who: c.Reader(), perms: a.Installation, needs: map[string]map[string][]string{}}
		apps = append(apps, reader)
		for _, p := range []string{"contents", "issues", "pull_requests", "checks", "statuses"} {
			need(reader, p, "read", "reconcile")
		}
		if s.ManageWebhook {
			need(reader, "repository_hooks", "write", "manage_webhook")
		}
	}
	writer = reader
	if a.Writer {
		writer = &app{who: c.Writer(), perms: a.BotInstallation, needs: map[string]map[string][]string{}}
		apps = append(apps, writer)
	}
	if writer != nil {
		if live {
			for _, p := range []string{"contents", "issues", "pull_requests"} {
				need(writer, p, "write", "projection")
			}
		}
		if c.GitBot() {
			need(writer, "contents", "write", "git as the bot")
		}
	}
	var out []installNote
	for _, x := range apps {
		if x.perms == nil {
			out = append(out, installNote{"installation_unreadable", fmt.Sprintf("%s's installation permissions on %s could not be read", x.who, s.Repo)})
			continue
		}
		held := make([]string, len(reportedPermissions))
		for i, p := range reportedPermissions {
			level := x.perms[p]
			if level == "" {
				level = "none"
			}
			held[i] = permissionLabel(p) + " " + level
		}
		out = append(out, installNote{"", fmt.Sprintf("%s holds %s", x.who, strings.Join(held, ", "))})
		for _, p := range reportedPermissions {
			var top string
			var features []string
			for _, level := range []string{"read", "write"} {
				if fs := x.needs[p][level]; len(fs) > 0 && !covers(x.perms[p], level) {
					top, features = level, append(features, fs...)
				}
			}
			if top == "" {
				continue
			}
			code := "permission_missing"
			if p == "repository_hooks" {
				code = "hook_admin_missing"
			}
			has := x.perms[p]
			if has == "" {
				has = "none"
			}
			verb := "needs"
			if len(features) > 1 {
				verb = "need"
			}
			want := Permission{p, top}
			out = append(out, installNote{code, fmt.Sprintf("%s %s %s, but %s has %s %s on %s; grant %s in the App's settings", joinWords(features), verb, want, x.who, permissionLabel(p), has, s.Repo, want)})
		}
	}
	return out
}

// joinWords joins words as a list: "a", "a and b", "a, b and c".
func joinWords(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
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

// permissionItems warns about each binding in the entry's scope granted an
// operation (opPermissions) whose GitHub App installation on the code
// repository lacks a permission it needs, so GitHub would refuse every
// call: github.pr.resolve without Contents write, say. Each operation is
// checked against the connection it writes as: a reply, review or resolve
// as the binding's own GitHub connection when it names one, opening or
// editing a PR always as the entry's (bindingWriterOps). A token connection, whose
// permissions GitHub doesn't report, and an installation that can't be read
// (the connection items say why) add nothing. It also warns
// (git_permission_missing) about a binding granted github.git.credential
// whose git token's App lacks a permission its other granted operations
// add to the token (gitOpPermissions): Issues write for an issue comment,
// say, without which a pushed closing keyword doesn't close its issue.
func (x *Extension) permissionItems(ctx context.Context, cfg protocol.CheckConfig, e ViewEntry, entry *Client) []writerItem {
	s := e.Settings
	github := map[string]bool{}
	for _, c := range cfg.Connections {
		if c.Type == Type {
			github[c.ID] = true
		}
	}
	type installed struct {
		perms map[string]string
		known bool
	}
	read := map[string]installed{}
	// installation is the client a connection writes with and its App
	// installation's permissions on the repository, read once; ok is false
	// for a connection that can't be built and for a token, whose
	// permissions GitHub doesn't report.
	installation := func(conn string) (*Client, installed, bool) {
		client := entry
		if conn != e.Connection {
			c, err := x.clients.Client(e.Scope, conn, entry)
			if err != nil {
				return nil, installed{}, false
			}
			client = c
		}
		h, ok := read[conn]
		if !ok {
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			perms, known, err := client.WriterPermissions(cx, s.Repo)
			cancel()
			h = installed{perms, known && err == nil}
			read[conn] = h
		}
		return client, h, h.known
	}
	held := func(level string) string {
		if level == "" {
			return "none"
		}
		return level
	}
	var out []writerItem
	for _, scope := range cfg.Scopes {
		if scope.ID != string(e.Scope) {
			continue
		}
		for _, b := range scope.Bindings {
			own := b.Connection
			if own == e.Connection || !github[own] {
				own = ""
			}
			byConn := grantedNeeds(b.Operations, own, e.Connection)
			conns := make([]string, 0, len(byConn))
			for conn := range byConn {
				conns = append(conns, conn)
			}
			sort.Strings(conns)
			for _, conn := range conns {
				needs := byConn[conn]
				client, h, ok := installation(conn)
				if !ok {
					continue
				}
				perms := make([]Permission, 0, len(needs))
				for p := range needs {
					perms = append(perms, p)
				}
				sort.Slice(perms, func(i, j int) bool { return perms[i].String() < perms[j].String() })
				for _, p := range perms {
					if covers(h.perms[p.Name], p.Level) {
						continue
					}
					out = append(out, writerItem{inproc.CheckWarn, "permission_missing", fmt.Sprintf("%s: binding %s is granted %s, but %s has %s %s on %s, not %s, so GitHub refuses it; grant %s in the App's settings, or take the operation off the binding", s.Namespace, b.Name, strings.Join(needs[p], ", "), client.Writer(), permissionLabel(p.Name), held(h.perms[p.Name]), s.Repo, p.Level, p)})
				}
			}
			// The git token: minted by the binding's own connection when it
			// names one, else the entry's, with the permissions its granted
			// operations need (gitOpPermissions). One the installation
			// lacks is left out of the token, so a push still works, but
			// what git does as the pusher with it doesn't: a pushed
			// closing keyword doesn't close its issue.
			if !slices.Contains(b.Operations, OpGitCredential) {
				continue
			}
			conn := e.Connection
			if own != "" {
				conn = own
			}
			client, h, ok := installation(conn)
			if !ok {
				continue
			}
			for _, p := range gitPermissionsFor(b.Operations) {
				if covers(h.perms[p.Name], p.Level) {
					continue
				}
				var ops []string
				for _, op := range b.Operations {
					if gitOpPermissions[op] == p {
						ops = append(ops, op)
					}
				}
				sort.Strings(ops)
				out = append(out, writerItem{inproc.CheckWarn, "git_permission_missing", fmt.Sprintf("%s: binding %s is granted %s and %s, but %s has %s %s on %s, not %s, so its git token goes without it and what git does as the pusher with it fails (a pushed \"Closes #N\" doesn't close the issue); grant %s in the App's settings", s.Namespace, b.Name, strings.Join(slices.Compact(ops), ", "), OpGitCredential, client.Writer(), permissionLabel(p.Name), held(h.perms[p.Name]), s.Repo, p.Level, p)})
			}
		}
	}
	return out
}

// unconfinedAgents names the bindings the scope's github routes start that
// run on the host with no sandbox or container.
func unconfinedAgents(cfg protocol.CheckConfig, scope string) []string {
	var out []string
	for _, sc := range cfg.Scopes {
		if sc.ID != scope {
			continue
		}
		bindings := map[string]protocol.CheckBinding{}
		for _, b := range sc.Bindings {
			bindings[b.Name] = b
		}
		for _, r := range sc.Routes {
			b, ok := bindings[r.Binding]
			if r.Handler == Type && ok && b.Isolation == "" && !slices.Contains(out, "binding "+b.Name) {
				out = append(out, "binding "+b.Name)
			}
		}
	}
	return out
}

// requiredChecksNote reports where the checks the base branch requires
// (08-01) are read from: the branch protection and rulesets of the entry's
// base branch. A refused protection read warns, since a check it requires is
// then not waited for; refused rulesets (a plan without them) are a note.
// unguarded names the base branch when both reads answered and neither
// protects it.
func requiredChecksNote(ctx context.Context, c *Client, s Settings, a Access) (note string, unreadable bool, unguarded string) {
	cx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base := s.BaseBranch
	if base == "" && s.Repo == s.TrackerRepo {
		base = a.DefaultBranch
	}
	if base == "" {
		var err error
		if base, err = c.DefaultBranch(cx, s.Repo); err != nil || base == "" {
			return "", false, ""
		}
	}
	req, err := c.RequiredChecks(cx, s.Repo, base)
	if err != nil {
		return "required checks on " + base + " could not be read", false, ""
	}
	if len(req.Refused) == 0 && !req.Guarded {
		unguarded = s.Repo + " " + base
	}
	var parts []string
	if len(req.Names) > 0 {
		parts = append(parts, fmt.Sprintf("%s requires %s (from %s)", base, strings.Join(req.Names, ", "), strings.Join(req.From, " and ")))
	}
	if note := RequiredNote(req, a.Checks); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "; "), slices.Contains(req.Refused, RequiredFromProtection), unguarded
}
