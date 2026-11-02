package claudecode

import (
	"context"
	"os"
	"os/exec"

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
	// Login is "subscription" (a Claude login) or "api_key"
	// (ANTHROPIC_API_KEY in the daemon's environment).
	Login string `json:"login,omitempty"`
	Next  string `json:"next,omitempty"`
}

func (p Probes) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{{Name: "status", Summary: "whether claude is on the daemon's PATH, its version, and whether it is signed in and how",
		Run: func(ctx context.Context, _ map[string]string) (extension.ProbeResult, error) {
			return extension.ProbeResult{Command: "claude --version", Output: p.status(ctx)}, nil
		}}}
}

func (p Probes) status(ctx context.Context) InstallStatus {
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
	switch {
	case func() bool { _, err := login(ctx, ""); return err == nil }():
		s.SignedIn, s.Login = true, "subscription"
	case getenv("ANTHROPIC_API_KEY") != "":
		s.SignedIn, s.Login = true, "api_key"
	default:
		s.Next = "run claude once in a terminal on this machine and sign in"
	}
	return s
}
