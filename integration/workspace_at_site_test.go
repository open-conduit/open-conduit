package integration_test

// A Work's workspace on the machine its run executes on (#726), end to
// end through the providers the host composes: the git provider makes
// the workspace at the placement the SSH provider gives, the SSH provider
// runs the segment there, and the Work's next attempt, placed on another
// machine, makes it again there from the repository and resumes the
// session. The two machines are fake remotes on this machine, each with a
// root of its own that every path on it sits under, so neither shares a
// file with the host or with the other.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/extensions/git"
	"github.com/open-conduit/open-conduit/extensions/ssh"
)

// siteMachine is a fake remote: its ssh runs each command on this machine
// in a home of its own, and the provider puts every path it names under
// root.
type siteMachine struct {
	root     string
	settings ssh.Settings
	p        *ssh.Provider
	placer   rootedPlacer
}

// rootedPlacer is the SSH provider of a fake remote. The provider puts its
// own paths (a command's directory, a run's files) under the remote's
// root, but a site command's arguments are the resource provider's, which
// name paths at the site as they are on a real machine: a path in the
// host's state directory is moved under the root here, as the machine
// would hold it.
type rootedPlacer struct {
	*ssh.Provider
	root, state string
}

func (r rootedPlacer) SiteRun(ctx context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	argv := append([]string(nil), cmd.Argv...)
	for i, a := range argv {
		if a == r.state || strings.HasPrefix(a, r.state+"/") {
			argv[i] = r.root + a
		}
	}
	cmd.Argv = argv
	return r.Provider.SiteRun(ctx, placement, cmd)
}

func newSiteMachine(t *testing.T, tmp, name, state string) *siteMachine {
	t.Helper()
	m := &siteMachine{root: filepath.Join(tmp, name, "root")}
	home, bin := filepath.Join(tmp, name, "home"), filepath.Join(tmp, name, "bin")
	for _, d := range []string{m.root, home, bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := bin + ":/usr/bin:/bin:/usr/sbin:/sbin"
	writeFile(t, filepath.Join(bin, "ssh"), `#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -o|-p|-i|-l|-R|-F) shift 2;;
    -t|-T) shift;;
    *) break;;
  esac
done
shift
cd `+shQuote(home)+` || exit 255
export HOME=`+shQuote(home)+` PATH=`+shQuote(path)+`
exec /bin/sh -c "$*"
`, 0o700)
	if _, err := exec.LookPath("setsid"); err != nil {
		// macOS has no setsid(1); the remote's is util-linux's.
		writeFile(t, filepath.Join(bin, "setsid"), `#!/bin/sh
exec perl -e 'use POSIX qw(setsid); setsid() or die "setsid: $!"; exec @ARGV or die "exec: $!"' "$@"
`, 0o700)
	}
	key := make([]byte, 51)
	_, _ = rand.Read(key)
	m.settings = ssh.Settings{Host: name + ".test", User: "oc", Port: 22, HostKey: "ssh-ed25519 " + base64.StdEncoding.EncodeToString(key), Network: ssh.NetworkHost}
	m.p = ssh.New(ssh.Options{SSH: filepath.Join(bin, "ssh"), Helper: []string{"open-conduit", "remote", "run"}, Host: "host-a", Dir: filepath.Join(tmp, name, "provider"), Root: m.root,
		Remotes: func() []ssh.Settings { return []ssh.Settings{m.settings} }, Environ: func() []string { return []string{"PATH=" + path, "HOME=" + tmp} }})
	m.placer = rootedPlacer{Provider: m.p, root: m.root, state: state}
	return m
}

// config is a container binding's config running on the machine.
func (m *siteMachine) config(t *testing.T) domain.Payload {
	t.Helper()
	digest, err := ssh.HostKeyDigest(m.settings.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := json.Marshal(m.settings)
	return domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(`{"image":"` + m.settings.Host + `@sha256:` + digest + `","write":["{workspace}"],"provider":` + string(provider) + `}`)}
}

func (m *siteMachine) site() string { return extension.SiteID(ssh.Type, m.settings.Endpoint()) }

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// dirSize is the bytes of the regular files beneath dir.
func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

// siteHost is what the daemon holds for one Work: the git provider, with
// no git of its own and so no way to touch the repository on the host,
// the Work's home and the executor's runs.
type siteHost struct {
	t                             *testing.T
	state, home, runRoot, records string
	git                           *git.Provider
	machines                      map[string]*siteMachine
	pins                          []extension.ContainerPin
	resource                      domain.Payload
}

// segment binds one attempt's container on machine m, preferring prefer,
// as the host does (bootstrap's bindContainer), with the git resource
// bound again at the placement.
func (h *siteHost) bind(m *siteMachine, execution domain.ID, prefer string, attach *extension.Resource) *extension.BoundContainer {
	owner := extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: execution}
	return extension.BindContainer(extension.ContainerBinding{Provider: m.placer, Name: ssh.Type, Owner: owner, Config: m.config(h.t), Host: "host-a", Segment: 1,
		Protected: []string{h.state}, RunRoot: h.runRoot, Records: filepath.Join(h.records, string(execution)), WorkHome: h.home,
		Prefer: prefer, Attach: attach != nil, Workspace: attach,
		Pin: func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil },
		AtSite: func(site extension.Site) extension.ResourceBroker {
			return extension.BindResourcesAt(h.git, owner, h.resource, nil, site)
		}})
}

