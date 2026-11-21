package bootstrap

import (
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/internal/management"
)

// A terminal never inherits the daemon's credentials, and only a container
// provider's command gets the variables that reach its daemon.
func TestTerminalEnviron(t *testing.T) {
	host := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "ANTHROPIC_API_KEY=secret", "GITHUB_TOKEN=secret", "OS_TOKEN=secret", "OPEN_CONDUIT_TOKEN=secret", "DOCKER_HOST=unix:///d", "SSH_AUTH_SOCK=/a", "TERM=dumb"}
	got := terminalEnviron(management.TerminalSpec{Term: "xterm"}, host)
	if want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "TERM=xterm"}; !slices.Equal(got, want) {
		t.Fatalf("host attach: %v", got)
	}
	got = terminalEnviron(management.TerminalSpec{Term: "xterm", Contained: true, Env: []string{"FROM=provider"}}, host)
	if want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "DOCKER_HOST=unix:///d", "SSH_AUTH_SOCK=/a", "FROM=provider", "TERM=xterm"}; !slices.Equal(got, want) {
		t.Fatalf("container attach: %v", got)
	}
}
