package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Type is the registered resource provider name.
const Type = "git"

const (
	// WorkspaceSchema identifies Resource.Data describing an acquisition.
	WorkspaceSchema = "open-conduit.git.workspace.v1"
	// ParamsSchema identifies optional executor request params.
	ParamsSchema = "open-conduit.git.request.v1"

	// ModeClone is the Git provider's own mode: like work, a branch workspace
	// private to one Work and reused by its attempts, but a separate clone of
	// the checkout that shares no writable state with it, so a sandbox can be
	// given the whole of it.
	ModeClone = "clone"

	defaultRemote         = "origin"
	defaultBranchTemplate = "agent/work-{work}"
	defaultRetainSeconds  = 24 * 60 * 60
	defaultSetupSeconds   = 30 * 60
	maxSeconds            = 365 * 24 * 60 * 60
	// defaultDiskFull and defaultDiskReserve are v1's settings.host.disk
	// default and the most it kept free (worktree.MaxReserve).
	defaultDiskFull    = 90.0
	defaultDiskReserve = 10 << 30
)

// Config is the strictly checked binding `resource.config` object.
type Config struct {
	// Checkout is the scope's main checkout. A relative path resolves against
	// the provider's base directory (the config file's directory).
	Checkout string `json:"checkout"`
	// Mode is the default when an executor requests none: shared (default),
	// work, fresh or clone.
	Mode string `json:"mode,omitempty"`
	// Remote is fetched from and tracked; empty is origin.
	Remote string `json:"remote,omitempty"`
	// BaseBranch is where new branches and fresh checkouts start; empty asks
	// the remote's HEAD and falls back to main.
	BaseBranch string `json:"base_branch,omitempty"`
	// BranchTemplate names a work-mode branch; {work} and {scope} are replaced
	// by the sanitized Work and Scope IDs.
	BranchTemplate string `json:"branch_template,omitempty"`
	// Sign keeps commit/tag signing as the checkout configures it. Off (the
	// default), the environment layer disables signing so commits never wait
	// on a signer.
	Sign bool `json:"sign,omitempty"`
	// Copy lists checkout-relative files (such as .env) copied into work,
	// fresh and clone workspaces on every acquisition.
	Copy []string `json:"copy,omitempty"`
	// Setup runs (sh -c) in a work, fresh or clone workspace when the command or any
	// SetupWhen file changed since it last succeeded there. With SetupWhen set
	// and none of its files present, it does not run.
	Setup     string   `json:"setup,omitempty"`
	SetupWhen []string `json:"setup_when,omitempty"`
	// SetupTimeoutSeconds bounds setup (default 1800).
	SetupTimeoutSeconds int64 `json:"setup_timeout_seconds,omitempty"`
	// DotEnv opts into the checkout's .env as the provider environment layer.
	DotEnv bool `json:"dotenv,omitempty"`
	// RetainFailedSeconds keeps a failed fresh run's (or failed Work's)
	// workspace for inspection before removal (default 86400).
	RetainFailedSeconds int64 `json:"retain_failed_seconds,omitempty"`
	// DiskFullPercent is the share of a disk in use above which it is running
	// out (v1 settings.host.disk). A new work or fresh workspace is refused
	// when its checkout would leave less free than the rest of the disk keeps,
	// counting at most DiskReserveBytes. Default 90; 100 turns the check off.
	DiskFullPercent float64 `json:"disk_full_percent,omitempty"`
	// DiskReserveBytes caps what the check keeps free, so a large disk is not
	// held at its percentage's share (default 10 GiB, as in v1).
	DiskReserveBytes uint64 `json:"disk_reserve_bytes,omitempty"`
}

// ErrConfig is returned for a config the provider rejects. Messages never echo
// configured values.
var ErrConfig = errors.New("invalid git resource config")

func configError(msg string) error { return fmt.Errorf("%w: %s", ErrConfig, msg) }

var (
	remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	refPart    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// ParseConfig strictly decodes, validates and defaults a resource config.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		return c, configError("config object required")
	}
	if err := strictDecode(data, &c); err != nil {
		return Config{}, configError("malformed config or unknown field")
	}
	if c.Checkout == "" || strings.ContainsRune(c.Checkout, 0) {
		return Config{}, configError("checkout is required")
	}
	switch c.Mode {
	case "":
		c.Mode = extension.ResourceShared
	case extension.ResourceShared, extension.ResourceWork, extension.ResourceFresh, ModeClone:
	default:
		return Config{}, configError("mode must be shared, work, fresh or clone")
	}
	if c.Remote == "" {
		c.Remote = defaultRemote
	} else if !remoteName.MatchString(c.Remote) || strings.Contains(c.Remote, "..") {
		return Config{}, configError("remote must be a plain remote name")
	}
	if c.BaseBranch != "" && !validRef(c.BaseBranch) {
		return Config{}, configError("base_branch must be a valid branch name")
	}
	if c.BranchTemplate == "" {
		c.BranchTemplate = defaultBranchTemplate
	} else if !validRef(strings.NewReplacer("{work}", "w", "{scope}", "s").Replace(c.BranchTemplate)) {
		return Config{}, configError("branch_template must be a valid branch name using only {work} and {scope}")
	}
	for _, list := range [][]string{c.Copy, c.SetupWhen} {
		for _, name := range list {
			if !relativeFile(name) {
				return Config{}, configError("copy and setup_when entries must be clean relative paths inside the checkout")
			}
		}
	}
	if strings.ContainsRune(c.Setup, 0) {
		return Config{}, configError("setup must not contain NUL")
	}
	if c.SetupTimeoutSeconds < 0 || c.SetupTimeoutSeconds > maxSeconds || c.RetainFailedSeconds < 0 || c.RetainFailedSeconds > maxSeconds {
		return Config{}, configError("timeouts must be between 0 and 31536000 seconds")
	}
	if c.DiskFullPercent < 0 || c.DiskFullPercent > 100 {
		return Config{}, configError("disk_full_percent must be between 0 and 100")
	}
	if c.DiskFullPercent == 0 {
		c.DiskFullPercent = defaultDiskFull
	}
	if c.DiskReserveBytes == 0 {
		c.DiskReserveBytes = defaultDiskReserve
	}
	if c.SetupTimeoutSeconds == 0 {
		c.SetupTimeoutSeconds = defaultSetupSeconds
	}
	if c.RetainFailedSeconds == 0 {
		c.RetainFailedSeconds = defaultRetainSeconds
	}
	return c, nil
}

