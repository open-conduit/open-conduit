package bootstrap

import (
	"slices"
	"testing"
)

// An interactive launch adds variables to the session's environment but
// never sets one the host decides (#495): the toolbelt's namespace or the
// config path.
func TestLaunchEnv(t *testing.T) {
	env, err := launchEnv([]string{"PATH=/bin", "OS_TOKEN=leak", "OPEN_CONDUIT_CONFIG=/elsewhere"}, "/cfg.json", map[string]string{"RUNNER_PLAYBOOK": "/p.md"})
	if err != nil || !slices.Equal(env, []string{"PATH=/bin", "OPEN_CONDUIT_CONFIG=/cfg.json", "RUNNER_PLAYBOOK=/p.md"}) {
		t.Fatalf("env %q: %v", env, err)
	}
	for _, k := range []string{"OS_TOKEN", "OS_ENDPOINT", "OPEN_CONDUIT_CONFIG", "A=B", ""} {
		if _, err := launchEnv(nil, "/cfg.json", map[string]string{k: "x"}); err == nil {
			t.Errorf("a launch set %q", k)
		}
	}
}
