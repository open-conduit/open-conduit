package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// ConfigDoc is an order-preserving config document for the full-screen
// config editor (06-01): the same JSON pointer edits as `config set/unset`,
// with secret_refs values redacted in views and checked as references.
type ConfigDoc struct{ root *node }

// ParseConfigDoc reads a config document (a JSON object).
func ParseConfigDoc(data []byte) (*ConfigDoc, error) {
	n, err := parseNode(data)
	if err != nil {
		return nil, err
	}
	if n.kind != 'o' {
		return nil, errors.New("config is not a JSON object")
	}
	return &ConfigDoc{root: n}, nil
}

// Bytes is the document as written (indented, keys in their order).
func (d *ConfigDoc) Bytes() []byte { return d.root.bytes() }

// DocEntry is one node of the document, flattened in display order.
type DocEntry struct {
	Pointer string
	Depth   int
	// Label is the key, or the element's id/name (else its index).
	Label string
	// Kind is "object", "array" or "value".
	Kind string
	// Value is the scalar's JSON, or a short summary of a container.
	Value string
	// Secret marks a secret_refs value (shown redacted).
	Secret bool
}

func escapeSeg(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// Entries flattens the document in display order.
func (d *ConfigDoc) Entries() []DocEntry {
	var out []DocEntry
	var walk func(n *node, ptr, label string, depth int, secret bool)
	walk = func(n *node, ptr, label string, depth int, secret bool) {
		e := DocEntry{Pointer: ptr, Depth: depth, Label: label}
		switch n.kind {
		case 'o':
			e.Kind, e.Value = "object", fmt.Sprintf("{%d}", len(n.obj))
		case 'a':
			e.Kind, e.Value = "array", fmt.Sprintf("[%d]", len(n.arr))
		default:
			e.Kind, e.Value, e.Secret = "value", string(n.scalar), secret
			if secret {
				e.Value = `"[redacted]"`
			}
		}
		if ptr != "" {
			out = append(out, e)
		}
		switch n.kind {
		case 'o':
			for _, m := range n.obj {
				walk(m.value, ptr+"/"+escapeSeg(m.key), m.key, depth+1, label == "secret_refs" && ptr != "")
			}
		case 'a':
			for i, el := range n.arr {
				name := strconv.Itoa(i)
				if el.kind == 'o' {
					for _, k := range []string{"id", "name"} {
						if f := el.field(k); f != nil && f.kind == 's' {
							if s, err := strconv.Unquote(string(f.scalar)); err == nil {
								name = s
								break
							}
						}
					}
				}
				walk(el, ptr+"/"+strconv.Itoa(i), name, depth+1, false)
			}
		}
	}
	walk(d.root, "", "", -1, false)
	return out
}

// Get returns the JSON at pointer (secret_refs values redacted).
func (d *ConfigDoc) Get(ptr string) (string, error) {
	parts, err := pointer(ptr)
	if err != nil {
		return "", err
	}
	n, err := d.root.lookup(parts)
	if err != nil {
		return "", err
	}
	if len(parts) >= 2 && parts[len(parts)-2] == "secret_refs" {
		return `"[redacted]"`, nil
	}
	return strings.TrimSpace(string(n.bytes())), nil
}

// Set replaces or adds the JSON value at pointer ("-" appends to an array).
// A secret_refs value must be a reference, never a secret.
func (d *ConfigDoc) Set(ptr, value string) error {
	parts, err := pointer(ptr)
	if err != nil {
		return err
	}
	v, err := parseNode([]byte(value))
	if err != nil {
		return errors.New("the value must be JSON (quote strings: \"text\")")
	}
	if len(parts) >= 2 && parts[len(parts)-2] == "secret_refs" && (v.kind != 's' || !refPattern.MatchString(strings.Trim(string(v.scalar), `"`))) {
		return errors.New("a secret_refs value is a reference (keyring:, file:, env:, cmd:); store values with open-conduit secret set")
	}
	return d.root.set(parts, v)
}

// Unset removes the value at pointer.
func (d *ConfigDoc) Unset(ptr string) error {
	parts, err := pointer(ptr)
	if err != nil {
		return err
	}
	return d.root.unset(parts)
}

func (d *ConfigDoc) str(parts []string, key string) string {
	n, err := d.root.lookup(append(append([]string{}, parts...), key))
	if err != nil || n.kind != 's' {
		return ""
	}
	s, _ := strconv.Unquote(string(n.scalar))
	return s
}

// Schema is the documented fields for the node at pointer, from the adapter
// catalog: a connection's config and secret_refs, an extension entry's config,
// a route's config (by handler) and a binding's config (by executor) or
// resource config (by provider). Title names what they belong to.
func (d *ConfigDoc) Schema(ptr string, catalog []Adapter) (title string, fields []Field) {
	parts, err := pointer(ptr)
	if err != nil {
		return "", nil
	}
	find := func(typ string) *Adapter {
		for i := range catalog {
			if catalog[i].Type == typ {
				return &catalog[i]
			}
		}
		return nil
	}
	// Normalize array segments to indices for matching.
	n := len(parts)
	switch {
	case n >= 2 && parts[0] == "connections":
		base := parts[:2]
		a := find(d.str(base, "type"))
		if a == nil {
			return "", nil
		}
		if n >= 3 && parts[2] == "secret_refs" {
			return a.Title + " connection secret_refs", a.Secrets
		}
		return a.Title + " connection config", a.Connection
	case n >= 4 && parts[0] == "scopes" && parts[2] == "extensions":
		a := find(d.str(parts[:4], "type"))
		if a == nil {
			return "", nil
		}
		return a.Title + " entry config", a.Entry
	case n >= 4 && parts[0] == "scopes" && parts[2] == "routes":
		a := find(d.str(parts[:4], "handler"))
		if a == nil {
			return "", nil
		}
		return a.Title + " route config", a.Route
	case n >= 4 && parts[0] == "scopes" && parts[2] == "bindings":
		if n >= 5 && parts[4] == "resource" {
			a := find(d.str(parts[:5], "provider"))
			if a == nil {
				return "", nil
			}
			return a.Title + " resource config", a.Binding
		}
		a := find(d.str(parts[:4], "executor"))
		if a == nil {
			return "", nil
		}
		return a.Title + " binding config", a.Binding
	}
	return "", nil
}

// EditRequest starts the full-screen config editor on a file.
type EditRequest struct {
	Path    string
	Doc     *ConfigDoc
	Catalog []Adapter
	// Validate checks a candidate document offline against the daemon's
	// registrations (empty details: valid).
	Validate func([]byte) ([]client.Detail, error)
	// Save writes a validated document (owner-only, atomically).
	Save func([]byte) error
}

func (o *operator) editConfig(args []string) int {
	f := newFlags("edit", "file")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 0 {
		return usageError(o.errOut, "expected config edit [--file F]")
	}
	if o.env.EditConfig == nil {
		fmt.Fprintln(o.errOut, "open-conduit: no config editor in this binary; use config get/set/unset")
		return 1
	}
	path := o.configPath(f.get("file"))
	data, err := os.ReadFile(path)
	if err != nil {
		return o.emit(nil, fmt.Errorf("no config at %s; create one with open-conduit init", path))
	}
	doc, err := ParseConfigDoc(data)
	if err != nil {
		return o.emit(nil, err)
	}
	var catalog []Adapter
	if o.env.Adapters != nil {
		catalog = o.env.Adapters()
	}
	validate := func(b []byte) ([]client.Detail, error) { return o.validateConfig(path, b) }
	saved, err := o.env.EditConfig(EditRequest{Path: path, Doc: doc, Catalog: catalog, Validate: validate, Save: func(b []byte) error { return writePrivate(path, b) }})
	if err != nil {
		return o.emit(nil, err)
	}
	if saved {
		fmt.Fprintf(o.errOut, "Saved %s. A running daemon, even after a restart, keeps its stored configuration until you run: open-conduit config apply %s --generation N\n", path, path)
	}
	return 0
}
