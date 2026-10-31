package protocol_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// firstParty are the shipped extensions and executors the 07-01 inventory
// read; every wire method must name at least one of them.
var firstParty = map[string]bool{
	"github": true, "slack": true, "linear": true, "jira": true, "bitbucket": true, "schedule": true,
	"introspection": true, "human": true, "posthog": true, "git": true, "shell": true, "claude-code": true, "legacy-job": true,
}

func TestCatalogueMapsEveryMethodToAShippedUse(t *testing.T) {
	names := map[string]bool{}
	for _, m := range protocol.Methods {
		if names[m.Name] {
			t.Errorf("duplicate method %s", m.Name)
		}
		names[m.Name] = true
		if len(m.UseCases) == 0 || len(m.Contract) == 0 {
			t.Errorf("%s: needs a shipped use case and the contract it carries", m.Name)
		}
		for _, u := range m.UseCases {
			if !firstParty[u] {
				t.Errorf("%s: unknown use case %q", m.Name, u)
			}
		}
		switch m.Direction {
		case protocol.HostToExtension, protocol.ExtensionToHost:
		case protocol.Both:
			if !m.Notification {
				t.Errorf("%s: only notifications go both ways", m.Name)
			}
		default:
			t.Errorf("%s: direction %q", m.Name, m.Direction)
		}
		// In-transaction calls are host-initiated, answered and bounded.
		if m.InTransaction && (m.Direction != protocol.HostToExtension || m.Notification) {
			t.Errorf("%s: an in-transaction method must be a host-to-extension request", m.Name)
		}
	}
	if _, ok := protocol.Lookup("kv.commit"); !ok {
		t.Fatal("lookup")
	}
}

// TestCatalogueCoversThePublicContract checks mechanically that every method
// of the public in-process contracts an extension implements or consumes is
// carried by some wire method, so the subprocess boundary loses no shipped
// interaction.
func TestCatalogueCoversThePublicContract(t *testing.T) {
	carried := map[string]bool{}
	for _, m := range protocol.Methods {
		for _, c := range m.Contract {
			carried[strings.Fields(c)[0]] = true
		}
	}
	interfaces := map[string]reflect.Type{
		"extension.Ingress":           reflect.TypeFor[extension.Ingress](),
		"extension.RouteHandler":      reflect.TypeFor[extension.RouteHandler](),
		"extension.RouteSampler":      reflect.TypeFor[extension.RouteSampler](),
		"extension.Executor":          reflect.TypeFor[extension.Executor](),
		"extension.RunHandle":         reflect.TypeFor[extension.RunHandle](),
		"extension.InterruptExecutor": reflect.TypeFor[extension.InterruptExecutor](),
		"extension.InjectExecutor":    reflect.TypeFor[extension.InjectExecutor](),
		"extension.ResumeExecutor":    reflect.TypeFor[extension.ResumeExecutor](),
		"extension.ForgetExecutor":    reflect.TypeFor[extension.ForgetExecutor](),
		"extension.StopExecutor":      reflect.TypeFor[extension.StopExecutor](),
		"extension.AttachExecutor":    reflect.TypeFor[extension.AttachExecutor](),
		"extension.ResourceProvider":  reflect.TypeFor[extension.ResourceProvider](),
		"extension.ResourceBroker":    reflect.TypeFor[extension.ResourceBroker](),
		"extension.Setup":             reflect.TypeFor[extension.Setup](),
		"extension.SetupHost":         reflect.TypeFor[extension.SetupHost](),
		"observability.Exporter":      reflect.TypeFor[observability.Exporter](),
	}
	for name, typ := range interfaces {
		for i := 0; i < typ.NumMethod(); i++ {
			if key := name + "." + typ.Method(i).Name; !carried[key] {
				t.Errorf("%s is not carried by any wire method", key)
			}
		}
	}
	// Storage: the namespaced KV and the read views extensions use. Writes
	// to core records (AcceptEvent, PutWork, leases, timers) are never
	// exposed; they go through event.accept, timer.arm and work.cancel.
	for _, m := range []string{"Get", "Put", "Delete", "List", "Work", "ListWorks", "ListExecutions", "Event", "Events", "ListScopes", "Scope", "ListTimers"} {
		if _, ok := reflect.TypeFor[storage.Tx]().MethodByName(m); !ok || !carried["storage.Tx."+m] {
			t.Errorf("storage.Tx.%s is not carried", m)
		}
	}
	for _, forbidden := range []string{"AcceptEvent", "PutWork", "Claim", "Renew", "Release", "PutTimer", "FireTimer", "PutWait", "PutScope", "PutExecution", "MarkRouted"} {
		if carried["storage.Tx."+forbidden] {
			t.Errorf("storage.Tx.%s must not cross the boundary", forbidden)
		}
	}
	for _, f := range []string{"Handler", "Wait", "Emit"} {
		if !carried["extension.Operation."+f] {
			t.Errorf("extension.Operation.%s is not carried", f)
		}
	}
	for _, f := range []string{"PersistRecovery"} {
		if !carried["extension.RunRequest."+f] {
			t.Errorf("extension.RunRequest.%s is not carried", f)
		}
	}
	if !carried["extension.Registration.DependencyFilter"] || !carried["extension.EmitPlan.Commit"] {
		t.Error("pure registration callbacks are not carried")
	}
}

