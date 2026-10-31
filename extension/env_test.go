package extension_test

import (
	"maps"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// TestApplyEnv: a layer replaces earlier variables, never supplies a
// toolbelt variable, and adds its git config entries after the ones
// already there.
func TestApplyEnv(t *testing.T) {
	vars := map[string]string{"PATH": "/bin", "MODE": "base"}
	extension.ApplyEnv(vars, map[string]string{"MODE": "workspace", "OS_TOKEN": "forged", "bad-name": "x", "NUL": "a\x00b",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false"})
	extension.ApplyEnv(vars, map[string]string{"GIT_AUTHOR_NAME": "bot",
		"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "", "GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!os git-credential"})
	extension.ApplyEnv(vars, nil)
	want := map[string]string{"PATH": "/bin", "MODE": "workspace", "GIT_AUTHOR_NAME": "bot",
		"GIT_CONFIG_COUNT": "3", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "", "GIT_CONFIG_KEY_2": "credential.helper", "GIT_CONFIG_VALUE_2": "!os git-credential"}
	if !maps.Equal(vars, want) {
		t.Errorf("env %v, want %v", vars, want)
	}
}
