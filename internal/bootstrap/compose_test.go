package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// A package's manifest reaches the registry: its types (configurable
// routes too), handlers, operations and resource types, with its tasks
// paced by their intervals.
func TestInstalledCompose(t *testing.T) {
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		Connections: []protocol.TypeDecl{{Type: "acme_conn"}},
		Extensions:  []protocol.ExtensionDecl{{Type: "acme", Ingress: true}},
		Routes:      []protocol.RouteDecl{{Handler: "acme_route", Config: true}, {Handler: "acme_plain"}},
		Operations:  []protocol.OperationDecl{{Name: "acme.ping", Kind: protocol.OperationHandler}},
		Resources:   []protocol.ResourceDecl{{Type: "acme_box"}},
		Tasks:       []protocol.TaskDecl{{Name: "sweep", IntervalSeconds: 90}},
	}}
	x := &installed{m: m}
	reg := config.Registry{}
	x.providerTypes(&reg)
	packageRegistry(&reg, []*installed{x})
	if reg.Connections["acme_conn"] == nil || !reg.Extensions["acme"].Ingress || reg.HandlerConfig["acme_route"] == nil {
		t.Fatalf("types not registered: %+v", reg)
	}
	if !reg.Handlers["acme_route"] || !reg.Handlers["acme_plain"] || reg.HandlerConfig["acme_plain"] != nil {
		t.Fatalf("handlers: %v %v", reg.Handlers, reg.HandlerConfig)
	}
	if len(reg.Operations) != 1 || reg.Operations[0] != "acme.ping" || !reg.Resources["acme_box"] {
		t.Fatalf("operations %v resources %v", reg.Operations, reg.Resources)
	}
	if err := reg.Extensions["acme"].Validate([]byte(`{}`), nil); err != nil {
		t.Fatalf("offline validator: %v", err)
	}
	if tasks := x.Tasks(); len(tasks) != 1 || tasks[0].Name != "sweep" || tasks[0].Pace != 90*time.Second {
		t.Fatalf("tasks %+v", tasks)
	}
}

// A package's Work kinds, recurrences, ingress sources, webhook paths and
// executors. Offline (no running package) it declares its Work kinds, so a
// dry run still knows its timed ones, and serves nothing; a configure
// result's sources are kept by name, its webhook paths by path.
func TestInstalledSurfaces(t *testing.T) {
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		WorkKinds:   []protocol.WorkKindDecl{{Kind: "acme.job", Standing: true, Timed: true}, {Kind: "acme.chat"}},
		Recurrences: []string{"acme.every"},
		Executors:   []protocol.ExecutorDecl{{Name: "acme-run", Pool: protocol.PoolJobs}},
	}}
	x := &installed{m: m}
	kinds := x.WorkKinds()
	if len(kinds) != 2 || kinds[0].Kind != "acme.job" || !kinds[0].Standing || !kinds[0].Timed || kinds[0].Trigger != nil || kinds[1].Kind != "acme.chat" || kinds[1].Timed {
		t.Fatalf("work kinds %+v", kinds)
	}
	d := &daemon{packages: []*installed{x}}
	if timed := d.timedKinds(); !timed["acme.job"] || timed["acme.chat"] {
		t.Fatalf("timed kinds %v", timed)
	}
	if len(x.Recurrences()) != 0 || len(x.Executors()) != 0 || len(x.Sources()) != 0 {
		t.Fatalf("an offline package serves recurrences %v executors %v sources %v", x.Recurrences(), x.Executors(), x.Sources())
	}
	src := protocol.SourceDecl{Name: "acme/one"}
	x.configured(protocol.ConfigureResult{Sources: []protocol.SourceDecl{src}, WebhookPaths: []string{"/acme/hook"}})
	x.configured(protocol.ConfigureResult{Sources: []protocol.SourceDecl{src, {Name: "acme/two"}}})
	if len(x.sources) != 2 || x.sources[0].Name != "acme/one" || x.sources[1].Name != "acme/two" || !x.hooks["acme/hook"] {
		t.Fatalf("configured sources %+v hooks %v", x.sources, x.hooks)
	}
	if x.Webhook("other") != nil || x.Webhook("hook") != nil {
		t.Fatal("an offline package served a webhook")
	}
}