func load(t *testing.T, name string) protocol.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	m, err := protocol.ParseManifest(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// TestExampleManifests covers a workspace-free ingress workload and an
// execution workload (plus timers and committed emissions) with one manifest
// format and one method catalogue: neither needs a special-case path.
func TestExampleManifests(t *testing.T) {
	ingress := load(t, "ingress-support.json").Enabled()
	for _, want := range []string{"initialize", "configure", "ingress.push", "ingress.reconcile", "ingress.deliver", "webhook.handle", "route.match", "operation.call", "task.run", "kv.commit", "state.works", "setup.prompt", "event.render", "check.run"} {
		if !slices.Contains(ingress, want) {
			t.Errorf("ingress example lacks %s", want)
		}
	}
	for _, absent := range []string{"executor.run", "resource.acquire", "provider.acquire", "timer.arm", "emit.commit", "telemetry.export"} {
		if slices.Contains(ingress, absent) {
			t.Errorf("ingress example enables %s", absent)
		}
	}
	exec := load(t, "executor-workspace.json").Enabled()
	for _, want := range []string{"executor.run", "executor.wait", "executor.recover", "executor.resume", "executor.interrupt", "executor.inject", "executor.stop", "executor.forget", "executor.attach", "execution.persist_recovery", "resource.acquire", "provider.reconcile", "provider.owner_status", "artifact.write", "kv.get"} {
		if !slices.Contains(exec, want) {
			t.Errorf("executor example lacks %s", want)
		}
	}
	for _, absent := range []string{"ingress.push", "route.match", "operation.call", "setup.run"} {
		if slices.Contains(exec, absent) {
			t.Errorf("executor example enables %s", absent)
		}
	}
	sched := load(t, "scheduled-reporting.json").Enabled()
	for _, want := range []string{"recurrence.next", "timer.arm", "work_kind.trigger", "operation.emit", "emit.commit", "event.accept", "work.cancel", "telemetry.export"} {
		if !slices.Contains(sched, want) {
			t.Errorf("scheduled example lacks %s", want)
		}
	}
}

// TestEveryMethodIsReachable checks that a manifest declaring every surface
// enables the whole catalogue, so no Method.Provides key is misspelled.
func TestEveryMethodIsReachable(t *testing.T) {
	m := load(t, "ingress-support.json")
	e := load(t, "executor-workspace.json").Provides
	m.Provides.Executors, m.Provides.Resources = e.Executors, e.Resources
	m.Provides.Recurrences, m.Provides.Exporters, m.Provides.Annotate, m.Provides.Actions, m.Provides.Panels = []string{"chat.calendar"}, true, true, true, true
	m.Provides.Operations = append(m.Provides.Operations, protocol.OperationDecl{Name: "chat.wait", Kind: protocol.OperationWait}, protocol.OperationDecl{Name: "chat.note", Kind: protocol.OperationEmit, Commit: true})
	m.Provides.DependencyFilter, m.Provides.RunEnvironment = true, true
	m.Provides.Routes[0].Samples = true
	m.Provides.WorkKinds = append(m.Provides.WorkKinds, protocol.WorkKindDecl{Kind: "chat.job", Standing: true, Trigger: true})
	m.Provides.Executors[0].Name, m.Provides.Resources[0].Type = "chat-runner", "chat-ws"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	enabled := m.Enabled()
	for _, method := range protocol.Methods {
		if !slices.Contains(enabled, method.Name) {
			t.Errorf("%s is unreachable (provides %q)", method.Name, method.Provides)
		}
	}
}

func TestManifestRejections(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("testdata", "ingress-support.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"unknown toolbelt":     {`"setup":`, `"executors": [{"name": "chat-agent", "capabilities": {"toolbelt": "http"}}], "setup":`},
		"unsupported protocol": {`"protocol": [1]`, `"protocol": [2]`},
		"reserved name":        {`"name": "chat"`, `"name": "core"`},
		"foreign family":       {`"chat.message.edited"`, `"other.message.edited"`},
		"family without loss":  {`"re_derivable": false, "loss": "Deletions arrive only by push; a deletion missed while push is down is never observed."`, `"re_derivable": false`},
		"foreign operation":    {`"chat.reply"`, `"slack.reply"`},
		"operation kind":       {`{"name": "chat.reply", "kind": "handler"}`, `{"name": "chat.reply", "kind": "effect"}`},
		"duplicate operation":  {`{"name": "chat.reply", "kind": "handler"}`, `{"name": "chat.thread", "kind": "handler"}`},
		"foreign executor":     {`"setup":`, `"executors": [{"name": "shell", "capabilities": {"toolbelt": "cli"}}], "setup":`},
		"command escape":       {`"./chat-extension"`, `"../chat-extension"`},
		"playbooks escape":     {`"playbooks": "playbooks"`, `"playbooks": "/etc"`},
		"task interval":        {`"interval_seconds": 5`, `"interval_seconds": 0`},
		"resource mode":        {`"setup":`, `"resources": [{"type": "chat-ws", "modes": ["pooled"]}], "setup":`},
		"external store":       {`"setup":`, `"external_stores": [{"connection": "chat-index", "description": "search index"}], "setup":`},
		"commit on handler":    {`{"name": "chat.reply", "kind": "handler"}`, `{"name": "chat.reply", "kind": "handler", "commit": true}`},
		"guidance too long":    {`{"name": "chat.reply", "kind": "handler"}`, `{"name": "chat.reply", "kind": "handler", "guidance": "` + strings.Repeat("x", extension.MaxGuidance+1) + `"}`},
		"filter without wait":  {`"webhooks": true,`, `"webhooks": true, "dependency_filter": true,`},
		"kinds without routes": {`"routes": [{"handler": "chat", "config": true}],`, ``},
	}
	for name, c := range cases {
		mutated := strings.Replace(string(base), c[0], c[1], 1)
		if mutated == string(base) {
			t.Fatalf("%s: mutation did not apply", name)
		}
		if _, err := protocol.ParseManifest([]byte(mutated)); !errors.Is(err, protocol.ErrManifest) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// A field this module does not know is refused as needing a newer Open
// Conduit, not as a broken package: additive surfaces arrive in a release
// without a protocol version bump. That includes any attempt to negotiate
// storage (KV is mandatory and implicit) or to claim semantic suitability
// (executors declare only mechanical labels).
func TestManifestUnknownFieldNeedsNewerHost(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("testdata", "ingress-support.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"provides field":     {`"webhooks": true,`, `"webhooks": true, "gadgets": [{"name": "chat-gadget"}],`},
		"top-level field":    {`"name": "chat"`, `"name": "chat", "requires": "2.0"`},
		"storage capability": {`"webhooks": true,`, `"webhooks": true, "storage": {"kv": true},`},
		"semantic label":     {`"setup":`, `"executors": [{"name": "chat-agent", "capabilities": {"toolbelt": "mcp", "coding": true}}], "setup":`},
	} {
		mutated := strings.Replace(string(base), c[0], c[1], 1)
		if mutated == string(base) {
			t.Fatalf("%s: mutation did not apply", name)
		}
		_, err := protocol.ParseManifest([]byte(mutated))
		if !errors.Is(err, protocol.ErrNewerManifest) || errors.Is(err, protocol.ErrManifest) {
			t.Errorf("%s: %v", name, err)
		} else if !strings.Contains(err.Error(), "newer Open Conduit") {
			t.Errorf("%s: %q does not say a newer Open Conduit is needed", name, err)
		}
	}
}

func TestNamespacesAndNegotiation(t *testing.T) {
	for ns, want := range map[string]bool{"chat": true, "chat.tools": true, "chat.": false, "chatter": false, "other.chat": false} {
		if protocol.Namespace("chat", ns) != want {
			t.Errorf("KV namespace %q", ns)
		}
	}
	for ns, want := range map[string]bool{"chat": true, "chat/ops": true, "chat/": false, "chatter/x": false} {
		if protocol.SourceNamespace("chat", ns) != want {
			t.Errorf("source namespace %q", ns)
		}
	}
	if v, err := protocol.Negotiate([]int{1, 2}, protocol.Versions); err != nil || v != 1 {
		t.Fatalf("negotiate %d %v", v, err)
	}
	if _, err := protocol.Negotiate([]int{2}, protocol.Versions); !errors.Is(err, protocol.ErrIncompatible) {
		t.Fatalf("mismatch %v", err)
	}
}
