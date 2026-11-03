package bootstrap

import (
	"maps"
	"testing"
)

// TestResourceEnv: a resource's pass_env resolves to the host's values of
// the names it has set, never a reserved one.
func TestResourceEnv(t *testing.T) {
	env := map[string]string{"SSH_AUTH_SOCK": "/tmp/agent.sock", "OS_TOKEN": "t", "OPEN_CONDUIT_CONFIG": "/c"}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	got := resourceEnv([]string{"SSH_AUTH_SOCK", "HTTPS_PROXY", "OS_TOKEN", "OPEN_CONDUIT_CONFIG"}, lookup)
	if want := map[string]string{"SSH_AUTH_SOCK": "/tmp/agent.sock"}; !maps.Equal(got, want) {
		t.Fatalf("resourceEnv = %v, want %v", got, want)
	}
	if got := resourceEnv(nil, lookup); got != nil {
		t.Fatalf("no names: %v", got)
	}
}