// The turn plugin carries the packages' playbooks, filled in. The setup
// playbook is the setup package's (#243): it joins the plugin as every
// package's does, and nothing compiled in supplies it.
func TestTurnPluginCarriesPackagePlaybooks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "playbooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "playbooks", "work-issue.md"), []byte("---\nname: work-issue\ndescription: Work an issue.\n---\n\nRun `{{bin}} help`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x := &installed{m: protocol.Manifest{Name: "acme", Provides: protocol.Provides{Playbooks: "playbooks"}}, dir: dir}
	d := &daemon{o: Options{StateDir: t.TempDir()}, packages: []*installed{x}}
	if err := d.preparePlaybooks(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.pluginDir(), "skills", "setup")); !os.IsNotExist(err) {
		t.Fatalf("a compiled-in setup playbook: %v", err)
	}
	for _, name := range []string{"work-issue"} {
		data, err := os.ReadFile(filepath.Join(d.pluginDir(), "skills", name, "SKILL.md"))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s skill: %v", name, err)
		}
		if strings.Contains(string(data), "{{bin}}") {
			t.Errorf("%s skill is not filled in", name)
		}
	}
}

// An extension's checks see each enabled scope that uses it, with every
// route and binding there, its own entries and the connections of its own
// types they name, without secret references; other scopes, other
// extensions' entries and other types' connections stay out.
func TestCheckConfig(t *testing.T) {
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		Connections: []protocol.TypeDecl{{Type: "acme_conn"}},
		Extensions:  []protocol.ExtensionDecl{{Type: "acme"}},
		Routes:      []protocol.RouteDecl{{Handler: "acme_route"}},
		Operations:  []protocol.OperationDecl{{Name: "acme.ping", Kind: protocol.OperationHandler}},
	}}
	x := &installed{m: m}
	s := &config.Snapshot{Order: []domain.ID{"entry", "route", "op", "conn", "other", "off"},
		Connections: []config.Connection{{ID: "a1", Type: "acme_conn", Config: json.RawMessage(`{"k":1}`), SecretRefs: map[string]string{"token": "env:ACME"}}, {ID: "o1", Type: "other"}},
		Scopes: map[domain.ID]*config.EffectiveScope{
			"entry": {ID: "entry", Enabled: true, Extensions: []config.Extension{{Name: "e", Type: "acme", Connection: "a1", SecretNames: []string{"token"}}, {Name: "g", Type: "github"}},
				Bindings: []config.Binding{{Name: "agent", Executor: "shell", Connection: "o1", Resource: &config.Resource{Provider: "git", Config: json.RawMessage(`{"checkout":"."}`)}}},
				Routes:   []config.Route{{Name: "r", Handler: "manual", Binding: "agent"}}},
			"route": {ID: "route", Enabled: true, Routes: []config.Route{{Name: "r", Handler: "acme_route", Binding: "b"}}},
			"op": {ID: "op", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Operations: []string{"acme.ping"}},
				{Name: "boxed", Executor: "shell", Sandbox: &config.Sandbox{Provider: "seatbelt"}}, {Name: "contained", Executor: "shell", Container: &config.Container{Provider: "docker"}}}},
			"conn":  {ID: "conn", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Connection: "a1"}}},
			"other": {ID: "other", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Operations: []string{"read_context"}}}},
			"off":   {ID: "off", Extensions: []config.Extension{{Name: "e", Type: "acme"}}},
		}}
	cfg := checkConfig(s, x, withShell(), "/cfg", nil)
	var ids []string
	for _, sc := range cfg.Scopes {
		ids = append(ids, sc.ID)
	}
	if strings.Join(ids, ",") != "entry,route,op,conn" || cfg.Dir != "/cfg" {
		t.Fatalf("scopes %v dir %q", ids, cfg.Dir)
	}
	e := cfg.Scopes[0]
	if len(e.Entries) != 1 || e.Entries[0].Name != "e" || e.Entries[0].Connection != "a1" || len(e.Entries[0].SecretNames) != 1 {
		t.Fatalf("entries %+v", e.Entries)
	}
	if len(e.Routes) != 1 || len(e.Bindings) != 1 || e.Bindings[0].Toolbelt != "cli" || e.Bindings[0].Resource == nil || e.Bindings[0].Resource.Provider != "git" {
		t.Fatalf("routes %+v bindings %+v", e.Routes, e.Bindings)
	}
	// Each binding says how its executor is confined.
	var isolation []string
	for _, b := range cfg.Scopes[2].Bindings {
		isolation = append(isolation, b.Name+"="+b.Isolation)
	}
	if strings.Join(isolation, ",") != "b=,boxed=sandbox,contained=container" || e.Bindings[0].Isolation != "" {
		t.Fatalf("isolation %v", isolation)
	}
	raw, _ := json.Marshal(cfg)
	if len(cfg.Connections) != 1 || cfg.Connections[0].ID != "a1" || strings.Contains(string(raw), "env:ACME") {
		t.Fatalf("connections %s", raw)
	}
	if got := checkConfig(nil, x, nil, "/cfg", nil); len(got.Scopes) != 0 {
		t.Fatalf("no snapshot: %+v", got)
	}
}

