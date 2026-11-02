package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// fakeRemote is a remote that is this machine: its ssh runs the command
// in a home of its own, and every mirrored path sits under root, so the
// host's paths and the remote's copies never meet.
type fakeRemote struct {
	t                  *testing.T
	root, home, log    string
	state, ws, run, tb string
	runRoot, records   string
	settings           Settings
	p                  *Provider
	pins               []extension.ContainerPin
	config             domain.Payload
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	f := &fakeRemote{t: t, root: filepath.Join(tmp, "remote"), home: filepath.Join(tmp, "remote-home"), log: filepath.Join(tmp, "ssh.log")}
	bin := filepath.Join(tmp, "bin")
	for _, d := range []string{f.root, f.home, bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := bin + ":/usr/bin:/bin:/usr/sbin:/sbin"
	fake := `#!/bin/sh
printf '%s\n' "$*" >> ` + quote(f.log) + `
while [ $# -gt 0 ]; do
  case $1 in
    -o|-p|-i|-l|-R|-F) shift 2;;
    -t|-T) shift;;
    *) break;;
  esac
done
shift
cd ` + quote(f.home) + ` || exit 255
export HOME=` + quote(f.home) + ` PATH=` + quote(path) + `
exec /bin/sh -c "$*"
`
	write(t, filepath.Join(bin, "ssh"), fake, 0o700)
	if _, err := exec.LookPath("setsid"); err != nil {
		// macOS has no setsid(1); the remote's is util-linux's.
		write(t, filepath.Join(bin, "setsid"), `#!/bin/sh
exec perl -e 'use POSIX qw(setsid); setsid() or die "setsid: $!"; exec @ARGV or die "exec: $!"' "$@"
`, 0o700)
	}
	key := make([]byte, 51)
	_, _ = rand.Read(key)
	f.settings = Settings{Host: "runner.test", User: "oc", Port: 22, HostKey: "ssh-ed25519 " + base64.StdEncoding.EncodeToString(key), Network: NetworkHost}
	f.state = filepath.Join(tmp, "state")
	f.runRoot = filepath.Join(f.state, "executors", "shell", "runs")
	f.run = filepath.Join(f.runRoot, "k", "n")
	f.ws = filepath.Join(f.state, "workspaces", "mine")
	f.tb = filepath.Join(f.state, "runs", "x", "toolbelt.sock")
	f.records = filepath.Join(f.state, "runs", "x", "container")
	for _, d := range []string{f.run, filepath.Join(f.ws, ".git"), filepath.Dir(f.tb)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(f.ws, "a.txt"), "alpha\n", 0o644)
	write(t, filepath.Join(f.ws, "b.txt"), "beta\n", 0o644)
	write(t, filepath.Join(f.ws, ".git", "config"), "[core]\n", 0o644)
	write(t, filepath.Join(filepath.Dir(f.tb), "token"), "toolbelt-token", 0o600)
	f.p = New(Options{SSH: filepath.Join(bin, "ssh"), Helper: "open-conduit", Host: "host-a", Dir: filepath.Join(tmp, "provider"), Root: f.root,
		Remotes: func() []Settings { return []Settings{f.settings} }, Environ: func() []string { return []string{"PATH=" + path, "HOME=" + tmp} }})
	f.config = f.configFor(f.settings)
	return f
}

func (f *fakeRemote) configFor(s Settings) domain.Payload {
	digest, err := HostKeyDigest(s.HostKey)
	if err != nil {
		f.t.Fatal(err)
	}
	provider, _ := json.Marshal(s)
	return domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(`{"image":"` + s.Host + `@sha256:` + digest + `","write":["{workspace}"],"provider":` + string(provider) + `}`)}
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeRemote) bind(pinned *extension.ContainerPin) *extension.BoundContainer {
	return extension.BindContainer(extension.ContainerBinding{Provider: f.p, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"},
		Config: f.config, Host: "host-a", Segment: 1, Protected: []string{f.state}, Shared: []string{filepath.Dir(f.tb)}, RunRoot: f.runRoot,
		Toolbelt: f.tb, Records: f.records, Pinned: pinned,
		Pin: func(p extension.ContainerPin) error { f.pins = append(f.pins, p); return nil }})
}

