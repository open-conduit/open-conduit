package migrate

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	gh "github.com/open-conduit/open-conduit/extensions/github"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// feed is the v1 source route that feeds a project without a tracker of its
// own.
type feed struct {
	source *v1Source
	route  v1Route
}

// scopeBuilder accumulates one scope's entries.
type scopeBuilder struct {
	id, where   string
	bindings    []any
	routes      []any
	extensions  []any
	secretRefs  obj
	connections []string
	operations  []string
	// issueRoutes follow the job routes; intro is the introspection config.
	issueRoutes []any
	intro       obj
}

func (b *scopeBuilder) conn(id string) {
	if id != "" && !slices.Contains(b.connections, id) {
		b.connections = append(b.connections, id)
	}
}

func (b *scopeBuilder) ops(ops ...string) {
	for _, op := range ops {
		if !slices.Contains(b.operations, op) {
			b.operations = append(b.operations, op)
		}
	}
}

func (b *scopeBuilder) binding(m obj) {
	if ops, ok := m["operations"].([]string); ok {
		b.ops(ops...)
	}
	if conn, ok := m["connection"].(string); ok {
		b.conn(conn)
	}
	b.bindings = append(b.bindings, m)
}

func route(name, handler, binding, policy string, config obj) obj {
	if config == nil {
		config = obj{}
	}
	return obj{"name": name, "handler": handler, "binding": binding, "active_policy": policy, "config": config}
}

// name joins an identifier prefix, a converted name and a suffix within the
// 63-character identifier limit.
func name(prefix, base, suffix string) string {
	room := 63 - len(prefix) - len(suffix)
	if len(base) > room {
		base = strings.TrimRight(base[:room], "-_")
	}
	return prefix + base + suffix
}

func (c *converter) projects() []any {
	feeds := map[string]feed{}
	for _, s := range c.v1.Sources {
		for _, r := range s.Routes {
			if _, dup := feeds[r.Project]; !dup {
				feeds[r.Project] = feed{source: s, route: r}
			}
		}
	}
	var scopes []any
	first := true
	for _, p := range c.v1.Projects {
		if p == nil {
			continue
		}
		f, fed := feeds[p.Name]
		var fp *feed
		if fed && p.Tracker == nil {
			fp = &f
		}
		if s := c.project(p, fp, first); s != nil {
			scopes = append(scopes, s)
			first = false
		}
	}
	return scopes
}

func (c *converter) connType(name string) string {
	return str(c.v1.Connections[name]["type"])
}