// A package's check is listed under its own name, kept apart from other
// packages' and the host's.
func TestCheckName(t *testing.T) {
	for _, tc := range [][3]string{{"notes", "notes", "notes"}, {"notes", "notes_store", "notes_store"}, {"slack", "slack-auth", "slack-auth"}, {"notes", "store", "notes_store"}, {"notes", "github", "notes_github"}} {
		if got := checkName(tc[0], tc[1]); got != tc[2] {
			t.Errorf("checkName(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

// probingExecutor is a shell executor that gives a version probe
// (extension.VersionProber) naming its binding config.
type probingExecutor struct{ extension.Executor }

func (probingExecutor) VersionProbe(raw json.RawMessage) ([]string, []string) {
	return []string{"tool", "--version", string(raw)}, []string{"PATH=/bin"}
}

// A container provider that runs probes (containers[].probe) sees the
// version probe of each binding in one of its containers whose executor
// gives one, and every view marks that binding Probed. A binding in a
// provider that runs none (podman here, as ssh), or whose executor gives
// none, carries neither, so its executor checks it as before.
func TestCheckConfigProbe(t *testing.T) {
	x := &installed{m: protocol.Manifest{Name: "docker", Provides: protocol.Provides{Containers: []protocol.ContainerDecl{{Type: "docker", Probe: true}}}}}
	podman := &installed{m: protocol.Manifest{Name: "podman", Provides: protocol.Provides{Containers: []protocol.ContainerDecl{{Type: "podman"}}}}}
	probing := probingContainers([]*installed{x, podman})
	executors := withShell()
	executors["prober"] = probingExecutor{executors["shell"]}
	s := &config.Snapshot{Order: []domain.ID{"s"}, Scopes: map[domain.ID]*config.EffectiveScope{"s": {ID: "s", Enabled: true, Bindings: []config.Binding{
		{Name: "probed", Executor: "prober", Config: json.RawMessage(`{"a":1}`), Container: &config.Container{Provider: "docker"}},
		{Name: "elsewhere", Executor: "prober", Container: &config.Container{Provider: "podman"}},
		{Name: "plain", Executor: "shell", Container: &config.Container{Provider: "docker"}},
	}}}}
	cfg := checkConfig(s, x, executors, "/cfg", probing)
	if len(cfg.Scopes) != 1 || len(cfg.Scopes[0].Bindings) != 3 {
		t.Fatalf("scopes %+v", cfg.Scopes)
	}
	bs := cfg.Scopes[0].Bindings
	if p := bs[0].Probe; p == nil || strings.Join(p.Argv, " ") != `tool --version {"a":1}` || strings.Join(p.Env, " ") != "PATH=/bin" {
		t.Errorf("probed binding's probe %+v", p)
	}
	if !bs[0].Probed || bs[1].Probe != nil || bs[1].Probed || bs[2].Probe != nil || bs[2].Probed {
		t.Errorf("probed %v %v %v, probes %+v %+v: want only the docker prober's", bs[0].Probed, bs[1].Probed, bs[2].Probed, bs[1].Probe, bs[2].Probe)
	}
	// The executor's own view (no container provider of its own) is told
	// the binding is probed, without the probe.
	view := checkConfig(s, &installed{m: protocol.Manifest{Name: "prober", Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: "prober"}}}}}, executors, "/cfg", probing)
	if b := view.Scopes[0].Bindings[0]; !b.Probed || b.Probe != nil || b.Container != nil {
		t.Errorf("executor's view of the probed binding %+v", b)
	}
}
