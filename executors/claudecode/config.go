package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Config is the strictly checked binding `config` object owned by this
// executor. Every field is optional.
type Config struct {
	// Binary is the Claude Code CLI (default "claude", found on the
	// sanitized PATH).
	Binary string `json:"binary,omitempty"`
	// Model is the model alias or ID for turns (default "opus").
	Model string `json:"model,omitempty"`
	// PermissionMode grants permissions up front, because turns run
	// unattended (default "bypassPermissions").
	PermissionMode string `json:"permission_mode,omitempty"`
	// Playbook is the skill a turn starts on, as /open-conduit:<playbook>
	// (default "work-issue"; "-" starts turns with the rendered body alone).
	Playbook string `json:"playbook,omitempty"`
	// PluginDir is a directory of playbook skills passed with --plugin-dir.
	PluginDir string `json:"plugin_dir,omitempty"`
	// Args is the playbook argument template. {key} is the Work's first
	// correlation key, {number} the first number in it (the key when it has
	// none) and {work} the Work ID (default "{number}").
	Args string `json:"args,omitempty"`
	// Structured asks for the work-issue status with --json-schema and maps it
	// to the outcome (default true). Otherwise a turn that ends cleanly is
	// completed with its final text as the summary.
	Structured *bool `json:"structured,omitempty"`
	// StatusSchema names the outcome data schema the parsed status is
	// reported under. Optional: the default, domain.TurnSchema, is what
	// every projector reads and what the host publishes a turn's end from.
	// A source-named "<source>.status.v1" (written by earlier setups) is
	// read as domain.TurnSchema.
	StatusSchema string `json:"status_schema,omitempty"`
	// Branch is the optional workspace branch template ({key}, {number},
	// {work}) requested from a `work` resource.
	Branch string `json:"branch,omitempty"`
	// WorkspaceMode is the resource mode requested (default "work": a
	// session's transcript is tied to its working directory).
	WorkspaceMode string `json:"workspace_mode,omitempty"`
	// Dir is an existing absolute folder a workspace-free binding's sessions
	// run in, used as-is (no resource, nothing copied). Without it each Work
	// gets a private directory. A binding with a resource must not set it.
	Dir string `json:"dir,omitempty"`
	// HandoffMinutes is how long after a turn the session counts as cold
	// (default 55, Open Supervisor's prompt-cache horizon).
	HandoffMinutes *int `json:"handoff_minutes,omitempty"`
	// ColdTurns is what a cold turn does: "handoff" starts a fresh session
	// from the last turn's handoff note, "resume" resumes the session
	// (default "handoff").
	ColdTurns string `json:"cold_turns,omitempty"`
	// HandoffShare is the removed handoff experiment's setting. It is
	// accepted and ignored for one release (config check warns about it),
	// then rejected.
	HandoffShare *float64 `json:"handoff_share,omitempty"`
	// Env adds explicit non-secret variables after the sanitized baseline.
	Env map[string]string `json:"env,omitempty"`
	// PassEnv names additional process variables admitted to the baseline
	// (for example a proxy or CLAUDE_CONFIG_DIR).
	PassEnv []string `json:"pass_env,omitempty"`
	// SecretEnv maps binding secret names to variable names (for example an
	// API key to ANTHROPIC_API_KEY). Unmapped selected secrets use their
	// upper-cased name.
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	// ExtraArgs are added to every claude invocation.
	ExtraArgs []string `json:"extra_args,omitempty"`
	// BuiltinTools, when set, are the only built-in tools Claude Code offers
	// (passed as --tools). An empty list turns them all off, leaving the
	// agent only its MCP toolbelt. Unset keeps Claude Code's default set.
	BuiltinTools *[]string `json:"builtin_tools,omitempty"`
	// Models are models a Work may choose for its turns through a "model"
	// field in its context (for example one an operator picked when starting
	// it). A Work that names none, or one not listed, uses Model.
	Models []string `json:"models,omitempty"`
}

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	skillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	toolName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]*$`)
	// ErrConfig is returned for a binding config the executor rejects.
	// Messages never echo configured values.
	ErrConfig = errors.New("invalid claude code binding config")
)

var permissionModes = []string{"bypassPermissions", "acceptEdits", "auto", "default", "plan", "dontAsk"}

// What a cold turn does (Config.ColdTurns).
const (
	ColdHandoff = "handoff"
	ColdResume  = "resume"
)

func configError(msg string) error { return fmt.Errorf("%w: %s", ErrConfig, msg) }

// reserved reports toolbelt protocol variables no configured layer may set.
func reserved(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "OS_") }

// ParseConfig strictly decodes, validates and defaults a binding config.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return Config{}, configError("malformed config or unknown field")
		}
		if _, err := d.Token(); err != io.EOF {
			return Config{}, configError("trailing data after config object")
		}
	}
	strs := append(append([]string{c.Binary, c.Model, c.PermissionMode, c.Playbook, c.PluginDir, c.Args, c.StatusSchema, c.Branch, c.WorkspaceMode, c.Dir}, c.ExtraArgs...), c.Models...)
	for _, s := range strs {
		if strings.ContainsRune(s, 0) {
			return Config{}, configError("strings must not contain NUL")
		}
	}
	if c.Binary == "" {
		c.Binary = "claude"
	}
	if c.Model == "" {
		c.Model = "opus"
	}
	if c.PermissionMode == "" {
		c.PermissionMode = "bypassPermissions"
	}
	if !slices.Contains(permissionModes, c.PermissionMode) {
		return Config{}, configError("permission_mode is not a Claude Code permission mode")
	}
	switch c.Playbook {
	case "":
		c.Playbook = "work-issue"
	case "-":
	default:
		if !skillName.MatchString(c.Playbook) {
			return Config{}, configError("playbook must be a skill name")
		}
	}
	if c.PluginDir != "" && (!filepath.IsAbs(c.PluginDir) || filepath.Clean(c.PluginDir) != c.PluginDir) {
		return Config{}, configError("plugin_dir must be a clean absolute path")
	}
	if c.Dir != "" && (!filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) != c.Dir) {
		return Config{}, configError("dir must be a clean absolute path")
	}
	if c.Args == "" {
		c.Args = "{number}"
	}
	if c.Structured == nil {
		yes := true
		c.Structured = &yes
	}
	if c.StatusSchema == "" || c.StatusSchema != domain.TurnSchema && domain.IsTurnSchema(c.StatusSchema) {
		c.StatusSchema = domain.TurnSchema
	}
	if c.WorkspaceMode == "" {
		c.WorkspaceMode = "work"
	}
	if c.HandoffMinutes == nil {
		n := 55
		c.HandoffMinutes = &n
	}
	if *c.HandoffMinutes < 0 || *c.HandoffMinutes > 7*24*60 {
		return Config{}, configError("handoff_minutes must be between 0 and 10080")
	}
	switch c.ColdTurns {
	case "":
		c.ColdTurns = ColdHandoff
	case ColdHandoff, ColdResume:
	default:
		return Config{}, configError("cold_turns must be handoff or resume")
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
	for _, a := range c.ExtraArgs {
		if a == "--mcp-config" || strings.HasPrefix(a, "--mcp-config=") || a == "--session-id" || a == "--resume" || a == "-p" || a == "--print" {
			return Config{}, configError("extra_args must not override the executor's own session, print or toolbelt flags")
		}
		if c.BuiltinTools != nil && (a == "--tools" || strings.HasPrefix(a, "--tools=")) {
			return Config{}, configError("extra_args must not override builtin_tools")
		}
	}
	if c.BuiltinTools != nil {
		for _, t := range *c.BuiltinTools {
			if !toolName.MatchString(t) {
				return Config{}, configError("builtin_tools must name Claude Code tools, such as Read")
			}
		}
	}
	for _, m := range c.Models {
		if !modelName.MatchString(m) {
			return Config{}, configError("models must be model aliases or IDs")
		}
	}
	return c, nil
}

func (c Config) structured() bool { return c.Structured == nil || *c.Structured }

// model is the model for w's turns: the one its context names when the
// binding lists it in models, else the binding's model.
func (c Config) model(w domain.Work) string {
	if len(c.Models) == 0 || len(w.Context.Data) == 0 {
		return c.Model
	}
	var ctx struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(w.Context.Data, &ctx) == nil && slices.Contains(c.Models, ctx.Model) {
		return ctx.Model
	}
	return c.Model
}

// toolArgs are the --tools flag for builtin_tools, if set.
func (c Config) toolArgs() []string {
	if c.BuiltinTools == nil {
		return nil
	}
	return []string{"--tools", strings.Join(*c.BuiltinTools, ",")}
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
	// A dir that has gone fails its own turns as workspace_unavailable; it
	// must not make the rest of the configuration invalid.
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

// FolderConfig adapts a binding from an included folder's config
// (config.FolderConfigurer): relative dir and plugin_dir resolve against the
// folder, and a workspace-free binding without dir runs in the folder itself.
func (e *Executor) FolderConfig(raw json.RawMessage, folder string, resource bool) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, configError("malformed config")
		}
	}
	for _, key := range []string{"dir", "plugin_dir"} {
		var path string
		if v, ok := fields[key]; !ok || json.Unmarshal(v, &path) != nil || path == "" || filepath.IsAbs(path) {
			continue
		}
		fields[key], _ = json.Marshal(filepath.Join(folder, path))
	}
	if _, ok := fields["dir"]; !ok && !resource {
		fields["dir"], _ = json.Marshal(folder)
	}
	return json.Marshal(fields)
}

// isDir reports whether path is an existing directory (following links).
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Settings lists the binding config fields safe to show an operator, with
// defaults applied (extension.SettingsDescriber). The binary, plugin
// folder, arguments and environment are never listed.
func (e *Executor) Settings(raw json.RawMessage) []extension.Setting {
	c, err := ParseConfig(raw)
	if err != nil {
		return nil
	}
	return []extension.Setting{
		{Name: "model", Value: c.Model},
		{Name: "permission_mode", Value: c.PermissionMode},
		{Name: "playbook", Value: c.Playbook},
		{Name: "structured", Value: c.structured()},
		{Name: "workspace_mode", Value: c.WorkspaceMode},
		{Name: "cold_turns", Value: c.ColdTurns},
		{Name: "handoff_minutes", Value: *c.HandoffMinutes},
	}
}
