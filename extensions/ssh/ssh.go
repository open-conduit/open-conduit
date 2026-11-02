// Package ssh is the container provider that runs a launch on another
// machine over SSH. It is a container in the contract's terms: created
// (the launch's granted paths copied to the remote at the same paths, its
// records and entry stub written) without starting, started by `open-conduit
// remote run`, the host helper the executor's wrapper runs, and stopped,
// inspected, listed and removed by the host. The helper reaches the
// execution's toolbelt back through a forward of its socket over the same
// SSH connection, relays signals, and on the run's exit copies what the
// run changed in its write grants back before exiting with its status.
//
// The remote is pinned by its host key, which the binding's image names,
// so the host never runs anything on a machine it can't prove is the one
// configured. The host's credentials never leave it: the remote gets the
// launch's own secrets, the execution's toolbelt token and, through the
// toolbelt, the scoped git token.
package ssh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const (
	// Type is the provider's name in a binding's container.
	Type = "ssh"
	// PlanSchema identifies the plan's Data, the effective policy.
	PlanSchema = "open-conduit.container.ssh.plan.v1"
	// SecretsFile is a launch's credentials file in its run records on the
	// remote, which the entry stub reads and removes.
	SecretsFile = "secrets"
	// healthTTL is how long a passing health check lets launches go ahead
	// without running it again.
	healthTTL = 10 * time.Minute
)

// Options are host-trusted, never from binding config.
type Options struct {
	// SSH is the ssh client, resolved on PATH when not absolute (default
	// "ssh").
	SSH string
	// Helper is the open-conduit binary whose hidden `remote run` starts a
	// run for an executor's wrapper.
	Helper string
	// Host is this daemon's ContainerLabelHost value.
	Host string
	// Dir is a host-owned directory for the provider's own files: the
	// known_hosts file each remote is pinned by.
	Dir string
	// Remotes are the settings of every remote the config's bindings use,
	// for a reference's remote and a listing of every run of the host.
	Remotes func() []Settings
	// Environ is the ssh client's environment (os.Environ when nil).
	Environ func() []string
	// Home expands ~ in the grant and the identity (the user's home when
	// empty).
	Home string
	// Root prefixes every path on the remote. It is empty but for a fake
	// remote that shares the host's filesystem.
	Root string
	Now  func() time.Time
}

// Provider implements extension.ContainerProvider.
type Provider struct {
	o      Options
	mu     sync.Mutex
	health map[string]time.Time
}

