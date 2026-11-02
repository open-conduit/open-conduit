package integration_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/packages/pagerduty"
	"github.com/open-conduit/open-conduit/packages/sentry"
	shellext "github.com/open-conduit/open-conduit/packages/shell"
	pkgslack "github.com/open-conduit/open-conduit/packages/slack"
	"github.com/open-conduit/open-conduit/protocol"
)

// The reference pages are generated from the command definitions, the config
// schema with the adapter catalog, and the operation registry, so they can't
// drift: docs-check fails when a page differs from what its sources render.
// `make docs-gen` rewrites them.
var updateReference = flag.Bool("update", false, "rewrite the generated reference pages")

const referenceDir = "../site/src/content/docs/reference"

func TestDocsReferenceCurrent(t *testing.T) {
	pages := map[string]string{
		"cli.md":        cliReference(t),
		"config.md":     configReference(t),
		"operations.md": operationsReference(t),
	}
	for name, want := range pages {
		path := filepath.Join(referenceDir, name)
		if *updateReference {
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s is not what its sources generate; run make docs-gen", path)
		}
	}
}

// The generators are deterministic, so a page differs from its render only
// when its sources changed or it was edited by hand, and the CLI page covers
// the commands no other page documents.
func TestDocsReferenceDetectsDrift(t *testing.T) {
	a, b := cliReference(t), cliReference(t)
	if a != b {
		t.Fatal("the CLI reference is not deterministic")
	}
	for _, cmd := range []string{"migrate config", "work rebind", "work wake", "scopes reconcile", "container attach", "auth revoke", "executions cancel"} {
		if !strings.Contains(a, cmd) {
			t.Errorf("the CLI reference does not cover %s", cmd)
		}
	}
	if strings.Contains(a, "Usage of open-conduit") {
		t.Error("the CLI reference repeats the per-command help header")
	}
	if c := configReference(t); c != configReference(t) {
		t.Fatal("the config reference is not deterministic")
	}
	if o := operationsReference(t); o != operationsReference(t) {
		t.Fatal("the operations reference is not deterministic")
	}
}

