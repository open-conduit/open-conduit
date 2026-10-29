package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	for _, x := range firstParty() {
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

// The turn plugin carries the setup playbook, so a session's binding
// (`playbook: setup`) starts on a skill Claude Code has, beside the
// work-issue playbook and the extensions' own.
func TestTurnPluginCarriesSetupPlaybook(t *testing.T) {
	d := &daemon{o: Options{StateDir: t.TempDir()}, inproc: firstParty()}
	if err := d.preparePlaybooks(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"setup", "work-issue", "sweep"} {
		data, err := os.ReadFile(filepath.Join(d.pluginDir(), "skills", name, "SKILL.md"))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s skill: %v", name, err)
		}
		if strings.Contains(string(data), "{{bin}}") {
			t.Errorf("%s skill is not filled in", name)
		}
	}
}