// New returns the provider.
func New(o Options) *Provider {
	if o.SSH == "" {
		o.SSH = "ssh"
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Remotes == nil {
		o.Remotes = func() []Settings { return nil }
	}
	return &Provider{o: o, health: map[string]time.Time{}}
}

// Plan is the plan's Data: everything the run is made from, except the
// environment's and secrets' values, which are in files in the records
// until Create reads them.
type Plan struct {
	Remote  Settings `json:"remote"`
	Daemon  string   `json:"daemon"`
	Workdir string   `json:"workdir"`
	Argv    []string `json:"argv"`
	EnvKeys []string `json:"env_keys"`
	EnvFile string   `json:"env_file"`
	// Write are copied to the remote and what the run changes in them
	// comes back; Read are only copied there. Skip are never copied, and
	// Keep never come back (a read-only need, what the host protects).
	Write   []string `json:"write"`
	Read    []string `json:"read,omitempty"`
	Skip    []string `json:"skip,omitempty"`
	Keep    []string `json:"keep,omitempty"`
	Records string   `json:"records"`
	Marker  string   `json:"marker"`
	// Toolbelt is the execution's toolbelt socket, forwarded to the same
	// path on the remote.
	Toolbelt   string   `json:"toolbelt,omitempty"`
	Secrets    string   `json:"secrets,omitempty"`
	SecretKeys []string `json:"secret_keys,omitempty"`
	Network    string   `json:"network"`
	TTY        bool     `json:"tty,omitempty"`
}

var secretKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p *Provider) Plan(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	cfg, s, err := ParseConfig(policy.Config.Data)
	if err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	paths := policy.Paths
	if !filepath.IsAbs(paths.Records) || !filepath.IsAbs(paths.Run) || !filepath.IsAbs(l.Dir) || owner.ExecutionID == "" || len(l.Argv) == 0 {
		return extension.ContainerPlan{}, fmt.Errorf("%w: incomplete launch", extension.ErrSandboxUnavailable)
	}
	// The only socket a run reaches is its toolbelt, forwarded back.
	for _, sock := range needs.Sockets {
		if paths.Toolbelt == "" || extension.CanonicalPath(sock) != extension.CanonicalPath(paths.Toolbelt) {
			return extension.ContainerPlan{}, fmt.Errorf("%w: the ssh provider forwards only the execution's toolbelt socket", extension.ErrSandboxUnavailable)
		}
	}
	for _, kv := range l.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envKey.MatchString(k) || strings.ContainsRune(kv, 0) {
			return extension.ContainerPlan{}, fmt.Errorf("%w: an environment entry can't be passed to a remote", extension.ErrSandboxUnavailable)
		}
	}
	var secretKeys []string
	for _, kv := range l.Secrets {
		k, v, _ := strings.Cut(kv, "=")
		if !secretKey.MatchString(k) || strings.ContainsRune(v, 0) {
			return extension.ContainerPlan{}, fmt.Errorf("%w: a secret can't be passed to a remote", extension.ErrSandboxUnavailable)
		}
		secretKeys = append(secretKeys, k)
	}
	sort.Strings(secretKeys)
	d, err := p.layout(cfg, paths, needs)
	if err != nil {
		return extension.ContainerPlan{}, err
	}
	if h := p.Health(ctx, s, extension.CanonicalPath(paths.Workspace), true); h.Status == "fail" {
		return extension.ContainerPlan{}, fmt.Errorf("%w: remote %s: %s", extension.ErrSandboxUnavailable, s.Endpoint(), h.Detail)
	}
	d.Remote, d.Daemon, d.Workdir, d.Argv, d.Records = s, s.Daemon(), l.Dir, l.Argv, paths.Records
	d.Marker = filepath.Join(paths.Run, extension.ContainerTermMarker)
	d.SecretKeys, d.Network, d.TTY = secretKeys, s.Network, l.TTY
	if len(needs.Sockets) > 0 {
		d.Toolbelt = extension.CanonicalPath(paths.Toolbelt)
	}
	for _, kv := range l.Env {
		k, _, _ := strings.Cut(kv, "=")
		d.EnvKeys = append(d.EnvKeys, k)
	}
	sort.Strings(d.EnvKeys)
	if err := os.MkdirAll(paths.Records, 0o700); err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	sum := sha256.Sum256([]byte(strings.Join(l.Env, "\n") + "\x00" + strings.Join(l.Argv, "\x00")))
	key := hex.EncodeToString(sum[:8])
	d.EnvFile = filepath.Join(paths.Records, "ssh-env-"+key)
	envJSON, _ := json.Marshal(l.Env)
	if err := writeFresh(d.EnvFile, envJSON); err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	if len(l.Secrets) > 0 {
		d.Secrets = filepath.Join(paths.Records, "ssh-secrets-"+key)
		if err := writeFresh(d.Secrets, []byte(secretExports(l.Secrets))); err != nil {
			return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
		}
	}
	data, _ := json.Marshal(d)
	summary := fmt.Sprintf("ssh %s: %d paths copied, %d copied back, the remote's own network", s.Endpoint(), len(d.Write)+len(d.Read), len(d.Write))
	if d.Toolbelt != "" {
		summary += ", toolbelt forwarded"
	}
	return extension.ContainerPlan{Image: cfg.Image, Summary: summary, Data: domain.Payload{Schema: PlanSchema, Data: data}}, nil
}

