package ssh

import (
	"strings"
	"testing"
)

// TestAuthHint: a remote's refusal of the client's keys names the package
// grant when the provider has no ssh agent, and only then.
func TestAuthHint(t *testing.T) {
	refused := "user@runner.test: Permission denied (publickey)."
	if h := authHint(refused, []string{"PATH=/bin"}); !strings.Contains(h, "settings.extensions.ssh.pass_env") {
		t.Fatalf("no agent: %q", h)
	}
	if h := authHint(refused, []string{"SSH_AUTH_SOCK=/tmp/agent"}); h != "" {
		t.Fatalf("with an agent: %q", h)
	}
	if h := authHint("Connection refused", nil); h != "" {
		t.Fatalf("another failure: %q", h)
	}
}
