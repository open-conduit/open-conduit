package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
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
	// Playbook is the skill a turn starts on, as /open-conduit:<playbook>.
	// "-", or leaving it out, starts turns with the rendered body alone.
	Playbook string `json:"playbook,omitempty"`
	// PluginDir is a directory of playbook skills passed with --plugin-dir.
	PluginDir string `json:"plugin_dir,omitempty"`
	// Args is the playbook argument template. {key} is the Work's first
	// correlation key, {number} the first number in it (the key when it has
	// none) and {work} the Work ID (default "{number}").
	Args string `json:"args,omitempty"`
	// Structured asks for the turn status (StatusSchema) with --json-schema and maps it
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
	// (default 55, the prompt-cache horizon).
	HandoffMinutes *int `json:"handoff_minutes,omitempty"`
	// ColdTurns is what a cold turn does: "handoff" starts a fresh session
	// from the last turn's handoff note, "resume" resumes the session
	// (default "handoff").
	ColdTurns string `json:"cold_turns,omitempty"`
	// HandoffTokens hands a warm session off as well: when the last turn
	// ended with more context than this, the next turn starts a fresh
	// session from the handoff note (default 400000; 0 turns it off).
	HandoffTokens *int64 `json:"handoff_tokens,omitempty"`
	// AutoCompactTokens is where Claude Code compacts a session in the
	// middle of a turn, set as CLAUDE_CODE_AUTO_COMPACT_WINDOW: it compacts
	// a little before this, or before the model's window if smaller
	// (default 600000; 0 leaves Claude Code's own threshold).
	AutoCompactTokens *int64 `json:"auto_compact_tokens,omitempty"`
	// HandoffShare is the removed handoff experiment's setting. It is
	// accepted and ignored for one release (validating the config warns about it),
	// then rejected.
	HandoffShare *float64 `json:"handoff_share,omitempty"`
	// Env adds explicit non-secret variables after the sanitized baseline.
	Env map[string]string `json:"env,omitempty"`
	// PassEnv names additional variables of the executor's own process
	// admitted to the baseline. Out of process that environment is
	// sanitized: the daemon's variables come through the binding's
	// pass_env (ExecutionContext.HostEnv) instead, and validating a config
	// that sets this warns so.
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
	// StrictMCP leaves the agent the toolbelt as its only MCP server
	// (--strict-mcp-config): none of the user's, a project's or claude.ai's
	// servers and connectors load.
	StrictMCP bool `json:"strict_mcp,omitempty"`
	// Models are models a Work may choose for its turns through a "model"
	// field in its context (for example one an operator picked when starting
	// it). A Work that names none, or one not listed, uses Model.
	Models []string `json:"models,omitempty"`
	// HomeSeed is a directory (a settings.json, plugins) copied into a
	// container binding's per-Work home on its first use. It is a copy,
	// never a mount, and its .credentials.json is left out.
	HomeSeed string `json:"home_seed,omitempty"`
	// Usage lets doctor read the subscription's 5-hour and 7-day usage
	// windows with this binding's Claude login (the undocumented endpoint
	// Claude Code itself uses), warning at 90%. It is off by default
	// because it reads the login token.
	Usage bool `json:"usage,omitempty"`
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

// The context limits on by default: a turn after one that ended with more
// context than DefaultHandoffTokens starts afresh from the handoff note, and
// Claude Code compacts in the middle of a turn near DefaultAutoCompactTokens.
const (
	DefaultHandoffTokens     int64 = 400_000
	DefaultAutoCompactTokens int64 = 600_000
)

// autoCompactVar is Claude Code's auto-compact window variable.
const autoCompactVar = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

// What a cold turn does (Config.ColdTurns).
const (
	ColdHandoff = "handoff"
	ColdResume  = "resume"
)

// invalidConfig is the executor's refusal of a binding config: ErrConfig,
// with the diagnostics (extension.ConfigError) that say which field is
// wrong and why. Messages never echo configured values.
type invalidConfig struct{ *extension.ConfigError }

func (e invalidConfig) Error() string {
	msgs := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		msgs[i] = d.Message
	}
	return ErrConfig.Error() + ": " + strings.Join(msgs, "; ")
}

func (e invalidConfig) Unwrap() []error { return []error{ErrConfig, e.ConfigError} }

// refuse is ce as the executor's refusal, nil when it holds nothing.
func refuse(ce *extension.ConfigError) error {
	if ce.Err() == nil {
		return nil
	}
	return invalidConfig{ce}
}

