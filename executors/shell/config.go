package shell

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Config is the strictly checked binding `config` object owned by this executor.
// Exactly one of Command (run by /bin/sh -c) or Argv is required.
type Config struct {
	Command string   `json:"command,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	// Dir is an absolute working directory. Without it the command runs in a
	// private per-segment scratch directory: no workspace is required.
	Dir string `json:"dir,omitempty"`
	// Env adds explicit non-secret variables after the sanitized baseline.
	Env map[string]string `json:"env,omitempty"`
	// SecretEnv maps binding secret names to variable names. Unmapped selected
	// secrets use their upper-cased name with other characters replaced by _.
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	// TimeoutSeconds bounds the process tree; the deadline survives restart.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
}

const maxTimeoutSeconds = 365 * 24 * 60 * 60

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reserved reports toolbelt protocol variables that no earlier environment layer
// may set or override.
func reserved(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "OS_") }

// ErrConfig is returned for a binding config the executor rejects. Messages never
// echo configured values.
var ErrConfig = errors.New("invalid shell binding config")

func configError(msg string) error { return fmt.Errorf("%w: %s", ErrConfig, msg) }

// ParseConfig strictly decodes and validates a binding config object.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		return c, configError("config object required")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, configError("malformed config or unknown field")
	}
	if _, err := d.Token(); err != io.EOF {
		return Config{}, configError("trailing data after config object")
	}
	if (c.Command == "") == (len(c.Argv) == 0) {
		return Config{}, configError("exactly one of command or argv is required")
	}
	if len(c.Argv) > 0 && c.Argv[0] == "" {
		return Config{}, configError("argv[0] must name a program")
	}
	for _, s := range append([]string{c.Command, c.Dir}, c.Argv...) {
		if strings.ContainsRune(s, 0) {
			return Config{}, configError("strings must not contain NUL")
		}
	}
	if c.Dir != "" && (!filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) != c.Dir) {
		return Config{}, configError("dir must be a clean absolute path")
	}
	for name, value := range c.Env {
		if !envName.MatchString(name) || reserved(name) || strings.ContainsRune(value, 0) {
			return Config{}, configError("env names must be valid, non-reserved (OS_*) variables")
		}
	}
	for secret, name := range c.SecretEnv {
		if secret == "" || !envName.MatchString(name) || reserved(name) {
			return Config{}, configError("secret_env must map secret names to valid, non-reserved (OS_*) variables")
		}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > maxTimeoutSeconds {
		return Config{}, configError("timeout_seconds must be between 0 and 31536000")
	}
	return c, nil
}

// secretVariable is the environment name for one selected secret.
func (c Config) secretVariable(secret string) string {
	if name, ok := c.SecretEnv[secret]; ok {
		return name
	}
	b := []byte(strings.ToUpper(secret))
	for i, ch := range b {
		if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			b[i] = '_'
		}
	}
	name := string(b)
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	return name
}

// ValidateConfig lets configuration loading reject an invalid binding before
// any execution is scheduled. secretNames are the binding's selected secrets.
func (e *Executor) ValidateConfig(raw json.RawMessage, secretNames []string) error {
	c, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	for secret := range c.SecretEnv {
		if !slices.Contains(secretNames, secret) {
			return configError("secret_env names a secret the binding does not select")
		}
	}
	for _, secret := range secretNames {
		if reserved(c.secretVariable(secret)) {
			return configError("a selected secret maps to a reserved OS_* variable; set secret_env")
		}
	}
	return nil
}
