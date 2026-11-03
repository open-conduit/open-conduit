package bootstrap

import (
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
	"slices"
	"sort"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// Adapters is the catalog of the config types this daemon offers (v1
// `adapters [type]`, 06-01): each package's, installed beside configPath or
// bundled with the binary, as its manifest describes them (title, summary,
// types, fields, operations and setup), and the core's own webhook
// listener. It is descriptive only; validation stays with each type's
// validate. The full-screen config editor builds its forms from it. The
// errors are the packages discovery refused, which are missing from it.
func Adapters(configPath string) ([]cli.Adapter, []error) {
	found, errs := discoverPackages(configPath, "")
	var out []cli.Adapter
	for _, f := range found {
		out = append(out, manifestAdapters(f.Manifest)...)
	}
	out = append(out, tunnelAdapter())
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, errs
}

// manifestAdapters are a package's catalog entries: one under its name with
// every type it declares, as "<kind>" for the type of its own name and
// "<kind>:<type>" for another, and the fields of its own-named types. A
// type of another name that describes fields gets an entry of its own, so
// its fields are found by its name. A package that declares no config type
// (only operations) has none.
func manifestAdapters(m protocol.Manifest) []cli.Adapter {
	title := m.Title
	if title == "" {
		title = m.Name
	}
	own := cli.Adapter{Type: m.Name, Title: title, Description: m.Summary}
	var others []cli.Adapter
	other := map[string]int{}
	add := func(kind, typ string, fields []protocol.FieldDecl, set func(*cli.Adapter, []cli.Field)) {
		a := &own
		label := kind
		if typ != m.Name {
			label = kind + ":" + typ
			if len(fields) > 0 {
				i, ok := other[typ]
				if !ok {
					i = len(others)
					other[typ] = i
					others = append(others, cli.Adapter{Type: typ, Title: title + " (" + typ + ")", Description: m.Summary})
				}
				a, label = &others[i], kind
			}
		}
		if !slices.Contains(a.Kinds, label) {
			a.Kinds = append(a.Kinds, label)
		}
		set(a, catalogFields(fields))
	}
	p := m.Provides
	for _, c := range p.Connections {
		var conf, secrets []protocol.FieldDecl
		for _, f := range c.Fields {
			if f.Secret {
				secrets = append(secrets, f)
			} else {
				conf = append(conf, f)
			}
		}
		add("connection", c.Type, c.Fields, func(a *cli.Adapter, _ []cli.Field) {
			a.Connection, a.Secrets = catalogFields(conf), catalogFields(secrets)
		})
	}
	for _, x := range p.Extensions {
		add("extension", x.Type, protocol.WithSettingDefaults(x.Fields, x.Settings), func(a *cli.Adapter, f []cli.Field) { a.Entry = f })
	}
	for _, r := range p.Routes {
		add("route", r.Handler, r.Fields, func(a *cli.Adapter, f []cli.Field) {
			if len(f) > 0 || a.Route == nil {
				a.Route = f
			}
		})
	}
	for _, x := range p.Executors {
		add("executor", x.Name, x.Fields, func(a *cli.Adapter, f []cli.Field) {
			if len(f) > 0 || a.Binding == nil {
				a.Binding = f
			}
		})
	}
	for _, r := range p.Resources {
		add("resource", r.Type, protocol.WithSettingDefaults(r.Fields, r.Settings), func(a *cli.Adapter, f []cli.Field) {
			if len(f) > 0 || a.Resource == nil {
				a.Resource = f
			}
		})
	}
	for _, sb := range p.Sandboxes {
		add("sandbox", sb.Type, nil, func(*cli.Adapter, []cli.Field) {})
	}
	if len(own.Kinds) == 0 && len(others) == 0 {
		return nil
	}
	for _, op := range p.Operations {
		own.Operations = append(own.Operations, op.Name)
	}
	sort.Strings(own.Operations)
	if p.Setup != nil {
		own.Setup = p.Setup.Type
	}
	return append([]cli.Adapter{own}, others...)
}

// catalogFields are declared fields as the catalog shows them.
func catalogFields(decls []protocol.FieldDecl) []cli.Field {
	if len(decls) == 0 {
		return nil
	}
	out := make([]cli.Field, 0, len(decls))
	for _, f := range decls {
		out = append(out, cli.Field{Name: f.Name, Help: f.Help, Default: f.Default, Required: f.Required, Options: slices.Clone(f.Options)})
	}
	return out
}

// tunnelAdapter is the core's own webhook listener (settings.webhooks),
// which no package provides.
func tunnelAdapter() cli.Adapter {
	f := func(name, help string) cli.Field { return cli.Field{Name: name, Help: help} }
	return cli.Adapter{Type: "tunnel", Title: "Webhook tunnels", Description: "Exposes the webhook listener: ngrok, cloudflared or a static public URL (settings.webhooks; serve flags of the same names override it for one run).",
		Kinds: []string{"serve"},
		Binding: []cli.Field{{Name: "addr", Help: "the listener, for example 127.0.0.1:8787", Required: true}, {Name: "tunnel", Options: []string{"ngrok", "cloudflared", "static"}}, f("tunnel_domain", "ngrok reserved domain"),
			f("tunnel_name", "cloudflared named tunnel"), f("tunnel_hostname", "its hostname"), f("public_url", "static")}}
}

// OperationDeclarations are the first-party extensions' operation
// declarations by name: each one's summary and input, as the generated
// operations reference renders them. They come from the in-process
// extensions (inproc.Declarer) and the bundled packages' manifests.
func OperationDeclarations() map[string]extension.Operation {
	out := map[string]extension.Operation{}
	found, _ := discoverPackages("", "")
	for _, f := range found {
		for _, op := range f.Manifest.Provides.Operations {
			out[op.Name] = extension.Operation{Name: op.Name, Summary: op.Summary, Input: op.Input, Guidance: op.Guidance}
		}
	}
	for _, x := range firstParty() {
		if d, ok := x.(inproc.Declarer); ok {
			for _, op := range d.Declarations() {
				out[op.Name] = op
			}
		}
	}
	return out
}

// BundledManifests are the manifests of the packages bundled with this
// binary, by name: for a generated reference to list what they serve.
func BundledManifests() []protocol.Manifest {
	found, _ := discoverPackages("", "")
	out := make([]protocol.Manifest, 0, len(found))
	for _, f := range found {
		out = append(out, f.Manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bundledRegistry is the registry offline config check uses, with the
// bundled packages' declarations (#428); it starts no package.
func bundledRegistry() config.Registry {
	reg := registry(offlineExecutors())
	offlineExternal(&reg, "", "", nil).close()
	return reg
}

// Toolsets are the toolsets the bundled extensions publish, which config
// grants as "@<name>", sorted by name.
func Toolsets() []extension.Toolset {
	reg := bundledRegistry()
	out := make([]extension.Toolset, 0, len(reg.Toolsets))
	for _, t := range reg.Toolsets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OperationNames are the toolbelt operations a config may grant offline
// (the registry config check validates authorization.operations against),
// sorted. Each extension's are in its Adapters entry; the generated
// operations reference page lists them by extension.
func OperationNames() []string {
	names := append([]string(nil), bundledRegistry().Operations...)
	sort.Strings(names)
	return slices.Compact(names)
}