// run launches script in the workspace through b and runs the executor's
// wrapper, as a segment does; the script must succeed.
func (h *siteHost) run(b *extension.BoundContainer, dir, script string) {
	h.t.Helper()
	run, err := os.MkdirTemp(h.runRoot, "run-")
	if err != nil {
		h.t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@t", "GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@t"}
	plan, err := b.Launch(context.Background(), extension.SandboxNeeds{Write: []string{dir, h.home}}, extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", script}, Dir: dir, RunDir: run, Env: env})
	if err != nil {
		h.t.Fatal(err)
	}
	if plan.Dir != run {
		h.t.Fatalf("the wrapper runs in %q, want the run directory: the workspace isn't on the host", plan.Dir)
	}
	if len(plan.Argv) < 3 || plan.Argv[1] != "remote" || plan.Argv[2] != "run" {
		h.t.Fatalf("argv %q is not the remote run helper", plan.Argv)
	}
	var out bytes.Buffer
	if code := ssh.RunHelper(plan.Argv[3:], ssh.HelperOptions{Stdin: bytes.NewReader(nil), Stdout: &out, Stderr: &out, Interval: time.Millisecond}); code != 0 {
		h.t.Fatalf("the run exited %d: %s", code, out.String())
	}
}

// TestWorkspaceAtSiteEndToEnd covers the issue's checks with the
// providers the host composes:
//
//   - the run starts with no checkout of the repository on the host, and
//     nothing of the repository is uploaded: the host has no workspace;
//   - the agent commits and pushes from the machine;
//   - a second attempt, placed on another machine, gets the Work's branch
//     there, and its session resumes: the transcript is where Claude Code
//     looks for it, since the workspace's path is the same on both;
//   - the host's disk doesn't grow with the repository;
//   - an operator's attach runs in the workspace on the second machine;
//   - when the Work completes, reconcile removes the clone there.
func TestWorkspaceAtSiteEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	ctx := context.Background()

	// The repository, with a megabyte the host must never hold.
	origin, seed := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "seed")
	gitIn(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, tmp, "clone", "-q", origin, seed)
	gitIn(t, seed, "checkout", "-q", "-B", "main")
	big := make([]byte, 1<<20)
	_, _ = rand.Read(big)
	writeFile(t, filepath.Join(seed, "big.bin"), string(big), 0o644)
	writeFile(t, filepath.Join(seed, "README"), "hello\n", 0o644)
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-qm", "seed")
	gitIn(t, seed, "push", "-q", "origin", "main")

	state := filepath.Join(tmp, "state")
	h := &siteHost{t: t, state: state, home: filepath.Join(state, "homes", "k"), runRoot: filepath.Join(state, "executors", "claude-code", "runs"),
		records: filepath.Join(state, "runs"), machines: map[string]*siteMachine{}}
	for _, d := range []string{h.home, h.runRoot, h.records} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]string{"repository": "file://" + origin})
	h.resource = domain.Payload{Schema: "core.config.resource.v1", Data: raw}
	if !git.NeedsSite(raw) {
		t.Fatal("a repository resource should need a site")
	}
	p, err := git.New(git.Options{StateDir: filepath.Join(state, "resources", "git"), Git: filepath.Join(tmp, "no-git-on-the-host")})
	if err != nil {
		t.Fatal(err)
	}
	h.git = p
	a, b := newSiteMachine(t, tmp, "runner-a", state), newSiteMachine(t, tmp, "runner-b", state)
	h.machines[a.site()], h.machines[b.site()] = a, b
	// The host resolves the sites it placed the git resource at.
	p.UseSites(func(_ context.Context, id string) (extension.Site, error) {
		m := h.machines[id]
		if m == nil {
			return nil, extension.ErrSiteUnavailable
		}
		return extension.PlacedSite{Provider: ssh.Type, Placer: m.placer, At: extension.Placement{ID: m.settings.Endpoint(), Daemon: m.settings.Daemon()}}, nil
	})
	owner := func(x domain.ID) extension.ResourceOwner {
		return extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: x}
	}

	// The first attempt, on runner-a.
	c1 := h.bind(a, "x1", "", nil)
	res, err := c1.Resources(extension.BindResources(p, owner("x1"), h.resource)).Acquire(ctx, extension.ResourceRequest{Mode: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Site != a.site() || !strings.HasPrefix(res.Dir, state+"/") {
		t.Fatalf("resource %+v, want a workspace at %s under the lease's path", res, a.site())
	}
	if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
		out, _ := exec.Command("find", res.Dir).CombinedOutput()
		t.Fatalf("the workspace is on the host: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(a.root+res.Dir, "README"))); got != "hello" {
		t.Fatalf("runner-a's clone has README %q", got)
	}
	session := "11111111-2222-3333-4444-555555555555"
	transcript := claudecode.TranscriptPath(h.home, res.Dir, session)
	// The agent edits, commits and pushes on runner-a, and its session's
	// transcript is written in the Work's home there, where claude keeps
	// it for the workspace's path.
	h.run(c1, res.Dir, `set -e
echo change > change.txt
git add change.txt
git commit -qm "the agent's change"
git push -q origin HEAD
mkdir -p `+shQuote(filepath.Dir(a.root+transcript))+`
echo '{"type":"user"}' > `+shQuote(a.root+transcript))
	if len(h.pins) != 1 || h.pins[0].Site != a.site() || h.pins[0].Workspace != res.Dir || h.pins[0].Daemon != a.settings.Daemon() {
		t.Fatalf("pins %+v, want the segment pinned on runner-a with its workspace there", h.pins)
	}
	branch := gitIn(t, a.root+res.Dir, "symbolic-ref", "--short", "HEAD")
	if got := gitIn(t, tmp, "--git-dir", origin, "log", "-1", "--format=%s", branch); got != "the agent's change" {
		t.Fatalf("the repository's %s is at %q, want the agent's push", branch, got)
	}
	// The session came back with the Work's home: the host decides resume
	// from it, as it does for a workspace on the host.
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("the transcript isn't in the Work's home on the host: %v", err)
	}
	if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
		t.Fatalf("the run left the workspace on the host: %v", err)
	}

	// runner-a is no longer the binding's: the second attempt is placed on
	// runner-b, which has never held the Work.
	c2 := h.bind(b, "x2", a.settings.Endpoint(), nil)
	res2, err := c2.Resources(extension.BindResources(p, owner("x2"), h.resource)).Acquire(ctx, extension.ResourceRequest{Mode: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Site != b.site() || res2.Dir != res.Dir {
		t.Fatalf("resource %+v, want the same workspace path at %s", res2, b.site())
	}
	if got := gitIn(t, b.root+res2.Dir, "log", "-1", "--format=%s"); got != "the agent's change" {
		t.Fatalf("runner-b's clone is at %q, want the Work's branch with the pushed change", got)
	}
	// The clone on runner-a held nothing the repository lacks, so it went.
	if _, err := os.Stat(a.root + res.Dir); !os.IsNotExist(err) {
		t.Fatalf("runner-a's clone is still there: %v", err)
	}
	// The second attempt resumes: Claude Code finds the session for the
	// workspace's path, and the run finds it on runner-b.
	if _, err := os.Stat(claudecode.TranscriptPath(h.home, res2.Dir, session)); err != nil {
		t.Fatalf("no transcript to resume for the workspace on runner-b: %v", err)
	}
	h.run(c2, res2.Dir, `set -e
test -f `+shQuote(b.root+transcript)+`
test "$(cat change.txt)" = change
echo resumed >> `+shQuote(b.root+transcript))
	if !strings.Contains(readFile(t, transcript), "resumed") {
		t.Fatal("the resumed session's transcript didn't come back to the host")
	}

	// An operator attaches: in the workspace on runner-b, from its pin.
	last := h.pins[len(h.pins)-1]
	if last.Site != b.site() {
		t.Fatalf("the last pin %+v, want runner-b", last)
	}
	// The host's step: the attach's placement and workspace from the pin.
	prefer, ws, err := extension.AttachWorkspace(&last, ssh.Type, a.settings.Endpoint())
	if err != nil || prefer != b.settings.Endpoint() || ws == nil {
		t.Fatalf("attach at %q in %+v, %v; want runner-b's workspace", prefer, ws, err)
	}
	attach := h.bind(b, "x2", prefer, ws)
	h.run(attach, last.Workspace, `test "$(git log -1 --format=%s)" = "the agent's change"`)
	// Placed elsewhere, the attach is refused.
	moved := h.bind(a, "x2", prefer, ws)
	runDir, _ := os.MkdirTemp(h.runRoot, "run-")
	if _, err := moved.Launch(ctx, extension.SandboxNeeds{Write: []string{last.Workspace}}, extension.SandboxLaunch{Argv: []string{"true"}, Dir: last.Workspace, RunDir: runDir}); err == nil {
		t.Fatal("an attach placed away from the workspace's site was launched")
	}

	// The host held none of the repository: no workspace, and less than
	// the repository's size in all its state.
	if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
		t.Fatalf("the workspace is on the host: %v", err)
	}
	if n := dirSize(t, state); n >= int64(len(big)) {
		t.Fatalf("the host's state holds %d bytes, as much as the repository", n)
	}

	// The Work completes: reconcile removes the clone on runner-b, which
	// holds nothing unpushed.
	report, err := p.Reconcile(ctx, func(context.Context, extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
		return extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true, WorkDone: true, WorkSucceeded: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.root + res2.Dir); !os.IsNotExist(err) {
		t.Fatalf("runner-b's clone is still there after the Work completed (%+v): %v", report, err)
	}
}
