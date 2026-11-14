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
		"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "", "GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!other-helper"})
	extension.ApplyEnv(vars, nil)
	want := map[string]string{"PATH": "/bin", "MODE": "workspace", "GIT_AUTHOR_NAME": "bot",
		"GIT_CONFIG_COUNT": "3", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "", "GIT_CONFIG_KEY_2": "credential.helper", "GIT_CONFIG_VALUE_2": "!other-helper"}
	if !maps.Equal(vars, want) {
		t.Errorf("env %v, want %v", vars, want)
	}
}

// TestGitTransportLayer: the layer routes each host's https remotes
// through the toolbelt's git transport, whose remote helper is `os
// git-remote`, and merges after the git config already there.
func TestGitTransportLayer(t *testing.T) {
	vars := map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false"}
	extension.ApplyEnv(vars, extension.GitTransportLayer("github.com", "git.acme.test"))
	want := map[string]string{"GIT_CONFIG_COUNT": "5", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": "alias.remote-open-conduit", "GIT_CONFIG_VALUE_1": "!os git-remote",
		"GIT_CONFIG_KEY_2": "protocol.open-conduit.allow", "GIT_CONFIG_VALUE_2": "always",
		"GIT_CONFIG_KEY_3": "url.open-conduit::https://github.com/.insteadOf", "GIT_CONFIG_VALUE_3": "https://github.com/",
		"GIT_CONFIG_KEY_4": "url.open-conduit::https://git.acme.test/.insteadOf", "GIT_CONFIG_VALUE_4": "https://git.acme.test/"}
	if !maps.Equal(vars, want) {
		t.Errorf("env %v, want %v", vars, want)
	}
}