func (f *fakeRemote) needs() extension.SandboxNeeds {
	return extension.SandboxNeeds{Write: []string{f.ws}, ReadOnly: []string{filepath.Join(f.ws, ".git", "config")}, Sockets: []string{f.tb}}
}

func (f *fakeRemote) launch(script string) extension.SandboxLaunch {
	return extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", script}, Dir: f.ws, RunDir: f.run,
		Env: []string{"PATH=/usr/bin:/bin", "FOO=bar baz"}, Secrets: []string{"SECRET_TOKEN=s3'cret"}}
}

// helper runs the wrapper's argv as the executor would: the helper is
// this package's RunHelper.
func (f *fakeRemote) helper(argv []string, stdin *os.File) (int, string) {
	f.t.Helper()
	if len(argv) < 3 || argv[1] != "remote" || argv[2] != "run" {
		f.t.Fatalf("argv %q is not the remote run helper", argv)
	}
	var out, errs bytes.Buffer
	o := HelperOptions{Stdout: &out, Stderr: &errs, Interval: time.Millisecond}
	if stdin != nil {
		o.Stdin = stdin
	} else {
		o.Stdin = bytes.NewReader(nil)
	}
	code := RunHelper(argv[3:], o)
	return code, out.String() + errs.String()
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRunOnRemote(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	script := `cat a.txt > copy.txt; cp -p a.txt preserved.txt; rm b.txt; mkdir moved; mv a.txt moved/renamed.txt; echo planted >> .git/config; mkdir -p sub; echo "$FOO/$SECRET_TOKEN" > sub/env.txt; ln -s a.txt link; exit 3`
	plan, err := f.bind(nil).Launch(ctx, f.needs(), f.launch(script))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pins) != 1 || f.pins[0].Daemon != f.settings.Daemon() || !strings.HasPrefix(f.pins[0].ID, f.settings.Endpoint()+"/") {
		t.Fatalf("pins %+v, want the run pinned on the remote", f.pins)
	}
	if strings.Contains(string(plan.Data.Data), "s3") || strings.Contains(string(plan.Data.Data), "bar baz") {
		t.Errorf("the plan holds a value: %s", plan.Data.Data)
	}
	// The host's paths are copied to the remote, at the same paths; the
	// toolbelt's token is, and nothing else of the state directory.
	if got := read(t, f.root+f.ws+"/a.txt"); got != "alpha\n" {
		t.Errorf("the remote's a.txt is %q", got)
	}
	if got := read(t, f.root+filepath.Dir(f.tb)+"/token"); got != "toolbelt-token" {
		t.Errorf("the remote's toolbelt token is %q", got)
	}
	if entries, _ := os.ReadDir(f.root + f.state); len(entries) != 3 {
		t.Errorf("the remote holds %d entries of the state directory, want workspaces, runs and executors", len(entries))
	}
	st, err := f.p.Inspect(ctx, extension.ContainerRef{ID: f.pins[0].ID})
	if err != nil || st.Running || !st.StartedAt.IsZero() || st.Gone {
		t.Fatalf("a created run %+v, %v, want unstarted", st, err)
	}
	code, out := f.helper(plan.Argv, nil)
	if code != 3 {
		t.Fatalf("the helper exited %d (%s), want the run's 3", code, out)
	}
	// What the run changed comes back; a read-only need doesn't.
	if got := read(t, filepath.Join(f.ws, "copy.txt")); got != "alpha\n" {
		t.Errorf("copy.txt is %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.ws, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt, removed on the remote, is still here: %v", err)
	}
	// A rename and a copy keep their mtime; both still come back.
	for _, name := range []string{"moved/renamed.txt", "preserved.txt"} {
		if got := read(t, filepath.Join(f.ws, name)); got != "alpha\n" {
			t.Errorf("%s is %q", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(f.ws, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt, renamed on the remote, is still here: %v", err)
	}
	if got := read(t, filepath.Join(f.ws, ".git", "config")); got != "[core]\n" {
		t.Errorf(".git/config came back as %q; a read-only need never does", got)
	}
	if got := read(t, filepath.Join(f.ws, "sub", "env.txt")); got != "bar baz/s3'cret\n" {
		t.Errorf("the run saw %q, want its environment and secret", got)
	}
	if link, err := os.Readlink(filepath.Join(f.ws, "link")); err != nil || link != "a.txt" {
		t.Errorf("link is %q, %v", link, err)
	}
	if _, err := os.Stat(f.root + f.records); !os.IsNotExist(err) {
		t.Error("the host's records were copied to the remote")
	}
	entries, _ := filepath.Glob(filepath.Join(f.home, runsDir, "*", SecretsFile))
	if len(entries) != 0 {
		t.Errorf("the secrets file outlived the run's start: %v", entries)
	}
	if strings.Contains(read(t, f.log), "s3") {
		t.Error("a secret was on ssh's command line")
	}
	if !strings.Contains(read(t, f.log), "-R "+f.root+f.tb+":"+f.tb) {
		t.Error("the toolbelt socket was not forwarded")
	}
	// Recovery with the wrapper gone: the pinned run's exit stands.
	exit, err := f.bind(&f.pins[0]).Adopt(ctx, false)
	if err != nil || !exit.Exited || exit.ExitCode != 3 || exit.Interrupted {
		t.Fatalf("adopt %+v, %v, want the exit", exit, err)
	}
	if err := extension.RemoveContainers(ctx, f.p, extension.ResourceOwner{ExecutionID: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.root + f.ws); !os.IsNotExist(err) {
		t.Error("the remote's copy of the workspace outlived its run")
	}
	st, err = f.p.Inspect(ctx, extension.ContainerRef{ID: f.pins[0].ID})
	if err != nil || !st.Gone {
		t.Errorf("a removed run is %+v, %v", st, err)
	}
}

func TestStopAndRecover(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	// The run reads its stdin until it's stopped.
	plan, err := f.bind(nil).Launch(ctx, f.needs(), f.launch(`cat > out.txt`))
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() { c, o := f.helper(plan.Argv, r); done <- result{c, o} }()
	ref := extension.ContainerRef{ID: f.pins[0].ID}
	if _, err := w.WriteString("streamed\n"); err != nil {
		t.Fatal(err)
	}
	for {
		st, err := f.p.Inspect(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if st.Running {
			break
		}
		select {
		case res := <-done:
			t.Fatalf("the run ended before it was stopped: %d %s", res.code, res.out)
		default:
		}
	}
	// A live run whose wrapper is alive is adopted; without its wrapper
	// it isn't, and the host's stop ends it.
	if _, err := f.bind(&f.pins[0]).Adopt(ctx, true); err != nil {
		t.Fatalf("adopt a live run: %v", err)
	}
	if _, err := f.bind(&f.pins[0]).Adopt(ctx, false); err == nil {
		t.Fatal("a run without its wrapper was adopted")
	}
	if err := extension.StopContainer(ctx, f.p, &f.pins[0], f.records, 5*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	res := <-done
	if res.code != 143 {
		t.Errorf("the stopped run exited %d (%s), want TERM's 143", res.code, res.out)
	}
	if got := read(t, filepath.Join(f.ws, "out.txt")); got != "streamed\n" {
		t.Errorf("out.txt is %q, want the stdin the run read", got)
	}
	// The stub saw a TERM the host sent, and recorded: not unrequested.
	exit, err := f.bind(&f.pins[0]).Adopt(ctx, false)
	if err != nil || exit.Interrupted || exit.ExitCode != 143 {
		t.Errorf("adopt %+v, %v", exit, err)
	}
}

func TestPinnedHostKey(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	if _, err := f.bind(nil).Launch(ctx, f.needs(), f.launch(`exit 0`)); err != nil {
		t.Fatal(err)
	}
	pin := f.pins[0]
	// The binding now pins another key: the run can't be proved.
	key := make([]byte, 51)
	_, _ = rand.Read(key)
	f.settings.HostKey = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(key)
	f.config = f.configFor(f.settings)
	if _, err := f.bind(&pin).Adopt(ctx, true); err == nil {
		t.Fatal("a run on a remote pinned to another key was adopted")
	}
	if err := extension.StopContainer(ctx, f.p, &pin, f.records, 0); err == nil {
		t.Fatal("a run on a remote pinned to another key was confirmed stopped")
	}
	known, _ := filepath.Glob(filepath.Join(f.p.o.Dir, "known_hosts-*"))
	if len(known) == 0 || !strings.Contains(read(t, known[len(known)-1]), "open-conduit-") {
		t.Errorf("known hosts %v", known)
	}
	if !strings.Contains(read(t, f.log), "StrictHostKeyChecking=yes") {
		t.Error("ssh was not told to check the pinned key")
	}
}

func TestConformance(t *testing.T) {
	f := newFakeRemote(t)
	err := conformance.CheckContainerProvider(conformance.ContainerFixture{
		Provider: f.p,
		Host:     "host-a",
		Policy:   extension.ContainerPolicy{Config: f.config, Paths: extension.SandboxPaths{Workspace: f.ws, Run: f.run, Records: f.records}},
		Needs:    extension.SandboxNeeds{Write: []string{f.ws}},
		Launch:   extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", "exit 7"}, Dir: f.ws, RunDir: f.run, Env: []string{"PATH=/usr/bin:/bin"}},
		Code:     7,
		Start: func(c extension.ContainerCreated) int {
			code, _ := f.helper(c.Argv, nil)
			return code
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseConfig(t *testing.T) {
	f := newFakeRemote(t)
	if _, s, err := ParseConfig(f.config.Data); err != nil || s.Port != 22 {
		t.Fatalf("a good config: %+v, %v", s, err)
	}
	digest, _ := HostKeyDigest(f.settings.HostKey)
	for name, raw := range map[string]string{
		"an image that doesn't pin the key": `{"image":"runner.test@sha256:` + strings.Repeat("0", 64) + `","provider":{"host":"runner.test","host_key":"` + f.settings.HostKey + `","network":"host"}}`,
		"no network acknowledgement":        `{"image":"runner.test@sha256:` + digest + `","provider":{"host":"runner.test","host_key":"` + f.settings.HostKey + `"}}`,
		"limits":                            `{"image":"runner.test@sha256:` + digest + `","limits":{"cpus":2},"provider":{"host":"runner.test","host_key":"` + f.settings.HostKey + `","network":"host"}}`,
		"a bad host key":                    `{"image":"runner.test@sha256:` + digest + `","provider":{"host":"runner.test","host_key":"nope","network":"host"}}`,
		"an option for a host":              `{"image":"-oProxyCommand=x@sha256:` + digest + `","provider":{"host":"-oProxyCommand=x","host_key":"` + f.settings.HostKey + `","network":"host"}}`,
		"no settings":                       `{"image":"runner.test@sha256:` + digest + `"}`,
	} {
		if err := ValidateConfig(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	err := ValidateConfig(json.RawMessage(`{"image":"runner.test@sha256:` + strings.Repeat("0", 64) + `","provider":{"host":"runner.test","host_key":"` + f.settings.HostKey + `","network":"host"}}`))
	if err == nil || !strings.Contains(err.Error(), digest) {
		t.Errorf("the error %v doesn't give the image to set", err)
	}
}

func TestRunCantReplaceGit(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	write(t, filepath.Join(f.ws, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	script := `rm -rf .git; mkdir evil; printf '[core]\n\tfsmonitor = /tmp/pwn\n' > evil/config; ln -s evil .git`
	plan, err := f.bind(nil).Launch(ctx, f.needs(), f.launch(script))
	if err != nil {
		t.Fatal(err)
	}
	if code, out := f.helper(plan.Argv, nil); code != 0 {
		t.Fatalf("the helper exited %d: %s", code, out)
	}
	fi, err := os.Lstat(filepath.Join(f.ws, ".git"))
	if err != nil || !fi.IsDir() {
		t.Fatalf(".git is %v, %v: it must stay the host's directory", fi, err)
	}
	if got := read(t, filepath.Join(f.ws, ".git", "config")); got != "[core]\n" {
		t.Errorf(".git/config is %q", got)
	}
}

// replaceGit is an archive in which a run has replaced <ws>/.git with a
// link to a config of its own, and written a .git file beside it.
func replaceGit(ws string) *bytes.Buffer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entry := func(name string, typ byte, link, data string) {
		_ = tw.WriteHeader(&tar.Header{Name: "." + name, Typeflag: typ, Linkname: link, Size: int64(len(data)), Mode: 0o644, ModTime: time.Now()})
		_, _ = tw.Write([]byte(data))
	}
	entry(ws+"/evil", tar.TypeDir, "", "")
	entry(ws+"/evil/config", tar.TypeReg, "", "[core]\n\tfsmonitor = /tmp/pwn\n")
	entry(ws+"/.git", tar.TypeSymlink, ws+"/evil", "")
	entry(ws+"/sub/.git", tar.TypeReg, "", "gitdir: /tmp/elsewhere\n")
	_ = tw.Close()
	return &buf
}

func TestApplyKeepsProtectedParents(t *testing.T) {
	setup := func(t *testing.T) (string, []string) {
		tmp, _ := filepath.EvalSymlinks(t.TempDir())
		ws := filepath.Join(tmp, "ws")
		for _, d := range []string{filepath.Join(ws, ".git", "hooks"), filepath.Join(ws, "sub", ".git")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		write(t, filepath.Join(ws, ".git", "config"), "[core]\n", 0o644)
		write(t, filepath.Join(ws, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
		write(t, filepath.Join(ws, "sub", ".git", "config"), "[core]\n", 0o644)
		return ws, []string{filepath.Join(ws, ".git", "config"), filepath.Join(ws, ".git", "hooks"), filepath.Join(ws, "sub", ".git", "config")}
	}
	ws, keep := setup(t)
	listed := map[string]bool{ws + "/.git": true, ws + "/evil": true, ws + "/evil/config": true, ws + "/sub/.git": true}
	if err := Apply(replaceGit(ws), listed, map[string]Entry{}, []string{ws}, keep); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".git", "sub/.git"} {
		if fi, err := os.Lstat(filepath.Join(ws, dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s was replaced: %v %v", dir, fi, err)
		}
	}
	if got := read(t, filepath.Join(ws, ".git", "HEAD")); got != "ref: refs/heads/main\n" {
		t.Errorf(".git/HEAD is %q", got)
	}
	if got := read(t, filepath.Join(ws, ".git", "config")); got != "[core]\n" {
		t.Errorf(".git/config is %q", got)
	}
	// Negative control: with nothing kept, the same archive does replace
	// .git, so the case above is the one the pinning stops.
	ws, _ = setup(t)
	if err := Apply(replaceGit(ws), listed, map[string]Entry{}, []string{ws}, nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(ws, ".git")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("without keep, .git is %v, %v: the archive doesn't exercise the replacement", fi, err)
	}
}

func TestApplyStaysInTheGrant(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(tmp, "ws")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(ws, "kept"), "host", 0o644)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entry := func(name string, typ byte, link, data string) {
		_ = tw.WriteHeader(&tar.Header{Name: "." + name, Typeflag: typ, Linkname: link, Size: int64(len(data)), Mode: 0o4755, ModTime: time.Now()})
		_, _ = tw.Write([]byte(data))
	}
	entry(ws+"/escape", tar.TypeSymlink, outside, "")
	entry(ws+"/escape/evil", tar.TypeReg, "", "x")
	entry(outside+"/direct", tar.TypeReg, "", "x")
	entry(ws+"/../outside/dotdot", tar.TypeReg, "", "x")
	entry(ws+"/kept", tar.TypeReg, "", "remote")
	entry(ws+"/fifo", tar.TypeFifo, "", "")
	entry(ws+"/tool", tar.TypeReg, "", "#!/bin/sh\n")
	_ = tw.Close()
	if err := Apply(&buf, map[string]bool{}, map[string]Entry{}, []string{ws}, []string{filepath.Join(ws, "kept")}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("Apply wrote outside the grant: %v", entries)
	}
	if got := read(t, filepath.Join(ws, "kept")); got != "host" {
		t.Errorf("a kept path was written: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(ws, "escape")); err != nil || !fi.IsDir() {
		t.Errorf("the link a later entry wrote through wasn't replaced by a directory: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(ws, "tool")); err != nil || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("tool %v, %v: no set-ID bit comes back", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "fifo")); !os.IsNotExist(err) {
		t.Error("a FIFO came back")
	}
}