// ValidateConfig lets configuration loading reject a binding's resource config
// before any execution is scheduled.
func ValidateConfig(raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

// ErrSandboxMode refuses a sandboxed binding whose git workspace would write
// through to the operator's checkout.
var ErrSandboxMode = errors.New("a sandboxed binding can't use a git workspace in work, fresh or shared mode: " +
	"a worktree commits through the main checkout's .git (its config, hooks and branches) and shared mode is the main checkout itself; " +
	"use mode clone, a separate clone that shares nothing writable with the checkout")

// ErrSandboxSetup refuses setup for a sandboxed binding: setup runs on the
// host, outside the sandbox, in a workspace the agent wrote.
var ErrSandboxSetup = errors.New("a sandboxed binding can't run a git workspace's setup: " +
	"setup runs outside the sandbox, in files the agent wrote in an earlier attempt (its hooks, its package scripts); " +
	"remove setup and run it as the agent's first step instead, inside the sandbox")

// ValidateSandbox checks a git resource config for a binding that runs its
// executor in a sandbox (B09). mode is what the executor requests, empty for
// the config's own default. Only clone is allowed: a work or fresh worktree
// commits through the main checkout's .git, and shared is the checkout, so
// the sandbox would have to be able to write it. It reports the config's own
// error first.
func ValidateSandbox(raw json.RawMessage, mode string) error {
	c, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	if mode == "" {
		mode = c.Mode
	}
	if mode != ModeClone {
		return ErrSandboxMode
	}
	if c.Setup != "" {
		return ErrSandboxSetup
	}
	return nil
}

// ModeDefault is the mode a git resource config names as its default when
// the executor requests none, "" when it names none (shared applies then) or
// doesn't parse.
func ModeDefault(raw json.RawMessage) string {
	var named struct {
		Mode string `json:"mode"`
	}
	if _, err := ParseConfig(raw); err != nil || json.Unmarshal(raw, &named) != nil {
		return ""
	}
	return named.Mode
}

// params are the optional executor request params (ParamsSchema).
type params struct {
	// Branch requests a work-mode branch instead of the template.
	Branch string `json:"branch,omitempty"`
}

func parseParams(data []byte) (params, error) {
	var p params
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return p, nil
	}
	if err := strictDecode(data, &p); err != nil {
		return params{}, fmt.Errorf("%w: malformed git request params", extension.ErrInvalid)
	}
	if p.Branch != "" && !validRef(p.Branch) {
		return params{}, fmt.Errorf("%w: requested branch is not a valid branch name", extension.ErrInvalid)
	}
	return p, nil
}

func strictDecode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// validRef is a conservative subset of git-check-ref-format for branch names.
func validRef(s string) bool {
	return refPart.MatchString(s) && !(strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") || strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "/.") || strings.HasPrefix(s, "."))
}

func relativeFile(name string) bool {
	if name == "" || strings.ContainsRune(name, 0) || filepath.IsAbs(name) || filepath.Clean(name) != name {
		return false
	}
	first := strings.Split(filepath.ToSlash(name), "/")[0]
	return name != "." && first != ".." && first != ".git"
}

// sanitize maps an opaque ID onto a branch-safe path component.
func sanitize(id string) string {
	s := strings.Trim(unsafeName.ReplaceAllString(id, "-"), "-.")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if len(s) > 80 {
		s = s[:80]
	}
	s = strings.TrimSuffix(strings.Trim(s, "-."), ".lock")
	if s == "" {
		s = "x"
	}
	return s
}

// Settings lists the resource config fields safe to show an operator, with
// defaults applied. The setup command and copied files are never listed.
func Settings(raw json.RawMessage) []extension.Setting {
	c, err := ParseConfig(raw)
	if err != nil {
		return nil
	}
	return []extension.Setting{
		{Name: "checkout", Value: c.Checkout},
		{Name: "mode", Value: c.Mode},
		{Name: "remote", Value: c.Remote},
		{Name: "base_branch", Value: c.BaseBranch},
		{Name: "branch_template", Value: c.BranchTemplate},
		{Name: "sign", Value: c.Sign},
	}
}
