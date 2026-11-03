package docker

import (
	"strings"
	"testing"
)

// TestEnvHint: a docker CLI that can't reach its daemon names the package
// grant for the variables it may lack out of process, and nothing once
// they're set.
func TestEnvHint(t *testing.T) {
	p := New(Options{Environ: func() []string { return []string{"PATH=/bin", "XDG_RUNTIME_DIR=/run/user/1"} }})
	if h := p.envHint(); !strings.Contains(h, "DOCKER_CONFIG") || strings.Contains(h, "XDG_RUNTIME_DIR") || !strings.Contains(h, "settings.extensions.docker.pass_env") {
		t.Fatalf("hint %q", h)
	}
	p = New(Options{Environ: func() []string { return []string{"DOCKER_CONFIG=/d", "XDG_RUNTIME_DIR=/r"} }})
	if h := p.envHint(); h != "" {
		t.Fatalf("with both set: %q", h)
	}
}
