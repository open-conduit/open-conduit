package claudecode

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Probes are the executor's probes for setup sessions: `claude-code.status`,
// whether Claude Code can do the work. LookPath, Version, LoginToken and
// Getenv are the system's unless set (tests).
type Probes struct {
	LookPath   func(string) (string, error)
	Version    func(context.Context, string) (string, error)
	LoginToken func(context.Context, string) (string, error)
	Getenv     func(string) string
}

var _ extension.SetupProbes = Probes{}

// InstallStatus is the status probe's output: never a token or account detail.
type InstallStatus struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	SignedIn  bool   `json:"signed_in"`
	// Login is how a turn would sign in: "secret" (a secret the binding
	// selects, named by the secret_env argument), "api_key" (a credential
	// in the daemon's environment that the pass_env argument says the
	// binding passes through), "subscription" (a Claude login) or
	// "provider": the probe was given a provider's base URL, so turns sign
	// in with the binding's provider secret and a Claude login doesn't
	// apply. A credential in the daemon's environment that the binding
	// doesn't pass reaches no turn, so it is not a sign-in.
	Login string `json:"login,omitempty"`
	// Provider is the provider's host, when the probe was given one.
	Provider string `json:"provider,omitempty"`
	Next     string `json:"next,omitempty"`
}

func (p Probes) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{{Name: "status", Summary: "whether claude is on the daemon's PATH, its version, and whether it is signed in and how",
		Args: []extension.ProbeArg{
			{Name: "base_url", Summary: "the provider base URL a binding will set (provider.base_url), when its turns go to a provider other than Anthropic; the Claude sign-in then doesn't apply"},
			{Name: "pass_env", Summary: "the variables the binding's pass_env names, comma-separated: a credential in the daemon's environment reaches a turn only through these"},
			{Name: "secret_env", Summary: "the variables the binding's selected secrets set, comma-separated, when one is a credential (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN or CLAUDE_CODE_OAUTH_TOKEN)"},
		},
		Run: func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
			return extension.ProbeResult{Command: "claude --version", Output: p.status(ctx, args["base_url"], names(args["pass_env"]), names(args["secret_env"]))}, nil
		}}}
}

func (p Probes) status(ctx context.Context, baseURL string, passEnv, secretEnv []string) InstallStatus {
	look, version, login, getenv := p.LookPath, p.Version, p.LoginToken, p.Getenv
	if look == nil {
		look = exec.LookPath
	}
	if version == nil {
		version = Version
	}
	if login == nil {
		login = LoginToken
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	var s InstallStatus
	path, err := look("claude")
	if err != nil {
		s.Next = "install Claude Code (https://claude.com/claude-code) where the daemon runs, then restart the daemon"
		return s
	}
	s.Installed, s.Path = true, path
	if v, err := version(ctx, path); err == nil {
		s.Version = v
	}
	if baseURL != "" {
		host, ok := providerHost(baseURL)
		if !ok {
			s.Next = "the base URL must be an absolute http or https URL with a host, and no credentials, query or fragment"
			return s
		}
		// The provider's key is a selected secret the probe can't see.
		s.SignedIn, s.Login, s.Provider = true, "provider", host
		return s
	}
	// In the order a turn's environment gives them precedence: a selected
	// secret, then the daemon's variable the binding passes, then the login.
	for _, name := range credentialEnvs {
		if slices.Contains(secretEnv, name) {
			s.SignedIn, s.Login = true, "secret"
			return s
		}
	}
	for _, name := range credentialEnvs {
		if slices.Contains(passEnv, name) && getenv(name) != "" {
			s.SignedIn, s.Login = true, "api_key"
			return s
		}
	}
	if _, err := login(ctx, ""); err == nil {
		s.SignedIn, s.Login = true, "subscription"
		return s
	}
	s.Next = "store a model credential as a secret and select it on the binding, or run claude once on this machine and sign in"
	for _, name := range credentialEnvs {
		if getenv(name) != "" {
			s.Next = name + " is set in the daemon's environment, but a turn doesn't receive it: store it as a secret and select it on the binding, or name it in the binding's pass_env"
			break
		}
	}
	return s
}

// names splits a comma-separated argument.
func names(arg string) []string {
	var out []string
	for _, n := range strings.Split(arg, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}
