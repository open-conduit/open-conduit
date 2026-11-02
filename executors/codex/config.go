package codex

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

	"github.com/open-conduit/open-conduit/domain"
)

// Config is the strictly checked binding `config` object. Every field is
// optional.
type Config struct {
	// Binary is the Codex CLI (default "codex", found on PATH).
	Binary string `json:"binary,omitempty"`
	// Model is passed as --model; empty keeps Codex's own default.
	Model string `json:"model,omitempty"`
	// Sandbox is Codex's own sandbox_mode for the commands the agent runs:
	// read-only, workspace-write or danger-full-access (the default, like
	// Claude Code's bypassPermissions: the agent pushes and calls the
	// network). Approvals are always off: nobody is there to answer them.
	Sandbox string `json:"sandbox,omitempty"`
	// Playbook is the playbook a turn starts on: its text is the start of
	// the turn's prompt. "-" or empty means none.
	Playbook string `json:"playbook,omitempty"`
	// PluginDir holds the playbooks, as <dir>/skills/<playbook>/SKILL.md.
	// Default: the host's own (ExecutionContext.PlaybookDir).
	PluginDir string `json:"plugin_dir,omitempty"`
	// Args is the playbook's argument template (default {number}).
	Args string `json:"args,omitempty"`
	// Structured asks for the turn status with --output-schema (default
	// true) and maps it to the outcome.
	Structured *bool `json:"structured,omitempty"`
	// Branch and WorkspaceMode are the workspace request.
	Branch        string `json:"branch,omitempty"`
	WorkspaceMode string `json:"workspace_mode,omitempty"`
	// Dir is an absolute folder a workspace-free binding's sessions run in.
	Dir string `json:"dir,omitempty"`
	// Env, PassEnv and SecretEnv are the environment.
	Env       map[string]string `json:"env,omitempty"`
	PassEnv   []string          `json:"pass_env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	// ExtraArgs are added to every `codex exec`; they may not set the
	// session, the output files, the sandbox, the approval policy, the
	// model or the toolbelt's MCP server, by flag or -c override.
	ExtraArgs []string `json:"extra_args,omitempty"`
	// TimeoutSeconds bounds a segment; the deadline survives restart.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
}

const maxTimeoutSeconds = 365 * 24 * 60 * 60

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// ErrConfig is returned for a binding config the executor rejects.
	// Messages never echo configured values.
	ErrConfig = errors.New("invalid codex binding config")
	sandboxes = []string{"read-only", "workspace-write", "danger-full-access"}
	// forbidden are the flags extra_args may not carry: the session, the
	// output the executor reads and the approval and sandbox it sets.
	forbidden = []string{"--json", "--output-schema", "-o", "--output-last-message", "--last", "-s", "--sandbox", "-a", "--ask-for-approval", "-C", "--cd", "--ephemeral", "-m", "--model", "--full-auto", "--dangerously-bypass-approvals-and-sandbox", "--yolo"}
	// forbiddenKeys are the config keys a -c/--config override in
	// extra_args may not set; a later -c wins in Codex, so these would
	// replace the executor's own.
	forbiddenKeys = []string{"sandbox_mode", "approval_policy", "model"}
)

func configError(msg string) error { return fmt.Errorf("%w: %s", ErrConfig, msg) }

// reserved names no earlier environment layer may set.
func reserved(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "OS_") }

// ParseConfig strictly decodes, validates and defaults a binding config.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, configError("malformed config or unknown field")
	}
	if _, err := d.Token(); err != io.EOF {
		return Config{}, configError("trailing data after config object")
	}
	strs := append(append([]string{c.Binary, c.Model, c.Sandbox, c.Playbook, c.PluginDir, c.Args, c.Branch, c.WorkspaceMode, c.Dir}, c.ExtraArgs...), c.PassEnv...)
	for _, s := range strs {
		if strings.ContainsRune(s, 0) {
			return Config{}, configError("strings must not contain NUL")
		}
	}
	if c.Binary == "" {
		c.Binary = "codex"
	}
	if c.Sandbox == "" {
		c.Sandbox = "danger-full-access"
	}
	if !slices.Contains(sandboxes, c.Sandbox) {
		return Config{}, configError("sandbox must be read-only, workspace-write or danger-full-access")
	}
	if c.Args == "" {
		c.Args = "{number}"
	}
	if c.Structured == nil {
		t := true
		c.Structured = &t
	}
	if c.Playbook != "" && c.Playbook != "-" && !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(c.Playbook) {
		return Config{}, configError("playbook must be a skill name")
	}
	for _, p := range []string{c.Dir, c.PluginDir} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			return Config{}, configError("dir and plugin_dir must be clean absolute paths")
		}
	}
	if !extraArgsAllowed(c.ExtraArgs) {
		return Config{}, configError("extra_args may not set the session, output, sandbox, approvals, model or toolbelt")
	}
	for name, value := range c.Env {
		if !envName.MatchString(name) || reserved(name) || strings.ContainsRune(value, 0) {
			return Config{}, configError("env names must be valid, non-reserved (OS_*) variables")
		}
	}
	for _, name := range c.PassEnv {
		if !envName.MatchString(name) || reserved(name) {
			return Config{}, configError("pass_env names must be valid, non-reserved (OS_*) variables")
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

// extraArgsAllowed reports whether extra_args leave the session, output,
// sandbox, approval policy, model and toolbelt server as the executor
// sets them, as flags or as -c/--config overrides.
func extraArgsAllowed(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, _, _ := strings.Cut(a, "=")
		if slices.Contains(forbidden, flag) || a == "resume" {
			return false
		}
		var override string
		switch {
		case a == "-c" || a == "--config":
			if i+1 == len(args) {
				return false
			}
			i++
			override = args[i]
		case strings.HasPrefix(a, "--config="):
			override = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-c"):
			override = strings.TrimPrefix(strings.TrimPrefix(a, "-c"), "=")
		default:
			continue
		}
		key, _, _ := strings.Cut(override, "=")
		key = strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(key)
		if slices.Contains(forbiddenKeys, key) || key == "mcp_servers" || key == "mcp_servers."+mcpServer || strings.HasPrefix(key, "mcp_servers."+mcpServer+".") {
			return false
		}
	}
	return true
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

// ValidateConfig rejects an invalid binding before any execution is
// scheduled. secretNames are the binding's selected secrets.
func ValidateConfig(raw json.RawMessage, secretNames []string) error {
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

// ResourceMode is the workspace mode a binding requests (default "work":
// the session belongs to the Work, so its folder must last across turns).
func ResourceMode(raw json.RawMessage) string {
	c, err := ParseConfig(raw)
	if err != nil || c.WorkspaceMode == "" {
		return "work"
	}
	return c.WorkspaceMode
}

// expand fills an argument or branch template: {key} is the Work's first
// correlation key, {number} the first number in it (the key when it has
// none) and {work} the Work ID.
func expand(template string, w domain.Work) string {
	key := ""
	if len(w.CorrelationKeys) > 0 {
		key = w.CorrelationKeys[0].ExternalID
	}
	number := regexp.MustCompile(`\d+`).FindString(key)
	if number == "" {
		number = key
	}
	return strings.NewReplacer("{key}", key, "{number}", number, "{work}", string(w.ID)).Replace(template)
}
