package integration_test

// The config file's siblings (#722): the secrets file, the last-good copy
// and the installed packages root are in the host's protected set, so a
// sandbox or container whose policy grants the whole config directory, or
// the operator's home, still can't read a stored secret or plant a package
// the daemon would start unconfined. Each test has a negative control: the
// same launch with the set as it was before (the state directory and the
// config file only) reaches them.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/srt"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
)

// configLayout is a config directory laid out as the daemon keeps it, in
// the operator's home: the config file, its last-good copy, the secrets
// file and an installed package.
type configLayout struct{ dir, config, secrets, lastGood, extensions string }

func newConfigLayout(t *testing.T, home string) configLayout {
	t.Helper()
	dir := filepath.Join(home, ".config", "open-conduit")
	c := configLayout{dir: dir, config: filepath.Join(dir, "config.json"), secrets: filepath.Join(dir, "secrets.json"),
		lastGood: filepath.Join(dir, "config.json.last-good"), extensions: filepath.Join(dir, "extensions")}
	if err := os.MkdirAll(filepath.Join(c.extensions, "installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{c.config: "config-secret", c.secrets: "secrets-secret", c.lastGood: "last-good-secret",
		filepath.Join(c.extensions, "installed", "manifest.json"): "installed"} {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// configProbes print "name: ALLOWED|DENIED" for each attempt on the
// config directory from inside.
func configProbes(c configLayout) string {
	return `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
read() { [ "$(cat "$1" 2>/dev/null)" = "$2" ]; }
export CFG='` + c.dir + `'
t cfg_read_config read "$CFG/config.json" config-secret
t cfg_read_secrets read "$CFG/secrets.json" secrets-secret
t cfg_read_last_good read "$CFG/config.json.last-good" last-good-secret
t cfg_write_secrets sh -c 'echo x > "$CFG/secrets.json"'
t cfg_write_last_good sh -c 'echo x > "$CFG/config.json.last-good"'
t cfg_plant_package sh -c 'mkdir "$CFG/extensions/planted" && echo x > "$CFG/extensions/planted/manifest.json"'
t cfg_plant_file sh -c 'echo x > "$CFG/extensions/planted.json"'
t cfg_write_package sh -c 'echo x >> "$CFG/extensions/installed/manifest.json"'
t cfg_write_granted sh -c 'echo x > "$CFG/granted"'
`
}

// legacyProtected is the protected set before #722, for the negative
// controls.
func legacyProtected(state, config string) []string { return []string{state, config} }

func TestSandboxProtectsTheConfigsSiblings(t *testing.T) {
	for _, grant := range []string{"dir", "home"} {
		for _, legacy := range []bool{false, true} {
			name := grant
			if legacy {
				name += "/negative_control"
			}
			t.Run(name, func(t *testing.T) {
				l := newSandboxLab(t, "shell")
				c := newConfigLayout(t, l.home)
				l.config = c.config
				if legacy {
					l.protected = legacyProtected(l.state, l.config)
				}
				path := c.dir
				if grant == "home" {
					path = "~"
				}
				policy := `{"write":["{workspace}","` + path + `"],"sockets":["{toolbelt}"],"unix_sockets":"all"}`
				var saved domain.Payload
				h, err := l.shell(t).Run(context.Background(), l.shellRequest("exec", configProbes(c), l.binding(policy, "", srt.Options{}), &saved))
				if err != nil {
					t.Fatal(err)
				}
				res := waitResult(t, h)
				out, _ := os.ReadFile(l.execOutput(t))
				if res.Outcome.Kind != domain.OutcomeCompleted {
					t.Fatalf("outcome %s %q\n%s", res.Outcome.Kind, res.Outcome.Summary, out)
				}
				got := probeResults(string(out))
				// The grant is live: a file of the run's own in the config
				// directory lands on the host, and the config file is denied
				// either way.
				if got["cfg_write_granted"] != "ALLOWED" || got["cfg_read_config"] != "DENIED" {
					t.Fatalf("the grant isn't as written: granted %q, config %q\n%s", got["cfg_write_granted"], got["cfg_read_config"], out)
				}
				if _, err := os.Stat(filepath.Join(c.dir, "granted")); err != nil {
					t.Fatalf("the granted write did not reach the host: %v", err)
				}
				reached := func() []string {
					var r []string
					probes := []string{"cfg_read_secrets", "cfg_read_last_good"}
					// On Linux a denied write lands in a throwaway tmpfs or on
					// a /dev/null bind and looks like it succeeded from inside,
					// so only macOS's answer is a verdict; the host's files
					// decide on both.
					if runtime.GOOS == "darwin" {
						probes = append(probes, "cfg_write_secrets", "cfg_write_last_good", "cfg_plant_package", "cfg_plant_file", "cfg_write_package")
					}
					for _, k := range probes {
						if got[k] != "DENIED" {
							r = append(r, k)
						}
					}
					for _, p := range []string{filepath.Join(c.extensions, "planted"), filepath.Join(c.extensions, "planted.json")} {
						if _, err := os.Lstat(p); err == nil {
							r = append(r, p+" on the host")
						}
					}
					for path, body := range map[string]string{c.secrets: "secrets-secret\n", c.lastGood: "last-good-secret\n", filepath.Join(c.extensions, "installed", "manifest.json"): "installed\n"} {
						if b, _ := os.ReadFile(path); string(b) != body {
							r = append(r, path+" changed on the host")
						}
					}
					return r
				}()
				if legacy {
					if len(reached) == 0 {
						t.Fatalf("without the config's siblings protected, the run reached none of them: the test can't tell\n%s", out)
					}
					return
				}
				if len(reached) > 0 {
					t.Fatalf("the run reached %s\n%s", strings.Join(reached, ", "), out)
				}
			})
		}
	}
}

// refusingContainers is a container provider the host never needs to
// reach: a launch the host refuses never gets to Plan, and one that gets
// there is reported as reached.
type refusingContainers struct{ planned bool }

var errReachedProvider = errors.New("the launch reached the provider")

func (p *refusingContainers) Plan(context.Context, extension.ResourceOwner, extension.ContainerPolicy, extension.SandboxNeeds, extension.SandboxLaunch) (extension.ContainerPlan, error) {
	p.planned = true
	return extension.ContainerPlan{}, errReachedProvider
}
func (p *refusingContainers) Create(context.Context, extension.ContainerPlan, map[string]string) (extension.ContainerCreated, error) {
	return extension.ContainerCreated{}, errReachedProvider
}
func (p *refusingContainers) Signal(context.Context, extension.ContainerRef, string) error {
	return errReachedProvider
}
func (p *refusingContainers) Stop(context.Context, extension.ContainerRef, time.Duration) error {
	return errReachedProvider
}
func (p *refusingContainers) Inspect(context.Context, extension.ContainerRef) (extension.ContainerState, error) {
	return extension.ContainerState{}, errReachedProvider
}
func (p *refusingContainers) List(context.Context, extension.ResourceOwner) ([]extension.ContainerState, error) {
	return nil, nil
}
func (p *refusingContainers) Remove(context.Context, extension.ContainerRef) error {
	return errReachedProvider
}

// A container's grant of the config directory, the operator's home, the
// packages root or the secrets file is refused before the provider plans
// anything, so nothing is mounted; with the set as it was, a grant of the
// packages root or the secrets file reaches the provider.
func TestContainerProtectsTheConfigsSiblings(t *testing.T) {
	root, err := os.MkdirTemp("", "occfg")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home, state := filepath.Join(root, "home"), filepath.Join(root, "state")
	c := newConfigLayout(t, home)
	ws := filepath.Join(state, "resources", "git", "workspaces", "mine")
	runRoot := filepath.Join(state, "executors", "shell", "runs")
	for _, d := range []string{ws + "/.git/hooks", ws + "/.git/info", runRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(ws+"/.git/config", nil, 0o600)
	launch := func(t *testing.T, protected []string, grant string) (bool, error) {
		t.Helper()
		p := &refusingContainers{}
		cfg := `{"image":"` + dockerTestImage + `","write":["{workspace}","` + grant + `"]}`
		b := extension.BindContainer(extension.ContainerBinding{
			Provider:  p,
			Owner:     extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: "exec"},
			Config:    domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(cfg)},
			Host:      "lab",
			Segment:   1,
			Protected: protected,
			RunRoot:   runRoot,
			Records:   filepath.Join(state, "runs", "own", "container"),
			Home:      home,
			Pin:       func(extension.ContainerPin) error { return nil },
		})
		res, err := b.Resources(dockerWorkspace{ws}).Acquire(context.Background(), extension.ResourceRequest{Sandboxed: true})
		if err != nil {
			t.Fatal(err)
		}
		run := filepath.Join(runRoot, "seg")
		_ = os.MkdirAll(run, 0o700)
		_, err = b.Launch(context.Background(), extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"/bin/sh"}, Dir: res.Dir, RunDir: run})
		return p.planned, err
	}
	for _, grant := range []string{c.dir, "~", "~/.config", c.extensions, filepath.Join(c.extensions, "installed"), c.secrets, c.lastGood} {
		t.Run(strings.ReplaceAll(strings.TrimPrefix(grant, root), "/", "_"), func(t *testing.T) {
			planned, err := launch(t, bootstrap.ProtectedPaths(state, "", c.config), grant)
			if !errors.Is(err, extension.ErrSandboxDenied) || planned {
				t.Fatalf("a grant of %s: %v (planned %v), want sandbox_denied before the provider plans", grant, err, planned)
			}
		})
	}
	// The negative control: the set without the config's siblings lets a
	// grant of each through to the provider.
	for _, grant := range []string{c.extensions, filepath.Join(c.extensions, "installed"), c.secrets, c.lastGood} {
		t.Run("negative_control"+strings.ReplaceAll(strings.TrimPrefix(grant, root), "/", "_"), func(t *testing.T) {
			planned, err := launch(t, legacyProtected(state, c.config), grant)
			if !planned || !errors.Is(err, errReachedProvider) {
				t.Fatalf("a grant of %s with the old set: %v (planned %v), want it to reach the provider", grant, err, planned)
			}
		})
	}
}