// project converts one v1 project into a scope, or reports why not.
func (c *converter) project(p *v1Project, f *feed, first bool) obj {
	where := "projects." + p.Name
	id := ID(p.Name, "scope")
	for n := 2; c.scopeIDs[id]; n++ {
		id = name("", ID(p.Name, "scope"), fmt.Sprintf("-%d", n))
	}
	c.scopeIDs[id] = true
	if id != p.Name {
		c.note(Changed, where, "/scopes/"+id, "scope id %s (the v1 name stays the display name)", id)
	}
	b := &scopeBuilder{id: id, where: where, secretRefs: obj{}}

	labels := c.v1.Settings.Labels.merge(v1DefaultLabels())
	trackerB, repoB := p.Tracker, p.Repo
	var routeMatch map[string][]string
	if f != nil {
		trackerB = f.source.Tracker
		if f.source.Labels != nil {
			labels = f.source.Labels.merge(labels)
		}
		var complex bool
		routeMatch, complex = match(f.route.When)
		if complex {
			c.note(Unsupported, "sources."+f.source.Name+".routes."+p.Name, "", "any/all/not and nested values are not converted: the route match keeps only the plain attribute lists; review it before switching")
		}
		for attr, values := range routeMatch {
			for _, v := range values {
				if strings.ContainsAny(v, "*?") {
					c.note(Unsupported, "sources."+f.source.Name+".routes."+p.Name+".when."+attr, "", "glob patterns are not supported by tracker route matches (values compare exactly, ignoring case)")
				}
			}
		}
		if f.source.Unrouted == "label" {
			c.note(Unsupported, "sources."+f.source.Name+".unrouted", "", "the unrouted label is not projected by Open Conduit: an issue no route takes is left alone")
		}
	} else if p.Labels != nil {
		labels = p.Labels.merge(labels)
	}
	if repoB == nil {
		c.note(Unsupported, where, "", "no repo binding: not converted")
		return nil
	}
	trackerType, repoType := "", c.connType(repoB.Connection)
	if trackerB != nil {
		trackerType = c.connType(trackerB.Connection)
	} else if repoType == "github" {
		trackerB, trackerType = repoB, "github"
	}
	checkout := c.path(p.Root)
	if checkout == "" {
		c.note(Unsupported, where, "", "no root (checkout): not converted")
		return nil
	}
	if !filepath.IsAbs(checkout) {
		c.note(Warn, where+".root", "", "a relative checkout resolves against the new config file's directory")
	}
	git := c.gitConfig(p, checkout)
	layer := c.env(b, p, where)
	agent := c.agentConfig(p, where)
	for k, v := range envObj(layer) {
		agent[k] = v
	}
	intro := c.introspection(p, b, git, first)

	switch {
	case trackerType == "github" && (f != nil || trackerB.get("repo") != repoB.get("repo")):
		c.note(Unsupported, where, "", "a GitHub tracker in another repository (or a shared GitHub source) is not converted: configure the code repository's tracker entry by hand (github extension tracker/track_issues)")
		return nil
	case trackerType == "github":
		c.githubScope(b, p, repoB, labels, agent, layer, git, intro)
	case trackerType == "linear" || trackerType == "jira":
		if !c.trackerScope(b, p, trackerType, trackerB, repoB, repoType, routeMatch, labels, agent, layer, git, intro, f) {
			return nil
		}
	default:
		c.note(Unsupported, where, "", "tracker connection type %q is not converted", trackerType)
		return nil
	}
	c.jobs(b, p, checkout, git, layer)
	b.routes = append(b.routes, b.issueRoutes...)
	c.introspectionRoutes(b, intro)
	for i, conn := range c.posthog {
		n := "posthog"
		if i > 0 {
			n = name("posthog-", conn, "")
		}
		b.conn(conn)
		b.extensions = append(b.extensions, obj{"name": n, "type": "posthog", "connection": conn, "secret_names": []string{}, "config": obj{}})
	}
	c.additions(p, checkout, trackerType)
	c.res.Scopes = append(c.res.Scopes, id)
	operations := append([]string{"emit"}, slices.DeleteFunc(slices.Clone(b.operations), func(s string) bool { return s == "emit" })...)
	if b.connections == nil {
		b.connections = []string{}
	}
	scope := obj{"id": id, "name": p.Name,
		"authorization": obj{"connections": b.connections, "operations": operations, "read_scopes": []string{}},
		"bindings":      b.bindings, "routes": b.routes, "extensions": b.extensions, "secret_refs": b.secretRefs}
	if p.Disabled {
		scope["enabled"] = false
		c.note(Info, where+".disabled", "/scopes/"+id+"/enabled", "the scope is converted disabled")
	}
	return scope
}

// gitConfig is the git resource config shared by the scope's bindings.
func (c *converter) gitConfig(p *v1Project, checkout string) obj {
	g := c.v1.Settings.Git
	if p.Git != nil {
		remote := g.Remote
		g = *p.Git
		if g.Remote == "" {
			g.Remote = remote
		}
	}
	cfg := obj{"checkout": checkout, "dotenv": true}
	if g.Remote != "" && g.Remote != "origin" {
		cfg["remote"] = g.Remote
	}
	if g.Sign {
		cfg["sign"] = true
	}
	if p.BaseBranch != "" {
		cfg["base_branch"] = p.BaseBranch
	}
	if len(p.Worktree.Copy) > 0 {
		cfg["copy"] = p.Worktree.Copy
	}
	if p.Worktree.Setup != "" {
		cfg["setup"] = p.Worktree.Setup
		if usesAgentTask(p.Worktree.Setup) {
			c.agentTask("projects." + p.Name + ".worktree.setup")
		}
	}
	// v1 1.3.0 (#63) also refuses a new worktree that would leave its disk
	// fuller than settings.host.disk, whether or not the host watch runs.
	if d := c.v1.Settings.Host.Disk; d > 0 && d != 90 {
		cfg["disk_full_percent"] = min(d, 100)
	}
	if len(p.Worktree.SetupWhen) > 0 {
		cfg["setup_when"] = p.Worktree.SetupWhen
	}
	return cfg
}

func resource(git obj, mode string) obj {
	cfg := obj{}
	for k, v := range git {
		cfg[k] = v
	}
	cfg["mode"] = mode
	return obj{"provider": "git", "config": cfg}
}

