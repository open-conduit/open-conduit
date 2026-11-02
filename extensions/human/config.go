// Package human is the `human` extension (issue #52): private decisions a
// person makes for Work, wherever the Work came from.
//
// An agent calls `human.ask {question, options?, context?, deadline?}`. Its
// Work parks on a translated wait whose one dependency is a fresh `ask/<id>`
// key that no Work owns; the scope's `human` entry decides who may answer
// and where the ask is shown. A person answers through an action the
// extension authorises (the TUI's "answer" on the asking Work); the action's
// Event lands on the ask key under core.action and is aliased into the
// asking Work, which wakes on the first one. Nothing about the ask is
// projected to the Work's source.
package human

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Type is the extension type, the toolbelt registration name and the
// correlation source of ask keys.
const Type = "human"

// SurfaceTUI shows an ask on the asking Work in the TUI (its annotations)
// and answers it through the Work's "answer" action.
const SurfaceTUI = "tui"

// DefaultResponder is the local operator: the owner of the management
// socket, who answers from the TUI or the CLI.
var DefaultResponder = extension.LocalOperator.Kind + ":" + extension.LocalOperator.ID

// Config is a scope's `human` entry.
type Config struct {
	// Responders are who may answer this scope's asks, each "kind:id" (a
	// caller identity) or a bare kind for any caller of that kind. Default
	// ["operator:local"].
	Responders []string `json:"responders,omitempty"`
	// Surfaces are where each ask is shown. Default ["tui"], the only
	// surface so far.
	Surfaces []string `json:"surfaces,omitempty"`
	// Gates are operations that need a person's approval before they run,
	// whoever calls them (see Gate).
	Gates []GateConfig `json:"gates,omitempty"`
}

// GateConfig is one approval gate of a scope's `human` entry.
type GateConfig struct {
	// Operations are toolbelt operation names ("github.pr.create"), or a
	// prefix ending in ".*" for every operation under it ("deploy.*").
	Operations []string `json:"operations"`
	// Responders are who may approve, as in Config.Responders. Default the
	// entry's responders.
	Responders []string `json:"responders,omitempty"`
	// Timeout is how long an approval request waits for an answer, a Go
	// duration ("24h", at most a year). With no answer by then, the call
	// is refused. Default "24h".
	Timeout string `json:"timeout,omitempty"`
	// timeout is Timeout parsed.
	timeout time.Duration
}

// DefaultGateTimeout is how long an approval request waits by default.
const DefaultGateTimeout = 24 * time.Hour

// maxGateTimeout is the longest an approval request may wait: the
// longest a wait's timeout may be.
const maxGateTimeout = 365 * 24 * time.Hour

var gateOperation = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// ParseConfig parses and checks an entry's config, filling defaults.
func ParseConfig(data json.RawMessage) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if d.More() {
		return c, errors.New("trailing data")
	}
	if len(c.Responders) == 0 {
		c.Responders = []string{DefaultResponder}
	}
	if err := checkResponders("responders", c.Responders); err != nil {
		return c, err
	}
	for i := range c.Gates {
		g := &c.Gates[i]
		where := fmt.Sprintf("gates[%d]", i)
		if len(g.Operations) == 0 {
			return c, fmt.Errorf("%s: operations: name at least one operation", where)
		}
		for _, op := range g.Operations {
			name := strings.TrimSuffix(op, ".*")
			if !gateOperation.MatchString(name) || strings.HasSuffix(name, ".") {
				return c, fmt.Errorf("%s: operations: %q is not an operation name or a prefix ending in .* (for example github.pr.create or deploy.*)", where, op)
			}
			if name == Type || strings.HasPrefix(name, Type+".") {
				return c, fmt.Errorf("%s: operations: %q is human's own; its operations can't need approval", where, op)
			}
		}
		if len(g.Responders) == 0 {
			g.Responders = c.Responders
		}
		if err := checkResponders(where+": responders", g.Responders); err != nil {
			return c, err
		}
		g.timeout = DefaultGateTimeout
		if g.Timeout != "" {
			d, err := time.ParseDuration(g.Timeout)
			if err != nil || d < time.Minute || d > maxGateTimeout {
				return c, fmt.Errorf("%s: timeout: %q must be a duration from 1m to 8760h, such as 24h", where, g.Timeout)
			}
			g.timeout = d
		}
	}
	if len(c.Surfaces) == 0 {
		c.Surfaces = []string{SurfaceTUI}
	}
	for _, s := range c.Surfaces {
		if s != SurfaceTUI {
			return c, fmt.Errorf("surfaces: unknown surface %q (asks can be shown in: %s)", s, SurfaceTUI)
		}
	}
	return c, nil
}

func checkResponders(field string, responders []string) error {
	for _, r := range responders {
		kind, id, _ := strings.Cut(r, ":")
		if !validPart(kind) || (strings.Contains(r, ":") && !validPart(id)) {
			return fmt.Errorf("%s: %q is not kind:id (for example %s)", field, r, DefaultResponder)
		}
	}
	return nil
}

// gate is the first gate that names op, if any.
func (c Config) gate(op string) (GateConfig, bool) {
	for _, g := range c.Gates {
		for _, pattern := range g.Operations {
			if pattern == op || strings.HasSuffix(pattern, ".*") && strings.HasPrefix(op, strings.TrimSuffix(pattern, "*")) {
				return g, true
			}
		}
	}
	return GateConfig{}, false
}

func validPart(s string) bool {
	return s != "" && len(s) <= 128 && !strings.ContainsAny(s, ": \t\r\n")
}

// ValidateExtension is the entry type's config validator.
func ValidateExtension(config json.RawMessage, secretNames []string) error {
	if len(secretNames) > 0 {
		return errors.New("human takes no secrets")
	}
	_, err := ParseConfig(config)
	return err
}

// Allows reports whether the entry lets c answer.
func (c Config) Allows(caller extension.Caller) bool {
	for _, r := range c.Responders {
		kind, id, scoped := strings.Cut(r, ":")
		if kind == caller.Kind && (!scoped || id == caller.ID) {
			return true
		}
	}
	return false
}

// Entry is a scope's effective `human` entry: its name (the action
// provider's name) and config.
type Entry struct {
	Name   string
	Config Config
}
