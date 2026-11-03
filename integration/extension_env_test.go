package integration_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// envPackageCommand is the command of a package this test binary serves
// (TestMain): it records its process environment in env.txt in its
// directory, where the host starts it, and serves the protocol. A package's
// environment is sanitized, so the command's name is what selects it.
const envPackageCommand = "envpkg-extension"

func envPackage() *sdk.Extension {
	return sdk.New("envpkg", sdk.Version("1.0.0"))
}

func envPackageMain() int {
	if err := os.WriteFile("env.txt", []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		return 1
	}
	if err := sdk.Serve(envPackage()); err != nil {
		return 1
	}
	return 0
}

// recordedEnv is the environment the package last started with.
func recordedEnv(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(raw), "\n")
}

// TestPackageProcessEnvironment: the operator's grant
// (settings.extensions.<name>.pass_env, given to the host as
// Options.PassEnv) adds the daemon variables it names to the package's own
// process environment, on top of the sanitized baseline, and never a
// reserved one. An ungranted variable doesn't reach it (the negative
// control), and a changed grant is taken at the package's next start.
func TestPackageProcessEnvironment(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := newExtEnv(t)
	m := envPackage().Manifest
	m.Command = []string{envPackageCommand}
	f := e.install(m, self)
	environ := []string{"PATH=" + os.Getenv("PATH"), "SSH_AUTH_SOCK=/agent.sock", "DOCKER_HOST=tcp://d", "OS_TOKEN=secret", "GITHUB_TOKEN=secret"}

	// Negative control: with no grant, none of the daemon's variables
	// beyond the baseline reach the package.
	o := e.options(m.Name)
	o.Environ, o.Configure = environ, func() protocol.ConfigureParams { return protocol.ConfigureParams{} }
	plain := e.load(f, e.deps(), o)
	env := recordedEnv(t, f.Dir)
	for _, kv := range env {
		if strings.HasPrefix(kv, "SSH_AUTH_SOCK=") || strings.HasPrefix(kv, "DOCKER_HOST=") || strings.HasPrefix(kv, "OS_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Fatalf("ungranted %s reached the package", kv)
		}
	}
	if got := plain.Process().PassedEnv(); len(got) != 0 {
		t.Fatalf("passed %v without a grant", got)
	}
	_ = plain.Process().Close()

	var grant atomic.Pointer[[]string]
	grant.Store(&[]string{"SSH_AUTH_SOCK", "OS_TOKEN", "UNSET_VARIABLE"})
	o.PassEnv = func() []string { return *grant.Load() }
	r := e.load(f, e.deps(), o)
	env = recordedEnv(t, f.Dir)
	if !slices.Contains(env, "SSH_AUTH_SOCK=/agent.sock") {
		t.Fatalf("the granted variable did not reach the package: %v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_HOST=") || strings.HasPrefix(kv, "OS_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "UNSET_VARIABLE=") {
			t.Fatalf("%s reached the package without a valid grant", kv)
		}
	}
	if got := r.Process().PassedEnv(); !slices.Equal(got, []string{"SSH_AUTH_SOCK"}) {
		t.Fatalf("passed %v, want SSH_AUTH_SOCK", got)
	}

	// A changed grant reaches the package at its next start, here a
	// restart after a crash.
	grant.Store(&[]string{"DOCKER_HOST"})
	crash(t, r)
	env = recordedEnv(t, f.Dir)
	if !slices.Contains(env, "DOCKER_HOST=tcp://d") || slices.Contains(env, "SSH_AUTH_SOCK=/agent.sock") {
		t.Fatalf("the restart did not take the current grant: %v", env)
	}
	if got := r.Process().PassedEnv(); !slices.Equal(got, []string{"DOCKER_HOST"}) {
		t.Fatalf("passed %v after the restart, want DOCKER_HOST", got)
	}
}
