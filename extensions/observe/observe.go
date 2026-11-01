// Package observe is the `observe` extension (issue #318): an executor
// reports what it observed in an outside source, and Open Conduit turns it
// into Work.
//
// Three identities are kept apart:
//
//   - Source: the logical outside source ("slack-help"), declared by the
//     scope's `observe` entry. It is part of every key, so it survives
//     replacing the executor that observes it.
//   - Reporter: the binding whose execution called observe.record. The
//     entry lists the bindings that may report for each source; the
//     toolbelt's trusted execution source and causation keep the reporter
//     for audit. It is never part of an identity.
//   - Item: the source's own ID for the item and its revision. Recording
//     the same item and revision again is the first Event again
//     (extension.EmitPlan.Once); the same item and revision with different
//     content is refused, so an edit needs a new revision.
//
// The `observe` route turns each thread of its source into Work of kind
// observe.thread: a new thread key starts Work and later items of the
// thread are its input.
package observe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/open-conduit/open-conduit/domain"
)

const (
	// Type is the extension type, the route handler, the toolbelt
	// registration name and the root of its correlation sources.
	Type = "observe"
	// OpRecord is the agent-facing operation.
	OpRecord = "observe.record"
	// TypeRecorded is the Event family of a recorded item.
	TypeRecorded = "observe.item.recorded"
	// SchemaItem is a recorded item's payload schema.
	SchemaItem = "observe.item.v1"
	// WorkKind is the kind of Work a thread gets.
	WorkKind = "observe.thread"
)

// Input limits.
const (
	MaxID       = 256
	MaxRevision = 128
	MaxText     = 16 << 10
)

// Operations lists every tool operation (for binding operation lists).
func Operations() []string { return []string{OpRecord} }

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Source is one logical outside source and the bindings that may report
// for it.
type Source struct {
	Name      string   `json:"name"`
	Reporters []string `json:"reporters"`
}

// Config is a scope's `observe` entry.
type Config struct {
	Sources []Source `json:"sources"`
}

func strict(data json.RawMessage, v any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// ParseConfig parses and checks an entry's config.
func ParseConfig(data json.RawMessage) (Config, error) {
	var c Config
	if err := strict(data, &c); err != nil {
		return c, err
	}
	if len(c.Sources) == 0 {
		return c, errors.New("sources: name at least one source")
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if !namePattern.MatchString(s.Name) {
			return c, fmt.Errorf("sources/%d/name: %q is not a source name (lower case letters, digits, '.', '_' and '-', at most 64)", i, s.Name)
		}
		if seen[s.Name] {
			return c, fmt.Errorf("sources/%d/name: %q is declared twice", i, s.Name)
		}
		seen[s.Name] = true
		if len(s.Reporters) == 0 {
			return c, fmt.Errorf("sources/%d/reporters: name at least one binding that may report for %s", i, s.Name)
		}
		for j, r := range s.Reporters {
			if r == "" || len(r) > 128 {
				return c, fmt.Errorf("sources/%d/reporters/%d: not a binding name", i, j)
			}
		}
	}
	return c, nil
}

// ValidateExtension is the entry type's config validator.
func ValidateExtension(config json.RawMessage, secretNames []string) error {
	if len(secretNames) > 0 {
		return errors.New("observe takes no secrets")
	}
	_, err := ParseConfig(config)
	return err
}

// RouteConfig is an `observe` route's config: the source whose threads it
// takes.
type RouteConfig struct {
	Source string `json:"source"`
}

// ParseRoute parses and checks a route's config.
func ParseRoute(data json.RawMessage) (RouteConfig, error) {
	var r RouteConfig
	if err := strict(data, &r); err != nil {
		return r, err
	}
	if !namePattern.MatchString(r.Source) {
		return r, errors.New("source: name the source whose threads this route takes")
	}
	return r, nil
}

// ValidateRoute is the route config validator.
func ValidateRoute(config json.RawMessage) error {
	_, err := ParseRoute(config)
	return err
}

// Sources are the sources the scope's `observe` entries declare, by name.
func Sources(scope domain.Scope) map[string]Source {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return nil
	}
	out := map[string]Source{}
	for _, x := range meta.Extensions {
		if x.Type != Type {
			continue
		}
		c, err := ParseConfig(x.Config)
		if err != nil {
			continue
		}
		for _, s := range c.Sources {
			if have, ok := out[s.Name]; ok {
				s.Reporters = append(have.Reporters, s.Reporters...)
			}
			out[s.Name] = s
		}
	}
	return out
}

// ReportsFor are the names of the scope's sources binding may report for,
// sorted.
func ReportsFor(scope domain.Scope, binding string) []string {
	var out []string
	for name, s := range Sources(scope) {
		if slices.Contains(s.Reporters, binding) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Key is a thread's correlation key: the source, never the reporter.
func Key(scope domain.ID, source, thread string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: SourceNamespace(scope, source), ExternalID: thread}
}

// SourceNamespace is the correlation source of a scope's source.
func SourceNamespace(scope domain.ID, source string) string {
	return Type + "/" + string(scope) + "/" + source
}

// Item is a recorded item's payload.
type Item struct {
	Source   string `json:"source"`
	Item     string `json:"item"`
	Revision string `json:"revision,omitempty"`
	Thread   string `json:"thread"`
	Text     string `json:"text"`
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