// layout is the grant as paths to copy: the write grant, the run
// directory and the Work's home copied there and back; the read grant,
// the executor's read needs and what the host shares only there. What the
// host denies inside them is never copied, and a read-only need inside a
// write grant never comes back.
func (p *Provider) layout(cfg extension.ContainerConfig, paths extension.SandboxPaths, needs extension.SandboxNeeds) (Plan, error) {
	expand := func(list []string) []string {
		var out []string
		for _, s := range list {
			s = strings.NewReplacer("{workspace}", paths.Workspace, "{run}", paths.Run).Replace(s)
			if s == "~" || strings.HasPrefix(s, "~/") {
				s = p.o.Home + s[1:]
			}
			out = append(out, extension.CanonicalPath(s))
		}
		return out
	}
	own := []string{extension.CanonicalPath(paths.Run)}
	if paths.Home != "" {
		own = append(own, extension.CanonicalPath(paths.Home))
	}
	var rw, ro []string
	for _, w := range append(expand(cfg.Write), own...) {
		if fi, err := os.Stat(w); err != nil || !fi.IsDir() && !fi.Mode().IsRegular() {
			return Plan{}, fmt.Errorf("%w: the write grant %s is not a file or directory here", extension.ErrSandboxUnavailable, w)
		}
		rw = append(rw, w)
	}
	for _, r := range append(expand(cfg.Read), needs.Read...) {
		r = extension.CanonicalPath(r)
		if _, err := os.Stat(r); err == nil {
			ro = append(ro, r)
		}
	}
	rw = outermost(rw)
	var read []string
	for _, r := range outermost(ro) {
		if !extension.Within(r, rw) {
			read = append(read, r)
		}
	}
	all := append(append([]string{}, rw...), read...)
	d := Plan{Write: rw, Read: read}
	// The host's records (this plan's environment and secrets files among
	// them) are never copied, though they sit beside what the host shares.
	if extension.Within(extension.CanonicalPath(paths.Records), all) {
		d.Skip = append(d.Skip, extension.CanonicalPath(paths.Records))
	}
	for _, x := range needs.Deny {
		x = extension.CanonicalPath(x)
		for _, m := range all {
			if x == m {
				return Plan{}, fmt.Errorf("%w: %s is granted and denied", extension.ErrSandboxDenied, x)
			}
		}
		if extension.Within(x, all) {
			d.Skip = append(d.Skip, x)
			d.Keep = append(d.Keep, x)
		}
	}
	for _, r := range needs.ReadOnly {
		r = extension.CanonicalPath(r)
		if extension.Within(r, rw) {
			d.Keep = append(d.Keep, r)
		}
	}
	sort.Strings(d.Skip)
	sort.Strings(d.Keep)
	return d, nil
}

// outermost drops each path inside another, and sorts them.
func outermost(paths []string) []string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var out []string
	for _, p := range sorted {
		if !extension.Within(p, out) {
			out = append(out, p)
		}
	}
	return out
}

// secretExports are a launch's credentials as the shell exports the stub
// reads.
func secretExports(secrets []string) string {
	var b strings.Builder
	for _, kv := range secrets {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("export " + k + "=" + quote(v) + "\n")
	}
	return b.String()
}

