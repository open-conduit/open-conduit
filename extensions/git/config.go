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

	"github.com/alsiola/open-conduit/extension"
)

// Type is the registered resource provider name.
const Type = "git"

const (
	// WorkspaceSchema identifies Resource.Data describing an acquisition.
	WorkspaceSchema = "open-conduit.git.workspace.v1"
	// ParamsSchema identifies optional executor request params.
	ParamsSchema = "open-conduit.git.request.v1"

	defaultRemote         = "origin"
	defaultBranchTemplate = "agent/work-{work}"
	defaultRetainSeconds  = 24 * 60 * 60
	defaultSetupSeconds   = 30 * 60
	maxSeconds            = 365 * 24 * 60 * 60
)

// Config is the strictly checked binding `resource.config` object.
type Config struct {
	// Checkout is the scope's main checkout. A relative path resolves against
	// the provider's base directory (the config file's directory).
	Checkout string `json:"checkout"`
	// Mode is the default when an executor requests none: shared (default),
	// work or fresh.
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
	// Copy lists checkout-relative files (such as .env) copied into work and
	// fresh workspaces on every acquisition.
	Copy []string `json:"copy,omitempty"`
	// Setup runs (sh -c) in a work or fresh workspace when the command or any
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
	case extension.ResourceShared, extension.ResourceWork, extension.ResourceFresh:
	default:
		return Config{}, configError("mode must be shared, work or fresh")
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