// reserved reports toolbelt protocol variables no configured layer may set.
func reserved(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "OS_") }

// ownFlags are the claude flags the executor sets itself, which extra_args
// must not override.
var ownFlags = []string{"--mcp-config", "--session-id", "--resume", "-p", "--print"}

// ParseConfig strictly decodes, validates and defaults a binding config.
// Its refusal is ErrConfig and an *extension.ConfigError naming every
// field that is wrong.
func ParseConfig(data []byte) (Config, error) {
	c, ce, decoded := parseConfig(data)
	if err := refuse(ce); err != nil || !decoded {
		return Config{}, err
	}
	return c, nil
}

// parseConfig is ParseConfig's work: the defaulted config, what is wrong
// with it, and whether it decoded at all (when it didn't, c is empty).
func parseConfig(data []byte) (c Config, ce *extension.ConfigError, decoded bool) {
	if err := extension.DecodeConfig(data, &c); err != nil {
		if !errors.As(err, &ce) {
			ce = extension.Invalid("", "malformed_config", "config must be one JSON object")
		}
		return Config{}, ce, false
	}
	ce = &extension.ConfigError{}
	nul := func(path, s string) {
		if strings.ContainsRune(s, 0) {
			ce.Add(path, "invalid_string", "strings must not contain NUL")
		}
	}
	for path, s := range map[string]string{"/binary": c.Binary, "/model": c.Model, "/permission_mode": c.PermissionMode, "/playbook": c.Playbook, "/plugin_dir": c.PluginDir, "/args": c.Args, "/status_schema": c.StatusSchema, "/branch": c.Branch, "/workspace_mode": c.WorkspaceMode, "/dir": c.Dir, "/home_seed": c.HomeSeed} {
		nul(path, s)
	}
	for i, a := range c.ExtraArgs {
		nul(extension.ConfigPointer("extra_args", i), a)
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
		ce.Add("/permission_mode", "unknown_permission_mode", "permission_mode must be one of "+strings.Join(permissionModes, ", "))
	}
	switch c.Playbook {
	case "", "-":
	default:
		if !skillName.MatchString(c.Playbook) {
			ce.Add("/playbook", "invalid_playbook", "playbook must be a skill name (letters, digits, dots, dashes and underscores), or - for none")
		}
	}
	for _, f := range []struct{ path, value string }{{"/plugin_dir", c.PluginDir}, {"/dir", c.Dir}, {"/home_seed", c.HomeSeed}} {
		if f.value != "" && (!filepath.IsAbs(f.value) || filepath.Clean(f.value) != f.value) {
			ce.Add(f.path, "invalid_path", strings.TrimPrefix(f.path, "/")+" must be a clean absolute path")
		}
	}
	if c.Args == "" {
		c.Args = "{number}"
	}
	if c.Structured == nil {
		yes := true
		c.Structured = &yes
	}
	if c.StatusSchema == "" || legacyStatusSchema(c.StatusSchema) {
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
		ce.Add("/handoff_minutes", "out_of_range", "handoff_minutes must be between 0 and 10080")
	}
	if c.HandoffTokens == nil {
		n := DefaultHandoffTokens
		c.HandoffTokens = &n
	}
	if c.AutoCompactTokens == nil {
		n := DefaultAutoCompactTokens
		c.AutoCompactTokens = &n
	}
	if *c.HandoffTokens < 0 {
		ce.Add("/handoff_tokens", "out_of_range", "handoff_tokens must not be negative; 0 turns it off")
	}
	if *c.AutoCompactTokens < 0 {
		ce.Add("/auto_compact_tokens", "out_of_range", "auto_compact_tokens must not be negative; 0 leaves Claude Code's own threshold")
	}
	switch c.ColdTurns {
	case "":
		c.ColdTurns = ColdHandoff
	case ColdHandoff, ColdResume:
	default:
		ce.Add("/cold_turns", "unknown_cold_turns", "cold_turns must be handoff or resume")
	}
	for _, name := range sortedKeys(c.Env) {
		switch {
		case !envName.MatchString(name):
			ce.Add(extension.ConfigPointer("env", name), "invalid_env_name", "env names must be variable names ([A-Za-z_][A-Za-z0-9_]*)")
		case reserved(name):
			ce.Add(extension.ConfigPointer("env", name), "reserved_env", "OS_* variables are the toolbelt's own; use another name")
		case strings.ContainsRune(c.Env[name], 0):
			ce.Add(extension.ConfigPointer("env", name), "invalid_string", "strings must not contain NUL")
		}
	}
	for i, name := range c.PassEnv {
		switch {
		case !envName.MatchString(name):
			ce.Add(extension.ConfigPointer("pass_env", i), "invalid_env_name", "pass_env names must be variable names ([A-Za-z_][A-Za-z0-9_]*)")
		case reserved(name):
			ce.Add(extension.ConfigPointer("pass_env", i), "reserved_env", "OS_* variables are the toolbelt's own; they can't be passed")
		}
	}
	for _, secret := range sortedKeys(c.SecretEnv) {
		name := c.SecretEnv[secret]
		switch {
		case secret == "":
			ce.Add(extension.ConfigPointer("secret_env", secret), "invalid_secret_name", "secret_env keys must be secret names")
		case !envName.MatchString(name):
			ce.Add(extension.ConfigPointer("secret_env", secret), "invalid_env_name", "secret_env must map a secret to a variable name ([A-Za-z_][A-Za-z0-9_]*)")
		case reserved(name):
			ce.Add(extension.ConfigPointer("secret_env", secret), "reserved_env", "OS_* variables are the toolbelt's own; map the secret to another name")
		}
	}
	for i, a := range c.ExtraArgs {
		flag, _, _ := strings.Cut(a, "=")
		if slices.Contains(ownFlags, a) || flag == "--mcp-config" {
			ce.Add(extension.ConfigPointer("extra_args", i), "reserved_argument", "extra_args must not set the flags the executor sets itself: "+strings.Join(ownFlags, ", "))
		} else if c.BuiltinTools != nil && flag == "--tools" {
			ce.Add(extension.ConfigPointer("extra_args", i), "reserved_argument", "extra_args must not set --tools when builtin_tools is set; list the tools in builtin_tools")
		}
	}
	if c.BuiltinTools != nil {
		for i, t := range *c.BuiltinTools {
			if !toolName.MatchString(t) {
				ce.Add(extension.ConfigPointer("builtin_tools", i), "invalid_tool", "builtin_tools must name Claude Code tools, such as Read")
			}
		}
	}
	for i, m := range c.Models {
		if !modelName.MatchString(m) {
			ce.Add(extension.ConfigPointer("models", i), "invalid_model", "models must be model aliases or IDs, such as opus or claude-opus-5-5")
		}
	}
	return c, ce, true
}

