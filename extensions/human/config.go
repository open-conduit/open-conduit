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
	"strings"

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
}

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
	for _, r := range c.Responders {
		kind, id, _ := strings.Cut(r, ":")
		if !validPart(kind) || (strings.Contains(r, ":") && !validPart(id)) {
			return c, fmt.Errorf("responders: %q is not kind:id (for example %s)", r, DefaultResponder)
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
