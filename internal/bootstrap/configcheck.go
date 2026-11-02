package bootstrap

import (
	"bytes"
	"path/filepath"
	"sync"

	"github.com/open-conduit/open-conduit/internal/config"
)

// ConfigCheck runs one offline validation for a CLI config check that asks
// for its warnings and then its diagnostics (cli.Env.ConfigWarnings, then
// ValidateConfigAt): Validate reuses, once, what ModeWarnings found for the
// same path and data, so each installed package is started once and both
// answers come from the same pass. Anything else validates afresh.
type ConfigCheck struct {
	check func(path string, data []byte) ([]config.Diagnostic, []string, error)

	mu    sync.Mutex
	path  string
	data  []byte
	diags []config.Diagnostic
	err   error
	held  bool
}

// NewConfigCheck validates with CheckConfigAt.
func NewConfigCheck() *ConfigCheck { return &ConfigCheck{check: CheckConfigAt} }

// ModeWarnings are config.ModeWarnings for the config at path, offline. The
// validation's diagnostics are kept for the next Validate of the same config.
func (c *ConfigCheck) ModeWarnings(path string, data []byte) []string {
	diags, warnings, err := c.check(path, data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.data, c.diags, c.err, c.held = path, bytes.Clone(data), diags, err, true
	return warnings
}

// Validate is ValidateConfigAt, reusing the diagnostics ModeWarnings kept
// for the same path and data.
func (c *ConfigCheck) Validate(path string, data []byte) ([]config.Diagnostic, error) {
	c.mu.Lock()
	held := c.held && c.path == path && bytes.Equal(c.data, data)
	diags, err := c.diags, c.err
	c.held, c.data, c.diags, c.err = false, nil, nil, nil
	c.mu.Unlock()
	if held {
		return diags, err
	}
	diags, _, err = c.check(path, data)
	return diags, err
}

// ConfigWarnings is everything `config check` warns about for the config at
// path: config.Warnings, renamed settings, include folders with no config,
// then the mode and executor binding warnings the caller's validation found. With machine false
// it leaves out the two that depend on the machine rather than the document
// (a cmd: secret's program not being installed, an included folder being
// missing), so a docs example is judged on what it says alone.
func ConfigWarnings(path string, data []byte, modes []string, machine bool) []string {
	var w []string
	if machine {
		w = config.Warnings(data)
	} else {
		w = config.UnenforcedWarnings(data)
	}
	w = append(w, RenamedWarnings(data)...)
	if machine {
		w = append(w, config.IncludeWarnings(data, filepath.Dir(path))...)
	}
	return append(w, modes...)
}
