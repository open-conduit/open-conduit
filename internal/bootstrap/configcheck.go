package bootstrap

import (
	"bytes"
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