// env maps the project env into a layer, adding referenced secrets to the
// scope's secret_refs.
func (c *converter) env(b *scopeBuilder, p *v1Project, where string) envLayer {
	l := envLayer{env: map[string]string{}, secretEnv: map[string]string{}}
	for _, k := range sortedKeys(p.Env) {
		v := p.Env[k]
		if !envPattern.MatchString(k) || strings.HasPrefix(k, "OS_") {
			c.note(Unsupported, where+".env."+k, "", "not a usable variable name (OS_* is reserved for the toolbelt)")
			continue
		}
		if isReference(v) {
			secret := name("env-", ID(k, "var"), "")
			b.secretRefs[secret] = c.secret(where+".env."+k, b.id+"-"+secret, v)
			l.secretEnv[secret] = k
			continue
		}
		l.env[k] = v
	}
	return l
}

func envObj(l envLayer) obj {
	out := obj{}
	if len(l.env) > 0 {
		out["env"] = l.env
	}
	if len(l.secretEnv) > 0 {
		out["secret_env"] = l.secretEnv
	}
	return out
}

// branch converts a v1 branch template. GitHub keys are issue numbers
// ({number}); tracker keys are issue/KEY ({key}), and the default becomes
// 06-01's agent/{key}.
func (c *converter) branch(p *v1Project, trackerType string) string {
	tpl := p.Branch
	if tpl == "" {
		tpl = c.v1.Settings.Branch
	}
	if tpl == "" {
		tpl = "agent/issue-{key}"
	}
	where := "projects." + p.Name + ".branch"
	if trackerType == "github" {
		out := strings.ReplaceAll(tpl, "{key}", "{number}")
		c.note(Info, where, "", "branch template %s (GitHub issue numbers, the same branch names as v1)", out)
		return out
	}
	if tpl == "agent/issue-{key}" {
		c.note(Changed, where, "", "tracker branches are agent/{key}, so agent/issue/ENG-12 rather than v1's agent/issue-ENG-12 (06-01); an open v1 branch keeps correlating through the key in its PR title or description")
		return "agent/{key}"
	}
	c.note(Warn, where, "", "a custom tracker branch template is kept, but {key} now expands to issue/KEY: review it")
	return tpl
}

func (c *converter) webhooks() bool { return c.v1.Settings.Tunnel != "" }

// webhookSecret adds a fresh scope webhook secret and its action.
func (c *converter) webhookSecret(b *scopeBuilder, secret, what string) {
	id := name(b.id+"-", secret, "")
	b.secretRefs[secret] = "keyring:" + id
	c.res.Secrets = append(c.res.Secrets, Secret{From: b.where + " (" + what + ")", Scheme: "new", To: "keyring:" + id,
		Action: fmt.Sprintf("generate a new webhook secret: `openssl rand -hex 32 | open-conduit secret set %s`", id)})
}