// sortedKeys are m's keys in order, so diagnostics come in a stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// playbook is the skill c's turns start on: its own, else "-" (none).
func (e *Executor) playbook(c Config) string {
	if c.Playbook != "" {
		return c.Playbook
	}
	return "-"
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

// toolArgs are the --tools flag for builtin_tools, if set, and
// --strict-mcp-config for strict_mcp.
func (c Config) toolArgs() []string {
	var out []string
	if c.BuiltinTools != nil {
		out = append(out, "--tools", strings.Join(*c.BuiltinTools, ","))
	}
	if c.StrictMCP {
		out = append(out, "--strict-mcp-config")
	}
	return out
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
	// Every problem is reported at once: the config's own, then how it
	// uses the binding's secrets. A dir that has gone fails its own turns
	// as workspace_unavailable; it must not make the rest of the
	// configuration invalid.
	c, ce, _ := parseConfig(raw)
	for _, secret := range sortedKeys(c.SecretEnv) {
		if !slices.Contains(secretNames, secret) {
			ce.Add(extension.ConfigPointer("secret_env", secret), "secret_not_selected", "secret_env names a secret the binding does not select; add it to the binding's secret_names or remove it here")
		}
	}
	for _, secret := range secretNames {
		if reserved(c.secretVariable(secret)) && c.SecretEnv[secret] == "" {
			ce.Add("/secret_env", "reserved_env", "a selected secret's default variable is an OS_* name, which is the toolbelt's own; map it to another name in secret_env")
		}
	}
	return refuse(ce)
}

// The reasons a sandboxed binding's claude config is refused. They name no
// configured value.
const (
	errOwnConfig    = "a sandboxed claude-code binding needs a config directory of its own: set env CLAUDE_CONFIG_DIR to an absolute path no host-side claude uses, and grant it in the sandbox's write list"
	errSharedConfig = "a sandboxed claude-code binding's CLAUDE_CONFIG_DIR must not be, hold or sit inside the operator's own ~/.claude or ~/.claude.json"
)

// SandboxConfig refuses a binding config that would give a sandboxed claude
// the operator's own claude config (config.SandboxChecker): anything it
// wrote there (a hook, an MCP server, a plugin) would run as the operator in
// every claude session on the host. The binding's env must set
// CLAUDE_CONFIG_DIR to an absolute path apart from ~/.claude and
// ~/.claude.json. The launch verifies the resolved directory again.
func (e *Executor) SandboxConfig(raw json.RawMessage) error {
	c, err := ParseConfig(raw)
	if err != nil {
		return nil // ValidateConfig reports it
	}
	dir := c.Env["CLAUDE_CONFIG_DIR"]
	if dir == "" || !filepath.IsAbs(dir) {
		return errors.New(errOwnConfig)
	}
	home, _ := os.UserHomeDir()
	if sharesConfig(dir, homes(home, c.Env["HOME"])) {
		return errors.New(errSharedConfig)
	}
	return nil
}

// homes are the distinct, non-empty home directories given: the operator's
// own and any a binding's env sets in its place.
func homes(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// operatorClaude is the operator's own claude config under each home.
func operatorClaude(homes []string) []string {
	var out []string
	for _, h := range homes {
		out = append(out, filepath.Join(h, ".claude"), filepath.Join(h, ".claude.json"))
	}
	return out
}

// sharesConfig reports whether a config directory is, holds or is inside
// the operator's own claude config under any of homes, symbolic links
// resolved.
func sharesConfig(dir string, homes []string) bool {
	d := extension.CanonicalPath(dir)
	for _, p := range operatorClaude(homes) {
		p = extension.CanonicalPath(p)
		if extension.Within(d, []string{p}) || extension.Within(p, []string{d}) {
			return true
		}
	}
	return false
}

// ConfigWarnings are what the executor warns an operator about in a valid
// binding config (extension.ConfigWarner): the removed handoff experiment's
// handoff_share, and, when the host gives bindings that name no playbook a
// default, a binding that relies on it, since a later release drops it.
func (e *Executor) ConfigWarnings(raw json.RawMessage) []string {
	c, err := ParseConfig(raw)
	if err != nil {
		return nil
	}
	var out []string
	if c.HandoffShare != nil {
		out = append(out, "config.handoff_share is ignored: the handoff experiment was removed and a later release rejects it; remove it, and set cold_turns (handoff or resume) to choose what a cold turn does")
	}
	if len(c.PassEnv) > 0 {
		out = append(out, "config.pass_env admits only variables of the executor's own process, which runs as a package with a sanitized environment; to pass variables of the daemon's environment (a proxy, CLAUDE_CONFIG_DIR), name them in the binding's pass_env")
	}
	return out
}

// ResourceMode is the resource mode a binding config requests (the
// configuration's ResourceModer): workspace_mode, "work" by default.
func (e *Executor) ResourceMode(raw json.RawMessage) string {
	c, err := ParseConfig(raw)
	if err != nil {
		return ""
	}
	return c.WorkspaceMode
}

// Playbook is the playbook a binding config starts turns on
// (extension.PlaybookNamer), "" for none.
func (e *Executor) Playbook(raw json.RawMessage) string {
	c, err := ParseConfig(raw)
	if err != nil || c.Playbook == "-" {
		return ""
	}
	return c.Playbook
}

// FolderConfig adapts a binding from an included folder's config
// (extension.FolderConfigurer): relative dir and plugin_dir resolve against the
// folder, and a workspace-free binding without dir runs in the folder itself.
func (e *Executor) FolderConfig(raw json.RawMessage, folder string, resource bool) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, invalidConfig{extension.Invalid("", "malformed_config", "config must be one JSON object")}
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
		{Name: "playbook", Value: e.playbook(c)},
		{Name: "structured", Value: c.structured()},
		{Name: "workspace_mode", Value: c.WorkspaceMode},
		{Name: "cold_turns", Value: c.ColdTurns},
		{Name: "handoff_minutes", Value: *c.HandoffMinutes},
		{Name: "handoff_tokens", Value: *c.HandoffTokens},
		{Name: "auto_compact_tokens", Value: *c.AutoCompactTokens},
	}
}

// legacyStatusSchema reports a source-named "<source>.status.v1" status
// schema, which earlier setups wrote before domain.TurnSchema.
func legacyStatusSchema(s string) bool {
	return strings.HasSuffix(s, ".status.v1") && len(s) > len(".status.v1")
}
