package protocol_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// Parity: a capability a bundled extension has must be one an installed
// package can have too, without changing Open Conduit (issue #239). Every
// surface of the in-process contract (extension/inproc) is carried by a
// wire method, a manifest field, or something a process has of its own;
// TestEveryMethodIsReachable checks the other direction.

// Carriers name what carries a surface: "method:<wire method>",
// "manifest:<json path in the manifest>" or "process:<why a process has it
// of its own>".
var parity = map[string][]string{
	// Interfaces an extension implements.
	"Extension":  {"manifest:name", "method:initialize"},
	"Typed":      {"manifest:provides.connections", "manifest:provides.extensions", "manifest:provides.routes.config", "method:validate"},
	"Operator":   {"manifest:provides.operations"},
	"Router":     {"manifest:provides.routes", "method:route.match"},
	"Starter":    {"method:initialize"},
	"Stopper":    {"method:shutdown"},
	"WorkKinder": {"manifest:provides.work_kinds", "method:work_kind.trigger"},
	"Recurring":  {"manifest:provides.recurrences", "method:recurrence.next"},
	"Configurer": {"method:configure"},
	"Tooled":     {"method:operation.call", "method:operation.wait", "method:operation.emit"},
	"Actor":      {"manifest:provides.actions", "method:work.actions", "method:work.act"},
	"Annotator":  {"manifest:provides.annotate", "method:work.annotate"},
	"Renderer":   {"manifest:provides.render", "method:event.render"},
	"Tasker":     {"manifest:provides.tasks", "method:task.run"},
	"Checker":    {"manifest:provides.checks", "method:check.run"},
	"Playbooker": {"manifest:provides.playbooks"},
	"Exporting":  {"manifest:provides.exporters", "method:telemetry.export"},
	"Sink":       {"manifest:permissions", "method:telemetry.sink"},
	"ConfigHost": {"manifest:permissions", "method:config.base", "method:config.edit", "method:config.validate", "method:config.trace"},
	"Timers":     {"method:timer.arm", "method:timer.cancel"},

	// Host services, inproc.Host's fields.
	"Host.Store":        {"method:kv.get", "method:kv.list", "method:kv.commit", "method:state.work", "method:state.works", "method:state.events"},
	"Host.Now":          {"process:its own clock; time-dependent calls carry the host's now"},
	"Host.Log":          {"process:its stderr, which the host keeps as the extension's log"},
	"Host.Secrets":      {"method:secret.resolve"},
	"Host.Report":       {"method:check.run"},
	"Host.Nudge":        {"method:event.accept", "method:timer.arm"},
	"Host.StateDir":     {"method:initialize"},
	"Host.DryRun":       {"method:configure"},
	"Host.AcceptInTx":   {"method:event.accept"},
	"Host.Timers":       {"method:timer.arm", "method:timer.cancel"},
	"Host.CancelWork":   {"method:work.cancel"},
	"Host.WaitProgress": {"method:wait.progress"},
	"Host.Capture":      {"manifest:permissions", "method:telemetry.capture"},
	"Host.Probes":       {"manifest:permissions", "method:probe.list", "method:probe.run"},
	"Host.Config":       {"manifest:permissions", "method:config.base"},

	// A config type's fields, inproc.ConfigType.
	"ConfigType.Kind":     {"method:validate"},
	"ConfigType.Type":     {"manifest:provides.connections.type", "manifest:provides.extensions.type", "manifest:provides.routes.handler"},
	"ConfigType.Ingress":  {"manifest:provides.extensions.ingress"},
	"ConfigType.Validate": {"method:validate"},
	"ConfigType.Settings": {"manifest:provides.extensions.settings"},
	"ConfigType.Describe": {"manifest:provides.extensions.describe"},
}

// parityGaps are the surfaces no wire method or manifest field carries yet,
// each with the open issue that closes it. Only those issues own entries.
var parityGaps = map[string]int{
	// The setup extension, which gates its operations on an entry and
	// checks its route's binding, becomes an installed package.
	"EntryGated":         243,
	"ConfigType.Binding": 243,
}

// inprocSurfaces lists the contract's interfaces, inproc.Host's fields and
// inproc.ConfigType's fields from its source, so a new surface fails here
// until it is carried or listed as a gap.
func inprocSurfaces(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "extension", "inproc", "inproc.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.TYPE {
			continue
		}
		for _, spec := range g.Specs {
			ts := spec.(*ast.TypeSpec)
			switch typ := ts.Type.(type) {
			case *ast.InterfaceType:
				out = append(out, ts.Name.Name)
			case *ast.StructType:
				if ts.Name.Name != "Host" && ts.Name.Name != "ConfigType" {
					continue
				}
				for _, field := range typ.Fields.List {
					for _, n := range field.Names {
						out = append(out, ts.Name.Name+"."+n.Name)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestInprocSurfacesAreCarried(t *testing.T) {
	surfaces := inprocSurfaces(t)
	if len(surfaces) < 30 {
		t.Fatalf("read only %d surfaces from the inproc contract: %v", len(surfaces), surfaces)
	}
	known := map[string]bool{}
	for _, s := range surfaces {
		known[s] = true
		carriers, carried := parity[s]
		issue, gap := parityGaps[s]
		switch {
		case carried && gap:
			t.Errorf("%s is carried and also listed as a gap of #%d: drop the gap", s, issue)
		case !carried && !gap:
			t.Errorf("%s is carried by no wire method or manifest field: carry it, or list it in parityGaps with the issue that will", s)
		}
		for _, c := range carriers {
			checkCarrier(t, s, c)
		}
	}
	for s := range parity {
		if !known[s] {
			t.Errorf("parity lists %s, which the inproc contract no longer has", s)
		}
	}
	// Gaps belong to the open follow-ups of #235 that close them.
	owners := map[int]bool{240: true, 241: true, 242: true, 243: true}
	for s, issue := range parityGaps {
		if !known[s] {
			t.Errorf("parityGaps lists %s, which the inproc contract no longer has", s)
		}
		if !owners[issue] {
			t.Errorf("gap %s names #%d, which is not one of the follow-up issues", s, issue)
		}
	}
}

func checkCarrier(t *testing.T, surface, carrier string) {
	t.Helper()
	kind, what, _ := strings.Cut(carrier, ":")
	switch kind {
	case "method":
		if _, ok := protocol.Lookup(what); !ok {
			t.Errorf("%s: no wire method %s", surface, what)
		}
	case "manifest":
		if !manifestPath(reflect.TypeFor[protocol.Manifest](), strings.Split(what, ".")) {
			t.Errorf("%s: the manifest has no field %s", surface, what)
		}
	case "process":
		if what == "" {
			t.Errorf("%s: say what the process has of its own", surface)
		}
	default:
		t.Errorf("%s: carrier %q is not method:, manifest: or process:", surface, carrier)
	}
}

// manifestPath reports whether a JSON path of field names exists in typ,
// through slices and pointers.
func manifestPath(typ reflect.Type, path []string) bool {
	for typ.Kind() == reflect.Slice || typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if len(path) == 0 {
		return true
	}
	if typ.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name == path[0] {
			return manifestPath(f.Type, path[1:])
		}
	}
	return false
}