func (c *converter) githubScope(b *scopeBuilder, p *v1Project, repoB *v1Binding, labels v1Labels, agent obj, layer envLayer, git obj, intro bool) {
	where := b.where
	conn := c.connIDs[repoB.Connection]
	repo := repoB.get("repo")
	v := c.v1.Connections[repoB.Connection]
	host := "github.com"
	if u, err := url.Parse(str(v["apiUrl"])); err == nil && u.Host != "" && u.Host != "api.github.com" {
		host = u.Host
	}
	entry := obj{"namespace": "github/" + host + "/" + repo, "scope": b.id, "repo": repo}
	if l := labelsObj(labels, true); l != nil {
		entry["labels"] = l
		c.note(Info, where+".labels", "", "custom labels are kept, so both runtimes read the same claim and status labels")
	}
	if pp := str(v["previewPattern"]); pp != "" {
		entry["preview_pattern"] = pp
	}
	if p.BaseBranch != "" {
		entry["base_branch"] = p.BaseBranch
	}
	if n := c.v1.Settings.NoChecksMinutes; n > 0 && n != 10 {
		entry["no_checks_minutes"] = n
	}
	secrets := []string{}
	if c.webhooks() {
		entry["webhook_secret"], entry["manage_webhook"] = "github-webhook", true
		c.webhookSecret(b, "github-webhook", "GitHub webhook")
		secrets = append(secrets, "github-webhook")
		c.note(Action, where, "", "manage_webhook re-points the repository hook at Open Conduit when the scope starts; v1 must not run this project at the same time")
	}
	ops := slices.Clone(gh.AgentOperations)
	if intro {
		ops = append(ops, is.OpReport, is.OpCorrection, is.OpProblem)
	}
	agent["branch"] = c.branch(p, "github")
	b.conn(conn)
	b.binding(obj{"name": "agent", "executor": "claude-code", "connection": conn,
		"requires":   obj{"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
		"operations": ops, "secret_names": layer.names(), "config": agent, "resource": resource(git, "work")})
	b.binding(obj{"name": "hold", "executor": gh.HoldExecutor, "connection": conn, "requires": obj{"toolbelt": "sdk"},
		"operations": slices.Clone(gh.HoldOperations), "secret_names": []string{}, "config": obj{}})
	b.routes = append(b.routes, route("held", gh.HoldHandler, "hold", "queue", nil))
	b.extensions = append(b.extensions, obj{"name": b.id, "type": gh.Type, "connection": conn, "secret_names": secrets, "config": entry})
	b.issueRoutes = append(b.issueRoutes, route("issues", gh.Type, "agent", "inject", nil))
}

func labelsObj(l v1Labels, scheduled bool) obj {
	d := v1DefaultLabels()
	out := obj{}
	set := func(k, v, def string) {
		if v != def {
			out[k] = v
		}
	}
	set("claimed", l.Claimed, d.Claimed)
	if scheduled {
		set("scheduled", l.Scheduled, d.Scheduled)
	}
	set("help", l.Help, d.Help)
	set("review", l.Review, d.Review)
	set("done", l.Done, d.Done)
	if !slices.Equal(l.Skip, d.Skip) {
		out["skip"] = append([]string{}, l.Skip...)
	}
	if len(l.Require) > 0 {
		out["require"] = l.Require
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var transitionKeys = []string{"claimed", "building", "review", "help", "done", "finished"}

func (c *converter) trackerScope(b *scopeBuilder, p *v1Project, typ string, trackerB, repoB *v1Binding, repoType string, routeMatch map[string][]string, labels v1Labels, agent obj, layer envLayer, git obj, intro bool, f *feed) bool {
	where := b.where
	conn := c.connIDs[trackerB.Connection]
	ns := typ + "/" + b.id
	entry := obj{"namespace": ns, "scope": b.id}
	var keys []string
	transitions := obj{}
	switch typ {
	case "linear":
		keys = splitList(trackerB.get("team"))
		entry["teams"] = keys
		if s := splitList(trackerB.get("states")); len(s) > 0 && strings.Join(s, ",") != "unstarted,started" {
			entry["states"] = s
		}
		for _, k := range transitionKeys {
			if v := trackerB.get("state" + strings.ToUpper(k[:1]) + k[1:]); v != "" {
				transitions[k] = v
			}
		}
	case "jira":
		keys = splitList(trackerB.get("project"))
		entry["projects"] = keys
		if j := trackerB.get("jql"); j != "" {
			entry["jql"] = j
		}
		if fl := splitList(trackerB.get("fields")); len(fl) > 0 {
			entry["fields"] = fl
		}
		for _, k := range transitionKeys {
			if v := trackerB.get("transition" + strings.ToUpper(k[:1]) + k[1:]); v != "" {
				transitions[k] = v
			}
		}
		if strings.Contains(labels.Claimed+labels.Help+labels.Review+labels.Done+strings.Join(labels.Skip, "")+strings.Join(labels.Require, ""), " ") {
			dash := func(v string) string { return strings.ReplaceAll(strings.TrimSpace(v), " ", "-") }
			labels.Claimed, labels.Help, labels.Review, labels.Done = dash(labels.Claimed), dash(labels.Help), dash(labels.Review), dash(labels.Done)
			labels.Skip, labels.Require = slices.Clone(labels.Skip), slices.Clone(labels.Require)
			for i := range labels.Skip {
				labels.Skip[i] = dash(labels.Skip[i])
			}
			for i := range labels.Require {
				labels.Require[i] = dash(labels.Require[i])
			}
			c.note(Changed, where+".labels", "", "Jira labels cannot contain spaces: spaces became dashes, so these differ from the v1 label names")
		}
	}
	if len(keys) == 0 {
		c.note(Unsupported, where, "", "the %s tracker binding names no team or project: not converted", typ)
		return false
	}
	if len(transitions) > 0 {
		entry["transitions"] = transitions
	}
	if l := labelsObj(labels, false); l != nil {
		entry["labels"] = l
	}
	if labels.Scheduled != "" && labels.Scheduled != v1DefaultLabels().Scheduled {
		c.note(Unsupported, where+".labels.scheduled", "", "scheduled holds are GitHub-only in Open Conduit (06-01)")
	}
	secrets := []string{}
	if c.webhooks() {
		entry["webhook_secret"], entry["manage_webhook"] = "tracker-webhook", true
		c.webhookSecret(b, "tracker-webhook", typ+" webhook")
		secrets = append(secrets, "tracker-webhook")
	}
	if f != nil {
		c.note(Changed, "sources."+f.source.Name, "/scopes/"+b.id+"/extensions/0", "a shared v1 source becomes one %s entry per scope (namespace %s) with a route match; keep the matches disjoint (06-01)", typ, ns)
	}
	ops := tracker.AgentOperations(typ)
	if intro {
		ops = append(ops, is.OpReport, is.OpCorrection, is.OpProblem)
	}
	agent["playbook"] = tracker.Playbook
	agent["args"] = "{key}"
	agent["branch"] = c.branch(p, typ)
	b.conn(conn)
	b.extensions = append(b.extensions, obj{"name": b.id, "type": typ, "connection": conn, "secret_names": secrets, "config": entry})
	routeCfg := obj{}
	if len(routeMatch) > 0 {
		routeCfg["match"] = routeMatch
	}
	routes := []any{route("issues", typ, "agent", "inject", routeCfg)}
	switch repoType {
	case "bitbucket":
		bconn := c.connIDs[repoB.Connection]
		code := obj{"namespace": "bitbucket/" + strings.ToLower(repoB.get("repo")), "scope": b.id, "repo": repoB.get("repo"),
			"issue_ref": "Closes {key}", "tracker": obj{"namespace": ns, "keys": keys}}
		if pp := str(c.v1.Connections[repoB.Connection]["previewPattern"]); pp != "" {
			code["preview_pattern"] = pp
		}
		if p.BaseBranch != "" {
			code["base_branch"] = p.BaseBranch
		}
		codeSecrets := []string{}
		if c.webhooks() {
			code["webhook_secret"], code["manage_webhook"] = "code-webhook", true
			c.webhookSecret(b, "code-webhook", "Bitbucket webhook")
			codeSecrets = append(codeSecrets, "code-webhook")
		}
		ops = append(ops, tracker.BitbucketOperations...)
		b.conn(bconn)
		b.extensions = append(b.extensions, obj{"name": "code", "type": "bitbucket", "connection": bconn, "secret_names": codeSecrets, "config": code})
		routes = append(routes, route("prs", "bitbucket", "agent", "inject", nil))
	case "github":
		c.note(Unsupported, where+".repo", "", "a GitHub code repository for a %s tracker: Open Conduit correlates only Bitbucket pull requests with tracker issues; agents still push branches and open PRs, but PR events do not reach the issue's Work", typ)
	}
	b.binding(obj{"name": "agent", "executor": "claude-code", "connection": conn,
		"requires":   obj{"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
		"operations": ops, "secret_names": layer.names(), "config": agent, "resource": resource(git, "work")})
	b.issueRoutes = append(b.issueRoutes, routes...)
	return true
}

// jobs converts the config's project jobs and the checkout's repository
// file into the scope's schedule entry, bindings and routes (05-01, 05-02).
func (c *converter) jobs(b *scopeBuilder, p *v1Project, checkout string, git obj, layer envLayer) {
	where := b.where
	var jobs []v1Job
	for _, j := range p.Jobs {
		j.origin = where + ".jobs"
		jobs = append(jobs, j)
	}
	if c.opts.ReadFile != nil {
		if data, ok := c.opts.ReadFile(filepath.Join(checkout, RepoFile)); ok {
			own, err := parseRepoFile(data)
			if err != nil {
				c.note(Warn, where+" "+RepoFile, "", "not read: %s", err)
			}
			for _, j := range own {
				j.origin = where + " " + RepoFile
				jobs = append(jobs, j)
			}
			c.note(Changed, where+" "+RepoFile, "/scopes/"+b.id+"/extensions", "converted into the scope's schedule entry: Open Conduit never reads %s, so a later edit there needs another conversion or a config edit and apply", RepoFile)
		}
	}
	if len(jobs) == 0 {
		return
	}
	if on := c.v1.Settings.Jobs; on != nil && !*on {
		c.note(Unsupported, "settings.jobs", "", "jobs are off in v1: %d job(s) of %s are not converted", len(jobs), p.Name)
		return
	}
	entryJobs := []any{}
	seen := map[string]bool{}
	webhook := false
	var later []any
	for _, j := range jobs {
		jw := j.origin + "." + j.Name
		if j.Name == "" || len(j.Command) == 0 {
			c.note(Warn, j.origin, "", "a job without a name or command is left out (as v1 did)")
			continue
		}
		if usesAgentTask(strings.Join(j.Command, " ")) {
			c.agentTask(jw + ".command")
		}
		jn := ID(j.Name, "job")
		for n := 2; seen[jn]; n++ {
			jn = name("", ID(j.Name, "job"), fmt.Sprintf("-%d", n))
		}
		seen[jn] = true
		if jn != j.Name {
			c.note(Changed, jw, "", "job name %s", jn)
		}
		job := obj{"name": jn}
		if j.EveryMinutes > 0 {
			job["every_minutes"] = j.EveryMinutes
		}
		if len(j.At) > 0 {
			job["at"] = j.At
		}
		if j.Cron != "" {
			job["cron"] = j.Cron
		}
		if j.TimeZone != "" {
			job["time_zone"] = j.TimeZone
		}
		triggers := []string{}
		if j.On != nil {
			on := obj{}
			if len(j.On.Events) > 0 {
				on["events"] = j.On.Events
				triggers = append(triggers, "event")
			}
			if len(j.On.Branches) > 0 {
				on["branches"] = j.On.Branches
			}
			if len(j.On.Paths) > 0 {
				on["paths"] = j.On.Paths
			}
			if j.On.Webhook {
				on["webhook"] = true
				webhook = true
				triggers = append(triggers, "webhook")
				c.note(Action, jw+".on.webhook", "", "the job's webhook URL changes to /schedule/%s/jobs/%s/<token>: update its senders with the URL doctor --check-secrets prints (schedule_webhooks)", b.id, jn)
			}
			job["on"] = on
		}
		if j.EveryMinutes > 0 || len(j.At) > 0 || j.Cron != "" {
			c.note(Changed, jw, "", "a converted timed job's first run waits for its next slot (an interval job runs at once); drain v1's pending run first so no slot is lost or run twice")
		}
		entryJobs = append(entryJobs, job)

		shellCfg := obj{"argv": j.Command}
		for k, v := range envObj(layer) {
			shellCfg[k] = v
		}
		bindingName := name("job-", jn, "")
		bind := obj{"name": bindingName, "operations": []string{}, "secret_names": layer.names(), "requires": obj{"workspace": true}}
		mode := "shared"
		if j.Checkout == "fresh" {
			mode = "fresh"
		}
		bind["resource"] = resource(git, mode)
		if j.Status != "" {
			bind["timeout_seconds"] = c.opts.DetachedTimeoutSeconds
			c.note(Changed, jw+".status", "/scopes/"+b.id+"/bindings/"+bindingName+"/timeout_seconds", "a detached job had no deadline in v1; the binding deadline is %d s, above its longest expected run: review it", c.opts.DetachedTimeoutSeconds)
		} else {
			minutes := j.TimeoutMinutes
			if minutes <= 0 {
				minutes = 5
			}
			shellCfg["timeout_seconds"] = minutes * 60
			if j.TimeoutMinutes <= 0 {
				c.note(Changed, jw+".timeoutMinutes", "", "v1's implicit five-minute timeout is written explicitly (timeout_seconds 300)")
			}
		}
		if j.Status != "" || !c.opts.ShellJobs {
			cfg := obj{"shell": shellCfg}
			if j.Status != "" {
				cfg["status"] = j.Status
			}
			bind["executor"], bind["config"] = sc.LegacyExecutorName, cfg
		} else {
			bind["executor"], bind["config"] = "shell", shellCfg
			c.note(Warn, jw, "", "plain shell binding (--shell-jobs): the script must not read OPEN_SUPERVISOR_* names")
		}
		if j.KeepRuns > 20 {
			c.note(Warn, jw+".keepRuns", "/settings/kept_executions", "keepRuns %d is above kept_executions (20): raise settings.kept_executions to keep that much run content", j.KeepRuns)
		}
		b.binding(bind)
		if j.Status != "" {
			later = append(later, route(name("job-", jn, "-slots"), sc.Type, bindingName, "queue", obj{"jobs": []string{jn}, "triggers": []string{"timer"}}),
				route(bindingName, sc.Type, bindingName, "drop", obj{"jobs": []string{jn}, "triggers": append(triggers, "manual")}))
		} else {
			later = append(later, route(bindingName, sc.Type, bindingName, "queue", obj{"jobs": []string{jn}}))
		}
	}
	if len(entryJobs) == 0 {
		return
	}
	cfg := obj{"namespace": "schedule/" + b.id, "scope": b.id, "jobs": entryJobs}
	secrets := []string{}
	if webhook {
		cfg["webhook_secret"] = "job-webhook"
		c.webhookSecret(b, "job-webhook", "job webhooks")
		secrets = append(secrets, "job-webhook")
	}
	b.extensions = append(b.extensions, obj{"name": "jobs", "type": sc.Type, "secret_names": secrets, "config": cfg})
	// Job routes go before the issue routes: they only match the schedule
	// entry's keys, and a later manual route must never take a job key.
	b.routes = append(b.routes, later...)
	c.note(Info, where+".jobs", "", "legacy-job bindings keep OPEN_SUPERVISOR_JOB, _JOB_RUN, _PROJECT, _JOB_TRIGGER, _EVENT*, _BASE_BRANCH, _WORKTREE and _REMOTE for the scripts (below the binding env, never over OS_*)")
}

// cronAt turns a v1 HH:MM into a daily cron.
func cronAt(at string) (string, bool) {
	m := hhmm.FindStringSubmatch(strings.TrimSpace(at))
	if m == nil {
		return "", false
	}
	h := strings.TrimLeft(m[1], "0")
	if h == "" {
		h = "0"
	}
	mm := strings.TrimLeft(m[2], "0")
	if mm == "" {
		mm = "0"
	}
	return mm + " " + h + " * * *", true
}

// introspection adds the scope's introspection entry (sweep, triage, host
// watch, telemetry) and reports whether it did.
func (c *converter) introspection(p *v1Project, b *scopeBuilder, git obj, first bool) bool {
	s := c.v1.Settings
	at := func(v *string, def string) string {
		if v == nil {
			return def
		}
		return *v
	}
	zone := c.opts.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	cfg := obj{}
	remote, _ := git["remote"].(string)
	land := func(l string) string {
		if l == "" {
			return "push"
		}
		return l
	}
	if sweepAt := at(s.SweepAt, "08:41"); sweepAt != "" && !p.Sweep.Disabled {
		if cr, ok := cronAt(sweepAt); ok {
			sw := obj{"cron": cr, "time_zone": zone, "land": land(p.Sweep.Land)}
			if len(p.Sweep.RuleFiles) > 0 {
				sw["rules"] = p.Sweep.RuleFiles
			}
			if p.Sweep.Guide != "" {
				sw["guide"] = p.Sweep.Guide
			}
			if remote != "" {
				sw["remote"] = remote
			}
			cfg["sweep"] = sw
		} else {
			c.note(Warn, "settings.sweepAt", "", "not HH:MM: the sweep is not scheduled")
		}
	}
	if triageAt := at(s.TriageAt, "08:51"); triageAt != "" && !p.Triage.Disabled {
		if cr, ok := cronAt(triageAt); ok {
			tr := obj{"cron": cr, "time_zone": zone, "land": land(p.Triage.Land)}
			if p.Triage.OpenIssues {
				tr["file_issues"] = true
			}
			if remote != "" {
				tr["remote"] = remote
			}
			cfg["triage"] = tr
		} else {
			c.note(Warn, "settings.triageAt", "", "not HH:MM: the triage is not scheduled")
		}
	}
	if first && s.Host.Minutes > 0 {
		hw := obj{"interval_seconds": s.Host.Minutes * 60}
		if s.Host.Load > 0 {
			hw["load"] = s.Host.Load
		}
		if s.Host.Memory > 0 {
			hw["memory"] = s.Host.Memory
		}
		if s.Host.Disk > 0 {
			hw["disk"] = s.Host.Disk
		}
		if len(s.Host.Disks) > 0 {
			disks := []string{}
			for _, d := range s.Host.Disks {
				disks = append(disks, c.path(d))
			}
			hw["disks"] = disks
		}
		cfg["host_watch"] = hw
		c.note(Changed, "settings.host", "/scopes/"+b.id+"/extensions", "the host watch is machine-wide: it is converted once, in scope %s, and crossings become introspection reports", b.id)
	}
	if len(cfg) == 0 {
		return false
	}
	if len(c.posthog) > 0 {
		cfg["telemetry"] = true
	}
	if c.opts.TimeZone == "" && (cfg["sweep"] != nil || cfg["triage"] != nil) {
		c.note(Warn, "settings.sweepAt, settings.triageAt", "", "v1 read these in the machine's local time; they are written in UTC because no time zone was given (--time-zone)")
	}
	b.extensions = append(b.extensions, obj{"name": "introspection", "type": is.Type, "secret_names": []string{}, "config": cfg})
	b.intro = cfg
	return true
}

func (c *converter) introspectionRoutes(b *scopeBuilder, intro bool) {
	if !intro {
		return
	}
	cfg := b.intro
	git := obj{}
	for _, raw := range b.bindings {
		if r, ok := raw.(obj)["resource"].(obj); ok && raw.(obj)["name"] == "agent" {
			git = r["config"].(obj)
		}
	}
	claude := func(bind, playbook string, ops []string, mode string) obj {
		m := obj{"name": bind, "executor": "claude-code", "requires": obj{"toolbelt": "mcp", "workspace": mode != ""},
			"operations": ops, "secret_names": []string{}, "config": obj{"playbook": playbook}}
		if mode != "" {
			m["resource"] = resource(git, mode)
		}
		return m
	}
	if cfg["sweep"] != nil {
		b.binding(claude("sweep", "sweep", []string{is.OpEvidence, is.OpNotesResolve}, "shared"))
		b.routes = append(b.routes, route("sweep", is.Type, "sweep", "queue", obj{"work": is.WorkSweep}))
	}
	if cfg["triage"] != nil {
		b.binding(claude("triage", "triage-reports", []string{is.OpEvidence, is.OpList, is.OpFix, is.OpResolve, is.OpNotesResolve}, ""))
		b.binding(claude("fix", "fix-report", []string{is.OpList, is.OpResolve}, "work"))
		b.routes = append(b.routes, route("triage", is.Type, "triage", "queue", obj{"work": is.WorkTriage}),
			route("fixes", is.Type, "fix", "queue", obj{"work": "fix"}))
	}
	if (cfg["sweep"] != nil || cfg["triage"] != nil) && !c.introNoted {
		c.introNoted = true
		c.note(Changed, "settings.sweepAt, settings.triageAt", "", "the daily sweep and triage are the introspection extension's standing Work on claude-code bindings (playbooks sweep and triage-reports); fixes the triage requests run on the fix binding (fix-report)")
	}
}

// additions reports v1 playbook additions in the checkout and their Open
// Conduit names. The checkout is never modified.
func (c *converter) additions(p *v1Project, checkout, trackerType string) {
	if c.opts.ReadFile == nil {
		return
	}
	renames := map[string]string{"work-issue": "work-issue", "sweep": "sweep", "triage": "triage-reports"}
	if trackerType != "github" {
		renames["work-issue"] = tracker.Playbook
	}
	for _, old := range sortedKeys(renames) {
		text, ok := c.opts.ReadFile(filepath.Join(checkout, ".open-supervisor", "playbooks", old+".md"))
		if !ok {
			continue
		}
		next := renames[old]
		where := "projects." + p.Name + " .open-supervisor/playbooks/" + old + ".md"
		if usesAgentTask(string(text)) {
			c.agentTask(where)
		}
		if next == old {
			c.note(Info, where, "", "read as a fallback while the checkout has no .open-conduit/playbooks/%s.md; move it there when v1 is retired", next)
		} else {
			c.note(Action, where, "", "the playbook is now %s: copy it to .open-conduit/playbooks/%s.md (the v1 name is not read for this playbook)", next, next)
		}
	}
}

// agentTaskPattern finds v1 1.3.0's `agent task` toolbelt command (#65).
var agentTaskPattern = regexp.MustCompile(`\bagent\s+task\b`)

func usesAgentTask(s string) bool { return agentTaskPattern.MatchString(s) }

// agentTask reports a use of v1's `agent task`, which Open Conduit rejected
// (08-05): it is converted as written and fails there.
func (c *converter) agentTask(where string) {
	c.note(Unsupported, where, "", "uses v1's `agent task`, which Open Conduit does not support (rejected): rewrite it before switching, for example as a schedule job or a command the turn itself runs")
}
