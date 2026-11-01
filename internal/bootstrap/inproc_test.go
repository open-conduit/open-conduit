package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// The first-party list composes through the contract alone: each
// extension's config types and operations reach the registry, and no two
// share a name.
func TestFirstPartyCompose(t *testing.T) {
	reg := registry(offlineExecutors())
	seen := map[string]bool{}
	for _, x := range firstParty(nil) {
		if seen[x.Name()] {
			t.Fatalf("%s listed twice", x.Name())
		}
		seen[x.Name()] = true
		if typed, ok := x.(inproc.Typed); ok {
			for _, c := range typed.Types() {
				_, conn := reg.Connections[c.Type]
				_, entry := reg.Extensions[c.Type]
				if !conn && !entry {
					t.Errorf("%s type %s not registered", x.Name(), c.Type)
				}
			}
		}
		if o, ok := x.(inproc.Operator); ok {
			for _, op := range o.Operations() {
				found := false
				for _, r := range reg.Operations {
					found = found || r == op
				}
				if !found {
					t.Errorf("%s operation %s not registered", x.Name(), op)
				}
			}
		}
	}
}

// An installed package's manifest reaches the registry through the same
// contract: its types (configurable routes too), handlers, operations and
// resource types, with its tasks paced by their intervals.
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
	x.resourceTypes(&reg)
	inprocRegistry(&reg, []inproc.Extension{x})
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

// The turn plugin carries the work-issue playbook and the in-process
// extensions' own, filled in. The setup playbook is the setup package's
// (#243): it joins the plugin as every package's does, and nothing compiled
// in supplies it.
func TestTurnPluginCarriesFirstPartyPlaybooks(t *testing.T) {
	d := &daemon{o: Options{StateDir: t.TempDir()}, inproc: firstParty(nil)}
	if err := d.preparePlaybooks(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.pluginDir(), "skills", "setup")); !os.IsNotExist(err) {
		t.Fatalf("a compiled-in setup playbook: %v", err)
	}
	for _, name := range []string{"work-issue", "sweep"} {
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
			"op":    {ID: "op", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Operations: []string{"acme.ping"}}}},
			"conn":  {ID: "conn", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Connection: "a1"}}},
			"other": {ID: "other", Enabled: true, Bindings: []config.Binding{{Name: "b", Executor: "shell", Operations: []string{"read_context"}}}},
			"off":   {ID: "off", Extensions: []config.Extension{{Name: "e", Type: "acme"}}},
		}}
	cfg := checkConfig(s, x, offlineExecutors(), "/cfg")
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
	raw, _ := json.Marshal(cfg)
	if len(cfg.Connections) != 1 || cfg.Connections[0].ID != "a1" || strings.Contains(string(raw), "env:ACME") {
		t.Fatalf("connections %s", raw)
	}
	if got := checkConfig(nil, x, nil, "/cfg"); len(got.Scopes) != 0 {
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
