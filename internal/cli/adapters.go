package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Field is one documented config field of a registered integration.
type Field struct {
	Name     string   `json:"name"`
	Help     string   `json:"help,omitempty"`
	Default  string   `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Adapter describes one registered integration (v1 `adapters`): what it
// registers (connection, extension entry, route handler, executor,
// resource provider, serve option) and the fields each takes. The
// entrypoint supplies the catalog (cli.Env.Adapters) from bootstrap, so this
// package imports no extension; the full-screen config editor builds its
// forms from the same catalog.
type Adapter struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Kinds       []string `json:"kinds"`
	Connection  []Field  `json:"connection,omitempty"`
	Secrets     []Field  `json:"secrets,omitempty"`
	Entry       []Field  `json:"entry,omitempty"`
	Route       []Field  `json:"route,omitempty"`
	Binding     []Field  `json:"binding,omitempty"`
	Operations  []string `json:"operations,omitempty"`
	// Setup is the `init` setup that adds it, when there is one.
	Setup string `json:"setup,omitempty"`
}

func (o *operator) adapters(args []string) int {
	asJSON := false
	var rest []string
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(a, "-") {
				return usageError(o.errOut, "adapters takes [TYPE] [--json]")
			}
			rest = append(rest, a)
		}
	}
	if len(rest) > 1 {
		return usageError(o.errOut, "adapters takes [TYPE] [--json]")
	}
	if o.env.Adapters == nil {
		fmt.Fprintln(o.errOut, "open-conduit: no adapter catalog in this binary")
		return 1
	}
	catalog := o.env.Adapters()
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Type < catalog[j].Type })
	if len(rest) == 1 {
		for _, a := range catalog {
			if a.Type == rest[0] {
				if asJSON {
					return o.emit(a, nil)
				}
				writeAdapter(o.out, a)
				return 0
			}
		}
		fmt.Fprintf(o.errOut, "open-conduit: no adapter %q; run open-conduit adapters\n", rest[0])
		return 1
	}
	if asJSON {
		return o.emit(catalog, nil)
	}
	for _, a := range catalog {
		fmt.Fprintf(o.out, "%-14s %-40s %s\n", a.Type, strings.Join(a.Kinds, ","), a.Title)
	}
	fmt.Fprintln(o.out, "\nopen-conduit adapters TYPE shows its fields.")
	return 0
}

func writeAdapter(w io.Writer, a Adapter) {
	fmt.Fprintf(w, "%s — %s\n%s\nregisters: %s\n", a.Type, a.Title, a.Description, strings.Join(a.Kinds, ", "))
	if a.Setup != "" {
		fmt.Fprintf(w, "setup: open-conduit init %s\n", a.Setup)
	}
	section := func(title string, fields []Field) {
		if len(fields) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s:\n", title)
		for _, f := range fields {
			line := "  " + f.Name
			var notes []string
			if f.Required {
				notes = append(notes, "required")
			}
			if f.Default != "" {
				notes = append(notes, "default "+f.Default)
			}
			if len(f.Options) > 0 {
				notes = append(notes, "one of "+strings.Join(f.Options, ", "))
			}
			if len(notes) > 0 {
				line += " (" + strings.Join(notes, "; ") + ")"
			}
			if f.Help != "" {
				line += ": " + f.Help
			}
			fmt.Fprintln(w, line)
		}
	}
	section("connection config", a.Connection)
	section("connection secret_refs", a.Secrets)
	section("extension entry config", a.Entry)
	section("route config", a.Route)
	section("binding config", a.Binding)
	if len(a.Operations) > 0 {
		fmt.Fprintf(w, "\ntool operations: %s\n", strings.Join(a.Operations, ", "))
	}
}