// The schema's settings defaults are the ones the code applies.
func TestDocsSchemaDefaultsMatchConfig(t *testing.T) {
	schema := loadSchema(t)
	doc, err := config.Parse([]byte(`{"version": 1, "settings": {}, "connections": [], "scopes": [{"id": "a", "name": "A",
		"authorization": {"connections": [], "operations": [], "read_scopes": []}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	settings := reflect.ValueOf(doc.Settings)
	checked := 0
	for key, prop := range schema.defs("settings").props() {
		def, ok := prop["default"]
		if !ok {
			continue
		}
		field := settings.FieldByName(camel(key))
		if !field.IsValid() {
			t.Errorf("settings.%s has a default but no field in config.Settings", key)
			continue
		}
		want, _ := json.Marshal(def)
		got, _ := json.Marshal(field.Interface())
		if string(got) != string(want) {
			t.Errorf("settings.%s: the schema's default is %s, the code's %s", key, want, got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no settings defaults checked")
	}
}

// Every settings field says what it does and when it takes effect, so a new
// setting can't land on the reference page with an empty Meaning.
func TestDocsSchemaSettingsDescribed(t *testing.T) {
	if missing := undescribed(loadSchema(t).defs("settings")); len(missing) > 0 {
		t.Errorf("settings fields without a description in config.schema.json: %v", missing)
	}
	// Negative control: a field without one is reported.
	if got := undescribed(schemaNode{"properties": map[string]any{"x": map[string]any{"type": "integer"}}}); len(got) != 1 {
		t.Errorf("undescribed missed a field with no description: %v", got)
	}
}

func undescribed(s schemaNode) []string {
	var out []string
	for k, p := range s.props() {
		if d, _ := p["description"].(string); strings.TrimSpace(d) == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func camel(key string) string {
	var b strings.Builder
	for _, part := range strings.Split(key, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

const generatedNote = "This page is generated from %s by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.\n"

func frontmatter(title, description string) string {
	return fmt.Sprintf("---\ntitle: %s\ndescription: %q\n---\n\n", title, description)
}

// run is what the CLI prints for args.
func run(t *testing.T, args ...string) string {
	t.Helper()
	env := cli.Env{Getenv: func(string) string { return "" }, Cwd: t.TempDir(), Setups: bootstrap.Setups(), Adapters: bootstrap.Adapters}
	var out, errOut bytes.Buffer
	cli.RunOperator("test", "docs", args, env, &out, &errOut)
	return strings.TrimRight(out.String()+errOut.String(), "\n")
}

func cliReference(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(frontmatter("Command line", "Every open-conduit command and flag, as the CLI's own help prints them."))
	fmt.Fprintf(&b, generatedNote, "the CLI's help (`open-conduit --help` and `open-conduit COMMAND --help`)")
	usage, footer := "", ""
	full := run(t, "--help")
	for _, line := range strings.Split(full, "\n") {
		if strings.HasPrefix(line, "Usage:") {
			usage = line
		}
	}
	if i := strings.Index(full, "\nLists print"); i >= 0 {
		footer = strings.TrimSpace(full[i:])
	}
	b.WriteString("\nEvery command takes the global flags before it, and `--help` after it:\n\n```text\n" + usage + "\n```\n\n")
	b.WriteString("```text\n" + footer + "\n```\n")
	for _, g := range cli.HelpGroups() {
		fmt.Fprintf(&b, "\n## %s\n", g.Title)
		for _, cmd := range g.Commands {
			text := strings.TrimSuffix(run(t, cmd, "--help"), footer)
			if first, rest, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(first, "Usage of open-conduit ") {
				// The shared help's entries, indented for the global help.
				lines := strings.Split(strings.Trim(rest, "\n"), "\n")
				for i, l := range lines {
					lines[i] = strings.TrimPrefix(l, "  ")
				}
				text = strings.Join(lines, "\n")
			}
			fmt.Fprintf(&b, "\n### `open-conduit %s`\n\n```text\n%s\n```\n", cmd, strings.Trim(text, "\n"))
		}
	}
	return b.String()
}

// schemaNode is a decoded JSON Schema object.
type schemaNode map[string]any

func loadSchema(t *testing.T) schemaNode {
	t.Helper()
	data, err := os.ReadFile("../docs/architecture/config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s schemaNode
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s schemaNode) defs(name string) schemaNode {
	defs, _ := s["$defs"].(map[string]any)
	d, _ := defs[name].(map[string]any)
	return d
}

func (s schemaNode) child(key string) schemaNode {
	c, _ := s[key].(map[string]any)
	return c
}

// variant is the schema itself, or for a oneOf of a tombstone and the entry
// (bindings, routes, extensions) the entry.
func (s schemaNode) variant() schemaNode {
	if one, ok := s["oneOf"].([]any); ok && len(one) > 0 {
		last, _ := one[len(one)-1].(map[string]any)
		return last
	}
	return s
}

func (s schemaNode) props() map[string]schemaNode {
	out := map[string]schemaNode{}
	p, _ := s.variant()["properties"].(map[string]any)
	for k, v := range p {
		m, _ := v.(map[string]any)
		out[k] = m
	}
	return out
}

func (s schemaNode) required() map[string]bool {
	out := map[string]bool{}
	r, _ := s.variant()["required"].([]any)
	for _, k := range r {
		out[fmt.Sprint(k)] = true
	}
	return out
}

func (s schemaNode) keys() []string {
	p, _ := s.variant()["properties"].(map[string]any)
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	return keys
}

// describe is a schema's type in a few words.
func (s schemaNode) describe(anchors map[string]string) string {
	if ref, ok := s["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		if a, ok := anchors[name]; ok {
			return "[" + name + "](#" + a + ")"
		}
		return name
	}
	if c, ok := s["const"]; ok {
		v, _ := json.Marshal(c)
		return "`" + string(v) + "`"
	}
	if e, ok := s["enum"].([]any); ok {
		var vs []string
		for _, v := range e {
			vs = append(vs, "`"+fmt.Sprint(v)+"`")
		}
		return "one of " + strings.Join(vs, ", ")
	}
	typ, _ := s["type"].(string)
	if typ == "array" {
		if items := s.child("items"); items != nil {
			return "array of " + items.describe(anchors)
		}
	}
	if typ == "" {
		return "any"
	}
	return typ
}

func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// fieldTable is a schema object's properties as a table, in key order.
func fieldTable(b *strings.Builder, s schemaNode, anchors map[string]string) {
	props, req := s.props(), s.required()
	keys := s.keys()
	sort.Strings(keys)
	b.WriteString("\n| Field | Type | Required | Default | Meaning |\n| --- | --- | --- | --- | --- |\n")
	for _, k := range keys {
		p := props[k]
		desc, _ := p["description"].(string)
		r, def := "", ""
		if req[k] {
			r = "yes"
		}
		if d, ok := p["default"]; ok {
			v, _ := json.Marshal(d)
			def = "`" + string(v) + "`"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", k, cell(p.describe(anchors)), r, def, cell(desc))
	}
}

func configReference(t *testing.T) string {
	t.Helper()
	s := loadSchema(t)
	anchors := map[string]string{"settings": "settings", "authorization": "authorization", "connection": "connections", "requires": "requires",
		"binding": "bindings", "route": "routes", "extension": "extensions", "scope": "scopes"}
	var b strings.Builder
	b.WriteString(frontmatter("Configuration", "Every field of the version 1 config file: its type, default and meaning, and the fields each integration takes."))
	fmt.Fprintf(&b, generatedNote, "[the configuration schema](https://github.com/open-conduit/open-conduit/blob/main/docs/architecture/config.schema.json) and the integrations' catalog (`open-conduit adapters --json`)")
	b.WriteString("\nThe config file is JSON, by default at `~/.config/open-conduit/config.json`. `open-conduit config check` validates it offline; [configuration and secrets](../../guides/configuration/) walks through writing one, and [decision B05](../../architecture/config-management/) records how inheritance, reload and secrets work.\n")
	b.WriteString("\n## Top level\n")
	fieldTable(&b, s, anchors)
	section := func(title, intro string, n schemaNode) {
		fmt.Fprintf(&b, "\n## %s\n", title)
		if d, _ := n["description"].(string); d != "" {
			b.WriteString("\n" + d + "\n")
		}
		if intro != "" {
			b.WriteString("\n" + intro + "\n")
		}
		fieldTable(&b, n, anchors)
	}
	section("settings", "", s.defs("settings"))
	fmt.Fprintf(&b, "\n### settings.webhooks\n\n%s\n", s.defs("settings").props()["webhooks"]["description"])
	fieldTable(&b, s.defs("settings").props()["webhooks"], anchors)
	section("connections", "Each entry of `connections`.", s.defs("connection"))
	section("scopes", "Each entry of `scopes`.", s.defs("scope"))
	fmt.Fprintf(&b, "\n### limits\n")
	fieldTable(&b, s.defs("scope").props()["limits"], anchors)
	section("authorization", "A scope's `authorization`.", s.defs("authorization"))
	section("bindings", "Each entry of a scope's `bindings`.", s.defs("binding"))
	section("requires", "A binding's `requires`.", s.defs("requires"))
	section("routes", "Each entry of a scope's `routes`.", s.defs("route"))
	section("extensions", "Each entry of a scope's `extensions`.", s.defs("extension"))
	includes := s.child("properties")
	inc, _ := includes["includes"].(map[string]any)
	section("includes", "Each entry of `includes`.", schemaNode(inc).child("items"))
	if d, _ := inc["description"].(string); d != "" {
		b.WriteString("\n" + d + "\n")
	}

	b.WriteString("\n## Integration fields\n\nThe `config` objects of connections, extension entries, routes and bindings belong to the integration that registers each type, which checks them. `open-conduit adapters TYPE` prints the same list.\n")
	catalog := bootstrap.Adapters()
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Type < catalog[j].Type })
	for _, a := range catalog {
		fmt.Fprintf(&b, "\n### %s (`%s`)\n\n%s Registers: %s.\n", a.Title, a.Type, a.Description, strings.Join(a.Kinds, ", "))
		if a.Setup != "" {
			fmt.Fprintf(&b, "Set up with `open-conduit init %s`.\n", a.Setup)
		}
		for _, sec := range []struct {
			title  string
			fields []cli.Field
		}{{"Connection `config`", a.Connection}, {"Connection `secret_refs`", a.Secrets}, {"Extension entry `config`", a.Entry}, {"Route `config`", a.Route}, {"Binding `config`", a.Binding}} {
			if len(sec.fields) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n%s:\n\n| Field | Default | Meaning |\n| --- | --- | --- |\n", sec.title)
			for _, f := range sec.fields {
				def := ""
				if f.Default != "" {
					def = "`" + f.Default + "`"
				}
				meaning := f.Help
				if f.Required {
					meaning = strings.TrimSpace("Required. " + meaning)
				}
				if len(f.Options) > 0 {
					meaning = strings.TrimSpace(meaning + " One of " + strings.Join(f.Options, ", ") + ".")
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", f.Name, cell(def), cell(meaning))
			}
		}
	}
	return b.String()
}

// packageManifests are the installable packages built in this repository.
func packageManifests() []protocol.Manifest {
	ms := []protocol.Manifest{pagerduty.Manifest(), sentry.Manifest(), shellext.Manifest(), pkgslack.Manifest()}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return ms
}

func operationsReference(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(frontmatter("Toolbelt operations", "Every operation an execution can call through the toolbelt, the extension that serves it, its input and the authority it needs."))
	fmt.Fprintf(&b, generatedNote, "the operation registry (the core's builtin operations, the bundled extensions' registrations and the installable packages' manifests)")
	b.WriteString(`
An execution reaches Open Conduit through its toolbelt: the ` + "`os`" + ` CLI, or the MCP server it serves a coding agent (` + "`os mcp`" + `). The builtin operations are tools of their own; every other operation is called through ` + "`call`" + ` with its ` + "`operation`" + ` name and ` + "`input`" + ` object.

## Authority

The host checks every call where it serves it, from the execution's token, and refuses what the token does not grant:

- The scope's ` + "`authorization.operations`" + ` lists the operations its executions may call. A child scope's list can only narrow its parent's.
- A binding's ` + "`operations`" + ` is the exact list its executions get, within the scope's. A Work whose route opts into one of the binding's ` + "`grants`" + ` also gets that grant's operations, still within the scope's.
- An operation a ` + "`human`" + ` extension entry lists in ` + "`gates`" + ` waits at the host for a person's approval before it runs, whoever calls it (see [configuration](../config/#human-decisions-human)).
- A call acts on the execution's own Work, and only while its lease is current. Waiting on another scope's Events needs that scope in ` + "`read_scopes`" + `; filing an issue in, or commenting on, another scope's tracker needs it in ` + "`file_scopes`" + `.

See [configuration](../config/#authorization) for the fields and [toolbelt authority](../../architecture/toolbelt-management/) for the design.
`)
	b.WriteString("\n## Builtin operations\n\nServed by the core for every execution whose grant lists them.\n")
	for _, op := range cli.BuiltinOperations() {
		fmt.Fprintf(&b, "\n### `%s`\n\n%s\n", op.Name, op.Description)
		inputTable(&b, op.InputSchema)
	}

	b.WriteString("\n## Bundled extensions\n\nThe extensions built into the daemon register these operations; `open-conduit adapters TYPE` lists them too. Their registrations name each operation but do not declare its input yet, so this section lists names only.\n")
	owner := map[string]string{}
	catalog := bootstrap.Adapters()
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Type < catalog[j].Type })
	for _, a := range catalog {
		if len(a.Operations) == 0 {
			continue
		}
		ops := append([]string(nil), a.Operations...)
		sort.Strings(ops)
		fmt.Fprintf(&b, "\n### %s (`%s`)\n\n%s\n\n", a.Title, a.Type, a.Description)
		for _, op := range ops {
			if prev, ok := owner[op]; ok {
				t.Errorf("operation %s is listed by both %s and %s", op, prev, a.Type)
			}
			owner[op] = a.Type
			fmt.Fprintf(&b, "- `%s`\n", op)
		}
	}
	for _, op := range bootstrap.OperationNames() {
		if owner[op] == "" {
			t.Errorf("operation %s is registered but no extension in the adapter catalog lists it", op)
		}
	}

	b.WriteString("\n## Installable packages\n\nPackages installed with `open-conduit extension install` declare their operations in their manifests, with the input each takes.\n")
	for _, m := range packageManifests() {
		if len(m.Provides.Operations) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", m.Name, m.Summary)
		ops := append([]protocol.OperationDecl(nil), m.Provides.Operations...)
		sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
		for _, op := range ops {
			fmt.Fprintf(&b, "\n#### `%s`\n", op.Name)
			if op.Kind == protocol.OperationWait {
				b.WriteString("\nParks the Work until what it names happens; end the turn after calling it.\n")
			}
			if len(op.Input) == 0 {
				b.WriteString("\nIts manifest declares no input schema.\n")
			} else {
				var in map[string]any
				if err := json.Unmarshal(op.Input, &in); err != nil {
					t.Errorf("%s: input schema: %v", op.Name, err)
				}
				inputTable(&b, in)
			}
		}
	}
	return b.String()
}

// inputTable is an operation's input fields, or a line saying it takes none.
func inputTable(b *strings.Builder, schema map[string]any) {
	// Round-trip through JSON so Go-built schemas ([]string lists) read
	// like decoded ones.
	data, _ := json.Marshal(schema)
	var s schemaNode
	_ = json.Unmarshal(data, &s)
	keys := s.keys()
	sort.Strings(keys)
	if len(keys) == 0 {
		b.WriteString("\nTakes no input.\n")
		return
	}
	props, req := s.props(), s.required()
	b.WriteString("\n| Input | Type | Required | Meaning |\n| --- | --- | --- | --- |\n")
	for _, k := range keys {
		p := props[k]
		desc, _ := p["description"].(string)
		r := ""
		if req[k] {
			r = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", k, cell(p.describe(nil)), r, cell(desc))
	}
}