// writeFresh writes an owner-only file in place of any earlier one, never
// through it.
func writeFresh(path string, data []byte) error {
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// record is a run's record.json on the remote.
type record struct {
	Labels map[string]string `json:"labels"`
	Image  string            `json:"image"`
	Write  []string          `json:"write"`
	Read   []string          `json:"read,omitempty"`
}

// Run is the host's record of a created run, in the execution's records,
// for the helper: how to reach the remote, what to forward and what comes
// back.
type Run struct {
	ID       string           `json:"id"`
	Ref      string           `json:"ref"`
	SSH      []string         `json:"ssh"`
	Host     string           `json:"host"`
	Root     string           `json:"root,omitempty"`
	Write    []string         `json:"write"`
	Keep     []string         `json:"keep,omitempty"`
	Toolbelt string           `json:"toolbelt,omitempty"`
	TTY      bool             `json:"tty,omitempty"`
	Manifest map[string]Entry `json:"manifest"`
}

// RunFile is a run's record in the execution's records.
func RunFile(records, id string) string { return filepath.Join(records, "ssh-run-"+id+".json") }

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (p *Provider) Create(ctx context.Context, plan extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	var d Plan
	if err := json.Unmarshal(plan.Data.Data, &d); err != nil || d.Remote.Host == "" || len(d.Argv) == 0 || d.Records == "" {
		return extension.ContainerCreated{}, errors.New("the plan isn't an ssh plan")
	}
	defer os.Remove(d.EnvFile)
	if d.Secrets != "" {
		defer os.Remove(d.Secrets)
	}
	var env []string
	raw, err := os.ReadFile(d.EnvFile)
	if err == nil {
		err = json.Unmarshal(raw, &env)
	}
	if err != nil {
		return extension.ContainerCreated{}, fmt.Errorf("the plan's environment: %v", err)
	}
	var secrets []byte
	if d.Secrets != "" {
		if secrets, err = os.ReadFile(d.Secrets); err != nil {
			return extension.ContainerCreated{}, fmt.Errorf("the plan's secrets: %v", err)
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return extension.ContainerCreated{}, err
	}
	id := hex.EncodeToString(b)
	ref := d.Remote.Endpoint() + "/" + id
	rec, _ := json.Marshal(record{Labels: labels, Image: plan.Image, Write: d.Write, Read: d.Read})
	files := []tarFile{
		{"record.json", rec, 0o600},
		{"start", []byte(stub(p.o.Root, env, d.Workdir, d.Marker, d.Argv)), 0o700},
	}
	if len(secrets) > 0 {
		files = append(files, tarFile{SecretsFile, secrets, 0o600})
	}
	if _, err := p.ssh(ctx, d.Remote, bytes.NewReader(tarFiles(files)), scriptPrepare, id); err != nil {
		return extension.ContainerCreated{}, fmt.Errorf("could not write the run's records on %s: %v", d.Remote.Endpoint(), err)
	}
	created := false
	defer func() {
		if !created {
			_, _ = p.ssh(context.WithoutCancel(ctx), d.Remote, nil, scriptRemove, append([]string{id}, outermost(append(append([]string{}, d.Write...), d.Read...))...)...)
		}
	}()
	// The upload is streamed as it is walked; its manifest is what came
	// from the host, so only what the run changes comes back.
	roots := outermost(append(append([]string{}, d.Write...), d.Read...))
	pr, pw := io.Pipe()
	var manifest map[string]Entry
	go func() {
		var err error
		manifest, err = writeTree(pw, roots, d.Skip, d.Write, d.Toolbelt)
		pw.CloseWithError(err)
	}()
	_, err = p.ssh(ctx, d.Remote, pr, scriptUpload, append([]string{id}, roots...)...)
	pr.Close()
	if err != nil {
		return extension.ContainerCreated{}, fmt.Errorf("could not copy the launch's paths to %s: %v", d.Remote.Endpoint(), err)
	}
	run := Run{ID: id, Ref: ref, SSH: p.sshArgv(d.Remote), Host: d.Remote.Host, Root: p.o.Root, Write: d.Write, Keep: append(append([]string{}, d.Keep...), d.Toolbelt), Toolbelt: d.Toolbelt, TTY: d.TTY, Manifest: manifest}
	raw, _ = json.Marshal(run)
	if err := writeFresh(RunFile(d.Records, id), raw); err != nil {
		return extension.ContainerCreated{}, err
	}
	created = true
	return extension.ContainerCreated{
		Ref:    extension.ContainerRef{ID: ref, Labels: labels},
		Daemon: d.Daemon,
		Argv:   []string{p.o.Helper, "remote", "run", "--records", d.Records, ref},
		Env:    p.cliEnv(),
	}, nil
}

func (p *Provider) Signal(ctx context.Context, ref extension.ContainerRef, signal string) error {
	s, id, err := p.splitRef(ref.ID)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Z0-9]+$`).MatchString(signal) {
		return fmt.Errorf("signal %q is not a signal name", signal)
	}
	_, err = p.ssh(ctx, s, nil, scriptSignal, id, signal)
	return err
}

func (p *Provider) Stop(ctx context.Context, ref extension.ContainerRef, grace time.Duration) error {
	s, id, err := p.splitRef(ref.ID)
	if err != nil {
		return err
	}
	_, err = p.ssh(ctx, s, nil, scriptStop, id, tenths(grace))
	return err
}

func (p *Provider) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	s, id, err := p.splitRef(ref.ID)
	if err != nil {
		return extension.ContainerState{}, err
	}
	out, err := p.ssh(ctx, s, nil, scriptInspect, id)
	if err != nil {
		return extension.ContainerState{}, err
	}
	states, err := parseStates(s, out)
	if err != nil {
		return extension.ContainerState{}, err
	}
	st, ok := states[id]
	if !ok {
		return extension.ContainerState{}, fmt.Errorf("remote %s didn't report run %s", s.Endpoint(), id)
	}
	return st, nil
}

func (p *Provider) List(ctx context.Context, owner extension.ResourceOwner) ([]extension.ContainerState, error) {
	seen := map[string]bool{}
	var out []extension.ContainerState
	for _, s := range p.o.Remotes() {
		if seen[s.Endpoint()] {
			continue
		}
		seen[s.Endpoint()] = true
		raw, err := p.ssh(ctx, s, nil, scriptList)
		if err != nil {
			return nil, err
		}
		states, err := parseStates(s, raw)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(states))
		for id := range states {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			st := states[id]
			if st.Gone || st.Ref.Labels[extension.ContainerLabelHost] != p.o.Host {
				continue
			}
			if owner.ExecutionID != "" && st.Ref.Labels[extension.ContainerLabelExecution] != string(owner.ExecutionID) {
				continue
			}
			out = append(out, st)
		}
	}
	return out, nil
}

func (p *Provider) Remove(ctx context.Context, ref extension.ContainerRef) error {
	s, id, err := p.splitRef(ref.ID)
	if err != nil {
		return err
	}
	out, err := p.ssh(ctx, s, nil, scriptInspect, id)
	if err != nil {
		return err
	}
	states, err := parseStates(s, out)
	if err != nil {
		return err
	}
	st := states[id]
	if st.Gone {
		return nil
	}
	_, err = p.ssh(ctx, s, nil, scriptRemove, append([]string{id}, st.Mounts...)...)
	return err
}

// tenths is a grace in tenths of a second, rounded up.
func tenths(d time.Duration) string {
	return strconv.Itoa(int((d + 100*time.Millisecond - 1) / (100 * time.Millisecond)))
}

// parseStates reads the remote's report of its runs: a line per fact,
// "<fact> <id> [value]".
func parseStates(s Settings, out []byte) (map[string]extension.ContainerState, error) {
	states := map[string]extension.ContainerState{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		fact, rest, _ := strings.Cut(sc.Text(), " ")
		id, value, _ := strings.Cut(rest, " ")
		if !idPattern.MatchString(id) {
			continue
		}
		st, ok := states[id]
		if !ok {
			st = extension.ContainerState{Ref: extension.ContainerRef{ID: s.Endpoint() + "/" + id}, Daemon: s.Daemon(), ExitCode: -1}
		}
		switch fact {
		case "gone":
			st.Gone = true
		case "record":
			var r record
			if err := json.Unmarshal([]byte(value), &r); err != nil {
				return nil, fmt.Errorf("remote %s: run %s has an unreadable record", s.Endpoint(), id)
			}
			st.Ref.Labels, st.Image = r.Labels, r.Image
			st.Mounts = outermost(append(append([]string{}, r.Write...), r.Read...))
		case "started":
			if ns, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && ns > 0 {
				st.StartedAt = time.Unix(0, ns)
			}
		case "exit":
			if code, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				st.ExitCode = code
			}
		case "pid":
		case "running":
			st.Running = true
		}
		states[id] = st
	}
	for id, st := range states {
		// A run whose stub died without recording its status was killed.
		if !st.Gone && !st.Running && !st.StartedAt.IsZero() && st.ExitCode < 0 {
			st.ExitCode = 137
		}
		if st.ExitCode < 0 {
			st.ExitCode = 0
		}
		states[id] = st
	}
	return states, sc.Err()
}

// splitRef is a reference's remote, among the config's, and run ID.
func (p *Provider) splitRef(ref string) (Settings, string, error) {
	i := strings.LastIndex(ref, "/")
	if i < 0 || !idPattern.MatchString(ref[i+1:]) {
		return Settings{}, "", fmt.Errorf("%q is not an ssh run reference", ref)
	}
	endpoint, id := ref[:i], ref[i+1:]
	var found *Settings
	for _, s := range p.o.Remotes() {
		if s.Endpoint() != endpoint {
			continue
		}
		if found != nil && found.HostKey != s.HostKey {
			return Settings{}, "", fmt.Errorf("%w: bindings pin %s to different host keys", extension.ErrContainerUnconfirmed, endpoint)
		}
		s := s
		found = &s
	}
	if found == nil {
		return Settings{}, "", fmt.Errorf("%w: no binding runs on %s", extension.ErrContainerUnconfirmed, endpoint)
	}
	return *found, id, nil
}

// sshArgv is the ssh command line for a remote, without its command: the
// operator's ssh config applies, but the host key checked is the pinned
// one alone, and nothing of the host is forwarded but what a run asks for.
func (p *Provider) sshArgv(s Settings) []string {
	digest, _ := HostKeyDigest(s.HostKey)
	alias := "open-conduit-" + (digest + strings.Repeat("0", 16))[:16]
	argv := []string{p.o.SSH,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + p.knownHosts(s, alias),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + alias,
		"-o", "UpdateHostKeys=no",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "PermitLocalCommand=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ConnectTimeout=20",
		"-o", "LogLevel=ERROR",
		"-p", strconv.Itoa(s.Port),
	}
	if s.Identity != "" {
		identity := s.Identity
		if strings.HasPrefix(identity, "~/") {
			identity = p.o.Home + identity[1:]
		}
		argv = append(argv, "-i", identity, "-o", "IdentitiesOnly=yes")
	}
	if s.User != "" {
		argv = append(argv, "-l", s.User)
	}
	return argv
}

// knownHosts is the file a remote's pinned key is kept in, under the
// alias every connection checks it by.
func (p *Provider) knownHosts(s Settings, alias string) string {
	path := filepath.Join(p.o.Dir, "known_hosts-"+strings.TrimPrefix(alias, "open-conduit-"))
	line := alias + " " + strings.TrimSpace(s.HostKey) + "\n"
	if raw, err := os.ReadFile(path); err != nil || string(raw) != line {
		_ = os.MkdirAll(p.o.Dir, 0o700)
		tmp := path + ".tmp"
		if writeFresh(tmp, []byte(line)) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return path
}

// sshError is a failed remote call, with what the remote said.
type sshError struct {
	endpoint, stderr string
	err              error
}

func (e *sshError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return "ssh " + e.endpoint + ": " + msg
}

func (e *sshError) Unwrap() error { return e.err }

// ssh runs a script on the remote with stdin and returns its output.
func (p *Provider) ssh(ctx context.Context, s Settings, stdin io.Reader, script string, args ...string) ([]byte, error) {
	argv := append(p.sshArgv(s), "-T", s.Host, command(p.o.Root, script, args...))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = p.cliEnv()
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &sshError{endpoint: s.Endpoint(), stderr: stderr.String(), err: err}
	}
	return out, nil
}

// cliEnv is the ssh client's environment: what it needs to find its
// config, keys and agent.
func (p *Provider) cliEnv() []string {
	var out []string
	for _, kv := range p.o.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK", "TMPDIR", "LANG":
			out = append(out, kv)
		}
	}
	return out
}
