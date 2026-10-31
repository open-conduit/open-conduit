// Package certify is the extension conformance runner (07-03), shipped as
// `open-conduit extension certify DIR`. It starts a package exactly as the
// daemon does (internal/extproc behind a real SQLite coordination store and
// artifact store, in a temporary directory), runs the version 1
// certification criteria (conformance.Criteria) with the package's own
// fixture, and reports the tested protocol, host and package versions, each
// criterion's result and the limits of what was checked.
package certify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// Runner identifies this runner in reports.
const Runner = "open-conduit extension certify"

// Options select the package and describe the host running the criteria.
type Options struct {
	Dir                     string
	HostVersion, HostCommit string
	// Progress, when set, receives one line per criterion as it finishes.
	Progress io.Writer
	// HandshakeTimeout bounds each start (default 30s).
	HandshakeTimeout time.Duration
}

// Status of one criterion.
const (
	Pass = "pass"
	Fail = "fail"
	Skip = "skip"
)

type Result struct {
	ID      string `json:"id"`
	Surface string `json:"surface"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

type PackageInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
	Dir     string `json:"dir"`
}

type ProtocolInfo struct {
	Package    []int `json:"package"`
	Host       []int `json:"host"`
	Negotiated int   `json:"negotiated,omitempty"`
}

type HostInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Report is a certification report.
type Report struct {
	Runner          string       `json:"runner"`
	CriteriaVersion int          `json:"criteria_version"`
	Package         PackageInfo  `json:"package"`
	Protocol        ProtocolInfo `json:"protocol"`
	Host            HostInfo     `json:"host"`
	StartedAt       time.Time    `json:"started_at"`
	DurationMillis  int64        `json:"duration_ms"`
	Results         []Result     `json:"results"`
	Limits          []string     `json:"limits"`
	Certified       bool         `json:"certified"`
}

// Failed lists the IDs of failed criteria.
func (r Report) Failed() []string {
	var out []string
	for _, x := range r.Results {
		if x.Status == Fail {
			out = append(out, x.ID)
		}
	}
	return out
}

// Result returns one criterion's result.
func (r Report) Result(id string) Result {
	for _, x := range r.Results {
		if x.ID == id {
			return x
		}
	}
	return Result{ID: id}
}

// Text renders the report for a terminal.
func (r Report) Text() string {
	var b strings.Builder
	verdict := "NOT CERTIFIED"
	if r.Certified {
		verdict = "CERTIFIED"
	}
	fmt.Fprintf(&b, "%s: %s %s — %s (criteria version %d)\n", r.Runner, r.Package.Name, r.Package.Version, verdict, r.CriteriaVersion)
	fmt.Fprintf(&b, "Tested: package protocol %v, host protocol %v (negotiated %d); host %s %s (%s) on %s/%s with %s\n", r.Protocol.Package, r.Protocol.Host, r.Protocol.Negotiated, r.Host.Name, r.Host.Version, r.Host.Commit, r.Host.OS, r.Host.Arch, r.Host.Go)
	if r.Package.Digest != "" {
		fmt.Fprintf(&b, "Package digest: sha256:%s\n", r.Package.Digest)
	}
	b.WriteString("\n")
	for _, x := range r.Results {
		fmt.Fprintf(&b, "  %-4s  %-28s %s\n", x.Status, x.ID, x.Detail)
	}
	b.WriteString("\nLimits:\n")
	for _, l := range r.Limits {
		fmt.Fprintf(&b, "  - %s\n", l)
	}
	return b.String()
}

// limits are what the current criteria version does not establish.
var limits = []string{
	"Certification checks protocol conformance, not safety: an extension runs with the operator's privileges (process separation is fault isolation, not a sandbox).",
	"Only the package's own fixture is exercised, against its fakes; no live service is contacted and no real credential is used.",
	"One host build, one platform and one protocol version are tested: the ones stated above.",
	"Not certified by criteria version 2: webhook.handle, setup.run and setup.* callbacks, provider.* resource providers, telemetry.export, run.environment, work.annotate, work.actions and work.act, view.render, event.render, emit and wait operations (the host cuts a wait plan's description to fit as it is declared), the in-transaction callbacks (recurrence.next, work_kind.trigger, emit.commit, dependency.filter) and the message-size and overload limits. The host's own suites cover those adapters.",
	"Executor checks run one attempt each without a toolbelt endpoint or workspace; resume, interrupt and attach are not exercised.",
}

type runner struct {
	o        Options
	pkg      extproc.Package
	m        protocol.Manifest
	found    extproc.Found
	fixture  protocol.Fixture
	tmp      string
	state    string
	store    storage.Store
	arts     *artifacts.Store
	scopes   []domain.ID
	protoErr atomic.Int64
	results  map[string]Result
	order    []string
	mu       sync.Mutex
}

func (r *runner) set(id, status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.results[id]; !ok {
		r.order = append(r.order, id)
	}
	if prev, ok := r.results[id]; ok && prev.Status == Fail {
		return // a failure is never overwritten
	}
	r.results[id] = Result{ID: id, Status: status, Detail: detail}
	if r.o.Progress != nil {
		fmt.Fprintf(r.o.Progress, "%-4s %s %s\n", status, id, detail)
	}
}
func (r *runner) pass(id, detail string) { r.set(id, Pass, detail) }
func (r *runner) fail(id string, format string, args ...any) {
	r.set(id, Fail, fmt.Sprintf(format, args...))
}
func (r *runner) skip(id, why string) { r.set(id, Skip, why) }

// Run certifies the package in o.Dir.
func Run(ctx context.Context, o Options) (rep Report, err error) {
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 30 * time.Second
	}
	started := time.Now()
	rep = Report{Runner: Runner, CriteriaVersion: conformance.CriteriaVersion, StartedAt: started.UTC(), Limits: append([]string(nil), limits...),
		Host: HostInfo{Name: "open-conduit", Version: o.HostVersion, Commit: o.HostCommit, Go: goruntime.Version(), OS: goruntime.GOOS, Arch: goruntime.GOARCH}}
	rep.Protocol.Host = protocol.Versions
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return rep, err
	}
	rep.Package.Dir = dir
	r := &runner{o: o, results: map[string]Result{}}
	tmp, err := os.MkdirTemp("", "oc-certify-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)
	r.tmp = tmp
	defer func() { rep.Results, rep.Certified = r.finish(), r.certified() }()
	defer func() { rep.DurationMillis = time.Since(started).Milliseconds() }()

	pkg, err := extproc.ReadPackage(dir)
	if err != nil {
		r.fail("manifest.valid", "%v", err)
		return rep, nil
	}
	r.pkg, r.m = pkg, pkg.Manifest
	rep.Package = PackageInfo{Name: r.m.Name, Version: r.m.Version, Digest: pkg.Digest, Dir: dir}
	rep.Protocol.Package = r.m.Protocol
	if v, err := protocol.Negotiate(r.m.Protocol, protocol.Versions); err == nil {
		rep.Protocol.Negotiated = v
	}
	if filepath.Base(dir) != r.m.Name {
		r.fail("manifest.valid", "the package directory must be named %q, after the manifest", r.m.Name)
		return rep, nil
	}
	r.pass("manifest.valid", "strict manifest and package layout")
	r.found = pkg.Found()
	r.state = filepath.Join(tmp, "state", "extensions", r.m.Name)
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		return rep, err
	}
	stopFixture, err := r.loadFixture(ctx)
	if err != nil {
		r.fail("handshake.manifest", "fixture: %v", err)
		return rep, nil
	}
	defer stopFixture()
	if r.store, err = sqlite.Open(ctx, filepath.Join(tmp, "coordination.db")); err != nil {
		return rep, err
	}
	defer r.store.(*sqlite.Store).Close()
	if err := os.MkdirAll(filepath.Join(tmp, "artifacts"), 0o700); err != nil {
		return rep, err
	}
	if r.arts, err = artifacts.Open(filepath.Join(tmp, "artifacts")); err != nil {
		return rep, err
	}
	defer r.arts.Close()
	if err := r.createScopes(ctx); err != nil {
		return rep, err
	}

	r.rawLifecycle(ctx)
	remote, err := r.load(ctx)
	if err != nil {
		r.fail("handshake.manifest", "%s", extproc.Diagnose(r.m.Name, err, extproc.Paths{}).Summary+": "+err.Error())
		r.skipRest("the package did not start")
		return rep, nil
	}
	defer remote.Process().Close()
	r.pass("handshake.manifest", fmt.Sprintf("protocol %d; configure registered %d source(s)", rep.Protocol.Negotiated, len(remote.Process().Configured().Sources)))
	r.storage(ctx, remote)
	r.ingress(remote)
	r.routes(ctx, remote)
	r.operations(ctx, remote)
	r.executors(ctx, remote)
	r.surfaces(ctx, remote)
	r.restart(ctx, remote)
	if n := remote.Service().Refused(); n > 0 {
		r.fail("scope.host_surface", "%d extension-to-host call(s) refused as unauthorized", n)
	} else {
		r.pass("scope.host_surface", "no unauthorized host call")
	}
	if n := r.protoErr.Load(); n > 0 {
		r.fail("transport.clean_stdout", "%d protocol error(s) on the extension's stdout", n)
	} else {
		r.pass("transport.clean_stdout", "only protocol messages on stdout")
	}
	return rep, nil
}

func (r *runner) skipRest(why string) {
	for _, c := range conformance.Criteria {
		r.mu.Lock()
		_, done := r.results[c.ID]
		r.mu.Unlock()
		if !done {
			r.fail(c.ID, "not run: %s", why)
		}
	}
}

// finish orders results by the criteria table and fills unreported ones as
// not applicable.
func (r *runner) finish() []Result {
	out := make([]Result, 0, len(conformance.Criteria))
	for _, c := range conformance.Criteria {
		r.mu.Lock()
		x, ok := r.results[c.ID]
		r.mu.Unlock()
		if !ok {
			x = Result{ID: c.ID, Status: Skip, Detail: "not reached"}
			if r.results["manifest.valid"].Status == Fail {
				x.Detail = "the package is invalid"
			}
		}
		x.Surface = c.Surface
		out = append(out, x)
	}
	return out
}

func (r *runner) certified() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return false
	}
	for _, c := range conformance.Criteria {
		if x, ok := r.results[c.ID]; ok && x.Status == Fail {
			return false
		}
	}
	return r.results["manifest.valid"].Status == Pass && r.results["handshake.manifest"].Status == Pass
}

// Fixture.

func (r *runner) loadFixture(ctx context.Context) (func(), error) {
	stop := func() {}
	raw, err := os.ReadFile(filepath.Join(r.pkg.Dir, extproc.CertificationFile))
	if errors.Is(err, os.ErrNotExist) {
		return stop, nil
	}
	if err != nil {
		return stop, err
	}
	var file protocol.CertificationFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return stop, fmt.Errorf("%s: %w", extproc.CertificationFile, err)
	}
	fixture := raw
	if file.Fixture != nil {
		fixture, _ = json.Marshal(file.Fixture)
	}
	if len(file.Command) > 0 {
		argv := extproc.Found{Dir: r.pkg.Dir, Manifest: protocol.Manifest{Command: file.Command}}.Argv()
		work := filepath.Join(r.tmp, "fixture")
		if err := os.MkdirAll(work, 0o700); err != nil {
			return stop, err
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = work, extproc.Environment(os.Environ())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stderr = io.Discard
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return stop, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return stop, err
		}
		if err := cmd.Start(); err != nil {
			return stop, fmt.Errorf("fixture command: %w", err)
		}
		stop = func() {
			_ = stdin.Close()
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
		}
		line := make(chan []byte, 1)
		go func() {
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 0, 1<<20), protocol.MaxMessageBytes)
			if sc.Scan() {
				line <- append([]byte(nil), sc.Bytes()...)
			}
			close(line)
			_, _ = io.Copy(io.Discard, stdout)
		}()
		select {
		case b, ok := <-line:
			if !ok {
				stop()
				return func() {}, errors.New("the fixture command printed no fixture")
			}
			fixture = b
		case <-time.After(r.o.HandshakeTimeout):
			stop()
			return func() {}, errors.New("the fixture command did not print a fixture in time")
		case <-ctx.Done():
			stop()
			return func() {}, ctx.Err()
		}
		fixture = bytes.ReplaceAll(fixture, []byte("{tmp}"), []byte(work))
	}
	fixture = bytes.ReplaceAll(fixture, []byte("{tmp}"), []byte(r.tmp))
	fixture = bytes.ReplaceAll(fixture, []byte("{state}"), []byte(r.state))
	if file.Fixture != nil || len(file.Command) > 0 {
		dec := json.NewDecoder(bytes.NewReader(fixture))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r.fixture); err != nil {
			return stop, fmt.Errorf("fixture: %w", err)
		}
	}
	return stop, nil
}

func (r *runner) createScopes(ctx context.Context) error {
	seen := map[domain.ID]bool{}
	add := func(id string) {
		if id != "" && !seen[domain.ID(id)] {
			seen[domain.ID(id)] = true
			r.scopes = append(r.scopes, domain.ID(id))
		}
	}
	for _, e := range r.fixture.Entries {
		add(e.ScopeID)
	}
	for _, s := range r.fixture.Scopes {
		add(s)
	}
	add("certify")
	return r.store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		for _, id := range r.scopes {
			if err := tx.PutScope(domain.Scope{ID: id, Name: string(id)}, 0); err != nil {
				return err
			}
		}
		return tx.PutScope(domain.Scope{ID: "certify-foreign", Name: "foreign"}, 0)
	})
}

// counting counts the host's "extension protocol error" log records: a
// malformed or non-protocol line on the extension's stdout.
type counting struct{ n *atomic.Int64 }

func (c counting) Enabled(context.Context, slog.Level) bool { return true }
func (c counting) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message == "extension protocol error" {
		c.n.Add(1)
	}
	return nil
}
func (c counting) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c counting) WithGroup(string) slog.Handler      { return c }

func (r *runner) deps() extproc.Deps {
	bound := map[string]bool{}
	conns := map[string]bool{}
	for _, e := range r.fixture.Entries {
		for _, n := range e.Secrets {
			bound[n] = true
		}
		if e.Connection != nil {
			conns[e.Connection.ID] = true
			for _, n := range e.Connection.Secrets {
				bound[e.Connection.ID+"\x00"+n] = true
			}
		}
	}
	return extproc.Deps{Store: r.store, Now: time.Now, Artifacts: r.arts,
		Scopes: func() []domain.ID { return r.scopes },
		Secrets: func(_ context.Context, _ domain.ID, connection string, names []string) (map[string]string, error) {
			out := map[string]string{}
			for _, n := range names {
				key := n
				if connection != "" {
					key = connection + "\x00" + n
				}
				v, ok := r.fixture.Secrets[n]
				if !bound[key] || !ok {
					return nil, extension.ErrUnauthorized
				}
				out[n] = v
			}
			return out, nil
		}}
}

func (r *runner) options() extproc.Options {
	return extproc.Options{StateDir: r.state, LogDir: filepath.Join(r.tmp, "state", "logs", "extensions"),
		Host: protocol.HostInfo{Name: "open-conduit", Version: r.o.HostVersion},
		Configure: func() protocol.ConfigureParams {
			return protocol.ConfigureParams{Generation: "certify", Entries: r.fixture.Entries}
		},
		KV:               extproc.StoreKV{Store: r.store, Now: time.Now},
		MinBackoff:       50 * time.Millisecond,
		MaxBackoff:       time.Second,
		HandshakeTimeout: r.o.HandshakeTimeout,
		Log:              slog.New(counting{&r.protoErr})}
}

func (r *runner) load(ctx context.Context) (*extproc.Remote, error) {
	return extproc.Load(ctx, r.found, r.deps(), r.options())
}

// Raw sessions: the lifecycle criteria need a connection the runner drives
// itself, outside the host's supervisor.

type rawSession struct {
	cmd    *exec.Cmd
	conn   *jsonrpc.Conn
	stdin  io.WriteCloser
	exited chan struct{}
}

func (r *runner) startRaw() (*rawSession, error) {
	argv := r.found.Argv()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = r.found.Dir, extproc.Environment(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &rawSession{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	s.conn = jsonrpc.NewConn(stdout, stdin, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, protocol.Errorf(protocol.CodeUnsupported, "the certification host serves no callbacks here")
	}, jsonrpc.Options{OnProtocolError: func(error) { r.protoErr.Add(1) }})
	go func() { _ = cmd.Wait(); close(s.exited) }()
	return s, nil
}

func (s *rawSession) kill() {
	_ = s.conn.Close()
	_ = s.stdin.Close()
	select {
	case <-s.exited:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.exited
	}
}

func (r *runner) initialize(ctx context.Context, s *rawSession, versions []int) (protocol.InitializeResult, error) {
	var out protocol.InitializeResult
	cx, cancel := context.WithTimeout(ctx, r.o.HandshakeTimeout)
	defer cancel()
	err := s.conn.Call(cx, "initialize", protocol.InitializeParams{ProtocolVersions: versions, Host: protocol.HostInfo{Name: "open-conduit", Version: r.o.HostVersion}, StateDir: r.state}, &out)
	return out, err
}

func (r *runner) rawLifecycle(ctx context.Context) {
	versions := protocol.Versions
	// An offer the package does not speak: one above anything it lists.
	unsupported := 1
	for _, v := range r.m.Protocol {
		unsupported = max(unsupported, v+1)
	}
	short := func(d time.Duration) (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, d) }

	if s, err := r.startRaw(); err != nil {
		r.fail("handshake.version_refusal", "start: %v", err)
	} else {
		out, err := r.initialize(ctx, s, []int{unsupported})
		switch {
		case protocol.ErrorCode(err) == protocol.CodeUnsupportedVersion:
			r.pass("handshake.version_refusal", fmt.Sprintf("offer %v refused", []int{unsupported}))
		case err == nil:
			r.fail("handshake.version_refusal", "offer %v accepted as version %d", []int{unsupported}, out.ProtocolVersion)
		default:
			r.fail("handshake.version_refusal", "offer %v: %v, want unsupported_protocol_version", []int{unsupported}, err)
		}
		s.kill()
	}

	if s, err := r.startRaw(); err != nil {
		r.fail("lifecycle.before_initialize", "start: %v", err)
	} else {
		cx, cancel := short(r.o.HandshakeTimeout)
		err := s.conn.Call(cx, "configure", protocol.ConfigureParams{}, nil)
		cancel()
		if err == nil {
			r.fail("lifecycle.before_initialize", "configure was served before initialize")
		} else if errors.Is(err, protocol.ErrTransport) || errors.Is(err, context.DeadlineExceeded) {
			r.fail("lifecycle.before_initialize", "no answer before initialize: %v", err)
		} else {
			r.pass("lifecycle.before_initialize", "refused ("+protocol.ErrorCode(err)+")")
		}
		if _, err := r.initialize(ctx, s, versions); err != nil {
			r.fail("lifecycle.unknown_method", "initialize: %v", err)
		} else {
			cx, cancel := short(r.o.HandshakeTimeout)
			err := s.conn.Call(cx, "certify.no_such_method", struct{}{}, nil)
			cancel()
			cx, cancel = short(r.o.HandshakeTimeout)
			again := s.conn.Call(cx, "certify.no_such_method", struct{}{}, nil)
			cancel()
			if protocol.ErrorCode(err) != protocol.CodeMethodNotFound || protocol.ErrorCode(again) != protocol.CodeMethodNotFound {
				r.fail("lifecycle.unknown_method", "unknown method answered %v, then %v; want method_not_found twice", err, again)
			} else {
				r.pass("lifecycle.unknown_method", "method_not_found; connection usable")
			}
			cx, cancel = short(r.o.HandshakeTimeout)
			err = s.conn.Call(cx, "shutdown", struct{}{}, nil)
			cancel()
			if err != nil {
				r.fail("lifecycle.shutdown", "shutdown: %v", err)
			} else {
				cx, cancel = short(r.o.HandshakeTimeout)
				err = s.conn.Call(cx, "configure", protocol.ConfigureParams{}, nil)
				cancel()
				if protocol.ErrorCode(err) == protocol.CodeShuttingDown {
					r.pass("lifecycle.shutdown", "shutting_down after shutdown")
				} else {
					r.fail("lifecycle.shutdown", "a request after shutdown answered %v, want shutting_down", err)
				}
			}
		}
		s.kill()
	}

	if s, err := r.startRaw(); err != nil {
		r.fail("lifecycle.stdin_eof", "start: %v", err)
	} else {
		if _, err := r.initialize(ctx, s, versions); err != nil {
			r.fail("lifecycle.stdin_eof", "initialize: %v", err)
		}
		_ = s.stdin.Close()
		select {
		case <-s.exited:
			r.pass("lifecycle.stdin_eof", "exited after stdin closed")
		case <-time.After(5 * time.Second):
			r.fail("lifecycle.stdin_eof", "still running 5s after stdin closed")
		}
		s.kill()
	}
}

// Surfaces.

func (r *runner) storage(ctx context.Context, remote *extproc.Remote) {
	id := "storage.kv_migration"
	if r.m.KVVersion == 0 {
		r.skip(id, "kv_version 0: no KV layout to migrate")
		return
	}
	kv := extproc.StoreKV{Store: r.store, Now: time.Now}
	if v, err := kv.StoredKV(ctx, r.m.Name); err != nil || v != r.m.KVVersion {
		r.fail(id, "after the first configure the recorded layout is %d (%v), want %d", v, err, r.m.KVVersion)
		return
	}
	// A crash mid-migration: the host recorded nothing, so the next start
	// asks for the same migration again.
	if err := kv.RecordKV(ctx, r.m.Name, 0); err != nil {
		r.fail(id, "%v", err)
		return
	}
	cx, cancel := context.WithTimeout(ctx, r.o.HandshakeTimeout)
	err := remote.Process().Reconfigure(cx)
	cancel()
	if err != nil {
		r.fail(id, "repeating the migration from layout 0 failed: %v", err)
		return
	}
	if v, err := kv.StoredKV(ctx, r.m.Name); err != nil || v != r.m.KVVersion {
		r.fail(id, "after the repeated migration the recorded layout is %d (%v)", v, err)
		return
	}
	r.pass(id, fmt.Sprintf("0 → %d, twice", r.m.KVVersion))
}

func hasIngress(m protocol.Manifest) bool {
	for _, x := range m.Provides.Extensions {
		if x.Ingress {
			return true
		}
	}
	return false
}

func (r *runner) ingress(remote *extproc.Remote) {
	id := "ingress.conformance"
	if !hasIngress(r.m) {
		r.skip(id, "no ingress extension type")
		return
	}
	c := r.fixture.Ingress
	if c == nil {
		r.fail(id, "the fixture has no ingress case (required for an ingress extension type)")
		return
	}
	var decl *protocol.SourceDecl
	for _, s := range remote.Process().Configured().Sources {
		if s.Name == c.Source {
			decl = &s
		}
	}
	if decl == nil {
		r.fail(id, "configure registered no source %q", c.Source)
		return
	}
	f := conformance.IngressFixture{Ingress: remote.Ingress(*decl), Source: decl.Source.Extension(), Delivery: c.Delivery.Extension(), Canonical: observation(c.Canonical), OneShotType: c.OneShotType}
	if c.OneShot != nil {
		d := c.OneShot.Extension()
		f.OneShot = &d
	}
	detail := "source " + c.Source
	if c.ReceiveTimeVaries {
		f.Ingress = ReceiveTimeless{f.Ingress}
		f.Canonical.Event.ReceivedAt = time.Time{}
		detail += " (received_at not compared: the package stamps its own receive time)"
	}
	if err := conformance.CheckIngress(f); err != nil {
		r.fail(id, "%v", err)
		return
	}
	r.pass(id, detail)
}

// ReceiveTimeless clears received_at on an ingress adapter's observations
// and events, for packages whose reconcile stamps its own receive time.
type ReceiveTimeless struct{ extension.Ingress }

func (i ReceiveTimeless) Push(ctx context.Context, s extension.Source, d extension.Delivery) (extension.PushResult, error) {
	res, err := i.Ingress.Push(ctx, s, d)
	for k := range res.Observations {
		res.Observations[k].Event.ReceivedAt = time.Time{}
	}
	for k := range res.Events {
		res.Events[k].ReceivedAt = time.Time{}
	}
	return res, err
}

func (i ReceiveTimeless) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	snap, err := i.Ingress.Reconcile(ctx, s)
	for k := range snap.Observations {
		snap.Observations[k].Event.ReceivedAt = time.Time{}
	}
	return snap, err
}

func observation(o protocol.Observation) extension.Observation {
	snap := protocol.Snapshot{Observations: []protocol.Observation{o}}.Extension()
	return snap.Observations[0]
}

func (r *runner) routes(ctx context.Context, remote *extproc.Remote) {
	id := "routes.decisions"
	if len(r.m.Provides.Routes) == 0 {
		r.skip(id, "no route handler")
		return
	}
	if len(r.fixture.Routes) == 0 {
		r.skip(id, "the fixture has no route cases")
		return
	}
	for i, c := range r.fixture.Routes {
		// The host drops a display over the limits; certification refuses
		// one, so read the package's own answer first.
		var raw protocol.RouteDecision
		if err := remote.Process().Call(ctx, "route.match", protocol.RouteMatchParams{Handler: c.Handler, Input: c.Input}, &raw); err != nil {
			r.fail(id, "case %d: %v", i, err)
			return
		}
		if err := extension.ValidateDisplay(raw.Display.Domain()); err != nil {
			r.fail(id, "case %d: display: %v", i, err)
			return
		}
		h := remote.RouteHandler(c.Handler)
		in := c.Input.Extension()
		first, err := h.Match(ctx, in)
		if err != nil {
			r.fail(id, "case %d: %v", i, err)
			return
		}
		second, err := h.Match(ctx, in)
		// Reason explains a decision without changing it, so certification
		// compares the decision alone.
		first.Reason, second.Reason = "", ""
		if err != nil || !equalJSON(protocol.FromRouteDecision(first), protocol.FromRouteDecision(second)) {
			r.fail(id, "case %d: the decision is not deterministic", i)
			return
		}
		want := c.Want
		want.Reason = ""
		if !equalJSON(protocol.FromRouteDecision(first), want) {
			got, _ := json.Marshal(protocol.FromRouteDecision(first))
			r.fail(id, "case %d: decided %s", i, got)
			return
		}
	}
	r.pass(id, fmt.Sprintf("%d case(s)", len(r.fixture.Routes)))
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (r *runner) authority() extension.Authority {
	return extension.Authority{ScopeID: r.scopes[0], WorkID: "certify-work", ExecutionID: "certify-execution", TriggerID: "certify-trigger", ExpiresAt: time.Now().Add(time.Hour)}
}

func (r *runner) operations(ctx context.Context, remote *extproc.Remote) {
	id := "operations.cases"
	if len(r.m.Provides.Operations) == 0 {
		r.skip(id, "no operations")
		return
	}
	err := remote.Process().Call(ctx, "operation.call", protocol.OperationParams{Operation: r.m.Name + ".certify-undeclared", Authority: protocol.FromAuthority(r.authority())}, nil)
	if protocol.ErrorCode(err) != protocol.CodeUndeclared {
		r.fail(id, "an undeclared operation answered %v, want undeclared", err)
		return
	}
	if len(r.fixture.Operations) == 0 {
		r.pass(id, "undeclared operation refused; the fixture has no operation cases")
		return
	}
	ops := map[string]extension.Operation{}
	for _, op := range remote.Registration().Operations {
		ops[op.Name] = op
	}
	for i, c := range r.fixture.Operations {
		op, ok := ops[c.Operation]
		if !ok || op.Handler == nil {
			r.fail(id, "case %d: %s is not a declared handler operation (the criteria call handler operations only)", i, c.Operation)
			return
		}
		cx := extension.WithCallRequestID(ctx, fmt.Sprintf("certify-%d", i))
		out, err := op.Handler(cx, r.authority(), c.Input.Domain())
		switch {
		case c.Error != "":
			var oe *extension.OperationError
			code := protocol.ErrorCode(err)
			if errors.As(err, &oe) {
				code = oe.Code
			}
			if code != c.Error {
				r.fail(id, "case %d (%s): answered %v, want refusal %s", i, c.Operation, err, c.Error)
				return
			}
		case err != nil:
			r.fail(id, "case %d (%s): %v", i, c.Operation, err)
			return
		case c.Output != nil && !equalJSON(protocol.FromPayload(out), *c.Output):
			got, _ := json.Marshal(protocol.FromPayload(out))
			r.fail(id, "case %d (%s): output %s", i, c.Operation, got)
			return
		}
	}
	r.pass(id, fmt.Sprintf("%d case(s); undeclared operation refused", len(r.fixture.Operations)))
}

func (r *runner) request(executor, execution string, config protocol.Payload) extension.RunRequest {
	scope := r.scopes[0]
	return extension.RunRequest{
		Work:      domain.Work{ID: "certify-work", ScopeID: scope, Kind: "certify.work"},
		Execution: domain.Execution{ID: domain.ID(execution), WorkID: "certify-work", Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Trigger:   domain.Event{ID: "certify-trigger", Type: "certify.trigger", Source: "certify", DeliveryID: "trigger", OccurredAt: time.Now().UTC(), ReceivedAt: time.Now().UTC()},
		Context:   extension.ExecutionContext{Config: config.Domain()},
	}
}

func (r *runner) executors(ctx context.Context, remote *extproc.Remote) {
	execs := r.m.Provides.Executors
	if len(execs) == 0 {
		r.skip("executor.conformance", "no executor")
		r.skip("executor.inject", "no executor")
		r.skip("executor.controls", "no executor")
		return
	}
	c := r.fixture.Executor
	if c == nil {
		r.fail("executor.conformance", "the fixture has no executor case (required for an executor)")
	} else if x, ok := remote.Executor(c.Executor); !ok {
		r.fail("executor.conformance", "executor %q is not declared", c.Executor)
	} else if err := conformance.CheckExecutor(x, r.request(c.Executor, "certify-execution", c.Config)); err != nil {
		r.fail("executor.conformance", "%s: %v", c.Executor, err)
	} else {
		r.pass("executor.conformance", c.Executor)
	}
	inject := false
	for _, e := range execs {
		inject = inject || e.Capabilities.Inject
	}
	switch ic := r.fixture.Inject; {
	case !inject:
		r.skip("executor.inject", "no executor advertises inject")
	case ic == nil:
		r.fail("executor.inject", "the fixture has no inject case (required for an inject-capable executor)")
	default:
		x, ok := remote.Executor(ic.Executor)
		if !ok {
			r.fail("executor.inject", "executor %q is not declared", ic.Executor)
			break
		}
		// However the check ends, the attempt is let finish and stopped, so
		// a failing package leaves no detached job behind.
		defer func() {
			_ = os.WriteFile(ic.FinishFile, nil, 0o600)
			if sx, ok := x.(extension.StopExecutor); ok {
				cx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = sx.Stop(cx, "certify-inject", 1)
				cancel()
			}
		}()
		f := conformance.InjectFixture{Executor: x, Request: r.request(ic.Executor, "certify-inject", ic.Config),
			Finish: func() error { return os.WriteFile(ic.FinishFile, nil, 0o600) }}
		if ic.ReadyFile != "" {
			f.Ready = func() error { return waitFile(ctx, ic.ReadyFile, 30*time.Second) }
		}
		if err := conformance.CheckInjectExecutor(f); err != nil {
			r.fail("executor.inject", "%s: %v", ic.Executor, err)
		} else {
			r.pass("executor.inject", ic.Executor)
		}
	}
	controls := false
	for _, e := range execs {
		x, _ := remote.Executor(e.Name)
		if e.Controls.Stop {
			controls = true
			cx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := x.(extension.StopExecutor).Stop(cx, "certify-unknown", 1)
			cancel()
			if unserved(err) {
				r.fail("executor.controls", "%s stop: %v", e.Name, err)
			}
		}
		if e.Controls.Forget {
			controls = true
			cx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := x.(extension.ForgetExecutor).Forget(cx, "certify-unknown")
			cancel()
			if unserved(err) {
				r.fail("executor.controls", "%s forget: %v", e.Name, err)
			}
		}
	}
	if controls {
		r.pass("executor.controls", "stop/forget served")
	} else {
		r.skip("executor.controls", "no stop or forget control")
	}
}

// unserved reports an answer that means the method was not served at all.
func unserved(err error) bool {
	code := protocol.ErrorCode(err)
	return code == protocol.CodeMethodNotFound || code == protocol.CodeUndeclared || errors.Is(err, protocol.ErrTransport) || errors.Is(err, context.DeadlineExceeded)
}

func waitFile(ctx context.Context, path string, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func (r *runner) surfaces(ctx context.Context, remote *extproc.Remote) {
	id := "surfaces.served"
	p := r.m.Provides
	if len(p.Checks) == 0 && len(p.Tasks) == 0 {
		r.skip(id, "no checks or tasks")
		return
	}
	var notes []string
	for _, c := range p.Checks {
		if c.Deep {
			notes = append(notes, "deep check "+c.Name+" not run (needs live credentials)")
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := remote.Check(cx, c.Name, false, protocol.CheckConfig{})
		cancel()
		if err != nil {
			r.fail(id, "check %s: %v", c.Name, err)
			return
		}
		notes = append(notes, fmt.Sprintf("check %s ok=%t", c.Name, res.OK))
	}
	names := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	for _, t := range names {
		cx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := remote.Task(cx, t)
		cancel()
		if unserved(err) {
			r.fail(id, "task %s: %v", t, err)
			return
		}
		status := "ok"
		if err != nil {
			status = "answered " + protocol.ErrorCode(err)
		}
		notes = append(notes, "task "+t+" "+status)
	}
	r.pass(id, strings.Join(notes, "; "))
}

func (r *runner) restart(ctx context.Context, remote *extproc.Remote) {
	id := "lifecycle.restart"
	p := remote.Process()
	gen := p.Generation()
	p.Kill()
	cx, cancel := context.WithTimeout(ctx, r.o.HandshakeTimeout+5*time.Second)
	defer cancel()
	if err := p.AwaitGeneration(cx, gen); err != nil {
		r.fail(id, "not restarted: %v", err)
		return
	}
	r.pass(id, fmt.Sprintf("restarted (generation %d) and configured again", p.Generation()))
}
