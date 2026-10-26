package extproc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// ErrRefused wraps a deterministic handshake refusal: no common protocol
// version, a manifest that differs from the discovered file, or a KV
// downgrade. A refused extension is not restarted.
var ErrRefused = errors.New("extproc: extension refused at handshake")

// ErrStopped means the process was closed by the host.
var ErrStopped = errors.New("extproc: extension stopped")

// Failure kinds of a start, for diagnostics (Diagnose). ErrManifestMismatch
// is also an ErrRefused.
var (
	ErrStart            = errors.New("extproc: extension command could not be started")
	ErrHandshake        = errors.New("extproc: extension handshake failed")
	ErrConfigure        = errors.New("extproc: extension configure refused")
	ErrManifestMismatch = errors.New("extproc: the process returned a manifest that differs from its file")
)

// KVMarker records the KV layout version an extension last reported.
type KVMarker interface {
	StoredKV(ctx context.Context, name string) (int, error)
	RecordKV(ctx context.Context, name string, version int) error
}

// Options start one extension process.
type Options struct {
	Found Found
	// StateDir is the extension's private state directory; LogDir receives
	// its captured stderr (<LogDir>/<name>.log, bounded).
	StateDir, LogDir string
	Host             protocol.HostInfo
	// Versions are the protocol versions the host speaks (default
	// protocol.Versions).
	Versions []int
	// Environ is the baseline the sanitized environment is drawn from
	// (default os.Environ). Only safe process variables survive (B05).
	Environ []string
	Service *Service
	// Configure returns the current generation's configure params; nil
	// sends an empty generation.
	Configure func() protocol.ConfigureParams
	// OnConfigured receives each successful configure result.
	OnConfigured func(protocol.ConfigureResult)
	KV           KVMarker
	// MinBackoff and MaxBackoff pace restarts (default 100ms and 30s).
	MinBackoff, MaxBackoff time.Duration
	// HandshakeTimeout bounds initialize and configure (default 10s).
	HandshakeTimeout time.Duration
	// MaxLogBytes bounds the stderr log (default 1 MiB, then truncated).
	MaxLogBytes int64
	Log         *slog.Logger
}

// Process supervises one extension process: start with a sanitized
// environment in its own process group, the initialize/configure handshake,
// stderr capture, restart with backoff after a crash, and shutdown. The
// process is disposable: nothing is reattached across a restart; executors
// re-adopt their executions through executor.recover.
type Process struct {
	o       Options
	name    string
	enabled map[string]bool

	mu      sync.Mutex
	conn    *jsonrpc.Conn
	cmd     *exec.Cmd
	gen     uint64
	changed chan struct{} // closed and replaced on every state change
	fatal   error
	closed  bool
	config  protocol.ConfigureResult
	intx    atomic.Int32
	restart int
	stop    chan struct{}
	exited  chan struct{}
}

// Start launches the extension and completes its handshake. A deterministic
// refusal returns ErrRefused and starts nothing further; any other failure
// of the first start is returned too (the caller decides whether to retry).
// After a successful start, a crash restarts the process with backoff.
func Start(ctx context.Context, o Options) (*Process, error) {
	if o.Service == nil {
		return nil, errors.New("extproc: a service is required")
	}
	if len(o.Versions) == 0 {
		o.Versions = protocol.Versions
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 100 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.MaxLogBytes <= 0 {
		o.MaxLogBytes = 1 << 20
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &Process{o: o, name: o.Found.Manifest.Name, enabled: map[string]bool{}, changed: make(chan struct{}), stop: make(chan struct{}), exited: make(chan struct{})}
	for _, m := range o.Found.Manifest.Enabled() {
		p.enabled[m] = true
	}
	o.Service.inTx = func() bool { return p.intx.Load() > 0 }
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if o.LogDir != "" {
		if err := os.MkdirAll(o.LogDir, 0o700); err != nil {
			return nil, err
		}
	}
	exited, err := p.launch(ctx)
	if err != nil {
		close(p.exited)
		return nil, err
	}
	go p.supervise(exited)
	return p, nil
}

// Name is the extension's registration name.
func (p *Process) Name() string { return p.name }

// Generation counts successful starts (1 after Start).
func (p *Process) Generation() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

// Options returns the options the process was started with.
func (p *Process) Options() Options { return p.o }

// Configured returns the last successful configure result.
func (p *Process) Configured() protocol.ConfigureResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config
}

// Restarts counts restarts after crashes.
func (p *Process) Restarts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restart
}

// Err returns the permanent failure, if any.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fatal
}

func (p *Process) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// sanitized keeps only safe process variables (the B05 baseline): no
// credentials, toolbelt variables or Open Conduit settings reach an
// extension through its environment. Secrets are resolved explicitly.
func sanitized(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch name {
		case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TMPDIR", "TERM":
			out = append(out, kv)
			continue
		}
		if strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// boundedLog appends stderr to a file and truncates it when it exceeds max.
type boundedLog struct {
	mu   sync.Mutex
	f    *os.File
	max  int64
	size int64
}

func (l *boundedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(b), nil
	}
	if l.size+int64(len(b)) > l.max {
		_ = l.f.Truncate(0)
		_, _ = l.f.Seek(0, io.SeekStart)
		l.size = 0
		n, _ := l.f.WriteString("[log truncated at bound]\n")
		l.size += int64(n)
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return len(b), err
}

func (p *Process) stderr() io.Writer {
	if p.o.LogDir == "" {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(p.o.LogDir, p.name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return io.Discard
	}
	info, _ := f.Stat()
	var size int64
	if info != nil {
		size = info.Size()
	}
	return &boundedLog{f: f, max: p.o.MaxLogBytes, size: size}
}

// Environment is the sanitized environment an extension process gets from
// environ (the B05 baseline only).
func Environment(environ []string) []string { return sanitized(environ) }

// Argv resolves the manifest's command: a relative argv[0] that names a
// path, or a file inside the extension directory, is resolved against it.
func (f Found) Argv() []string {
	argv := append([]string(nil), f.Manifest.Command...)
	if !filepath.IsAbs(argv[0]) && strings.ContainsRune(argv[0], filepath.Separator) {
		argv[0] = filepath.Join(f.Dir, argv[0])
	} else if !filepath.IsAbs(argv[0]) {
		if local := filepath.Join(f.Dir, argv[0]); fileExists(local) {
			argv[0] = local
		}
	}
	return argv
}

// launch starts one process and completes its handshake. It returns the
// channel closed when that process exits.
func (p *Process) launch(ctx context.Context) (chan struct{}, error) {
	m := p.o.Found.Manifest
	argv := p.o.Found.Argv()
	if p.o.KV != nil {
		stored, err := p.o.KV.StoredKV(ctx, p.name)
		if err != nil {
			return nil, err
		}
		if _, err := protocol.CheckKV(stored, m.KVVersion); err != nil {
			return nil, p.refuse(err)
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = p.o.Found.Dir
	cmd.Env = sanitized(p.o.Environ)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logw := p.stderr()
	cmd.Stderr = logw
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrStart, p.name, err)
	}
	exited := make(chan struct{})
	conn := jsonrpc.NewConn(stdout, stdin, p.o.Service.Handle, jsonrpc.Options{OnProtocolError: func(err error) {
		p.o.Log.Warn("extension protocol error", "extension", p.name, "error", err)
	}})
	go func() {
		<-conn.Done()
		// A closed stream means the peer is unusable: make sure it exits.
		_ = stdin.Close()
		go func() {
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}()
	}()
	go func() {
		_ = cmd.Wait()
		_ = conn.Close()
		if l, ok := logw.(*boundedLog); ok {
			l.mu.Lock()
			_ = l.f.Close()
			l.f = nil
			l.mu.Unlock()
		}
		close(exited)
	}()
	if err := p.handshake(ctx, conn); err != nil {
		_ = conn.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		return nil, err
	}
	p.mu.Lock()
	p.conn, p.cmd = conn, cmd
	p.gen++
	p.notify()
	p.mu.Unlock()
	return exited, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (p *Process) refuse(err error) error {
	err = fmt.Errorf("%w: %s: %w", ErrRefused, p.name, err)
	p.mu.Lock()
	p.fatal = err
	p.notify()
	p.mu.Unlock()
	return err
}

func (p *Process) handshake(ctx context.Context, conn *jsonrpc.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, p.o.HandshakeTimeout)
	defer cancel()
	var init protocol.InitializeResult
	err := conn.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: p.o.Versions, Host: p.o.Host, StateDir: p.o.StateDir}, &init)
	if errors.Is(err, protocol.ErrIncompatible) {
		return p.refuse(err)
	}
	if err != nil {
		return fmt.Errorf("%w: initialize %s: %w", ErrHandshake, p.name, err)
	}
	if _, err := protocol.Negotiate([]int{init.ProtocolVersion}, p.o.Versions); err != nil {
		return p.refuse(err)
	}
	if !sameManifest(init.Manifest, p.o.Found.Manifest) {
		return p.refuse(ErrManifestMismatch)
	}
	params := protocol.ConfigureParams{}
	if p.o.Configure != nil {
		params = p.o.Configure()
	}
	if p.o.KV != nil {
		if params.KV.Stored, err = p.o.KV.StoredKV(ctx, p.name); err != nil {
			return err
		}
	}
	params.KV.Declared = p.o.Found.Manifest.KVVersion
	return p.configure(ctx, conn, params)
}

func (p *Process) configure(ctx context.Context, conn *jsonrpc.Conn, params protocol.ConfigureParams) error {
	var res protocol.ConfigureResult
	if err := conn.Call(ctx, "configure", params, &res); err != nil {
		if errors.Is(err, protocol.ErrTransport) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: configure %s: %w", ErrHandshake, p.name, err)
		}
		return fmt.Errorf("%w: %s: %w", ErrConfigure, p.name, err)
	}
	if res.KVVersion != p.o.Found.Manifest.KVVersion {
		return fmt.Errorf("%w: %s: KV layout %d, manifest %d", ErrConfigure, p.name, res.KVVersion, p.o.Found.Manifest.KVVersion)
	}
	if err := p.checkSources(res); err != nil {
		return err
	}
	if p.o.KV != nil && params.KV.Stored != res.KVVersion {
		if err := p.o.KV.RecordKV(ctx, p.name, res.KVVersion); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(res.Sources))
	for _, s := range res.Sources {
		names = append(names, s.Name)
	}
	p.o.Service.SetSources(names)
	p.mu.Lock()
	p.config = res
	p.mu.Unlock()
	if p.o.OnConfigured != nil {
		p.o.OnConfigured(res)
	}
	return nil
}

// checkSources enforces the extension's own namespaces on what configure
// registers.
func (p *Process) checkSources(res protocol.ConfigureResult) error {
	m := p.o.Found.Manifest
	families := map[string]protocol.Family{}
	for _, f := range m.Provides.Families {
		families[f.Type] = f
	}
	for _, s := range res.Sources {
		if !protocol.SourceNamespace(m.Name, s.Name) || !protocol.SourceNamespace(m.Name, s.Source.Namespace) {
			return fmt.Errorf("%w: %s: source %q is outside its namespace", ErrConfigure, p.name, s.Name)
		}
		for _, f := range s.Families {
			if d, ok := families[f.Type]; !ok || d.ReDerivable != f.ReDerivable || d.Loss != f.Loss {
				return fmt.Errorf("%w: %s: source family %q differs from the manifest", ErrConfigure, p.name, f.Type)
			}
		}
	}
	for _, path := range res.WebhookPaths {
		if !strings.HasPrefix(path, "/"+m.Name+"/") {
			return fmt.Errorf("%w: %s: webhook path %q is outside /%s/", ErrConfigure, p.name, path, m.Name)
		}
	}
	return nil
}

// Reconfigure sends configure again (config apply, tunnel URL change)
// without a restart.
func (p *Process) Reconfigure(ctx context.Context) error {
	conn, err := p.connection(ctx)
	if err != nil {
		return err
	}
	params := protocol.ConfigureParams{}
	if p.o.Configure != nil {
		params = p.o.Configure()
	}
	if p.o.KV != nil {
		if params.KV.Stored, err = p.o.KV.StoredKV(ctx, p.name); err != nil {
			return err
		}
	}
	params.KV.Declared = p.o.Found.Manifest.KVVersion
	return p.configure(ctx, conn, params)
}

func sameManifest(raw json.RawMessage, want protocol.Manifest) bool {
	got, err := protocol.ParseManifest(raw)
	if err != nil {
		return false
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return bytes.Equal(a, b)
}

func (p *Process) supervise(exited chan struct{}) {
	defer close(p.exited)
	backoff := p.o.MinBackoff
	for {
		started := time.Now()
		select {
		case <-exited:
		case <-p.stop:
			p.terminate(exited)
			return
		}
		p.mu.Lock()
		p.conn, p.cmd = nil, nil
		p.notify()
		closed := p.closed
		p.mu.Unlock()
		p.o.Service.abandonWriters()
		if closed {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = p.o.MinBackoff
		}
		p.o.Log.Warn("extension exited; restarting", "extension", p.name, "backoff", backoff)
		for {
			select {
			case <-time.After(backoff):
			case <-p.stop:
				return
			}
			backoff = min(backoff*2, p.o.MaxBackoff)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-p.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			next, err := p.launch(ctx)
			cancel()
			if err == nil {
				p.mu.Lock()
				p.restart++
				p.mu.Unlock()
				exited = next
				break
			}
			if errors.Is(err, ErrRefused) {
				p.o.Log.Error("extension refused; not restarting", "extension", p.name, "error", err)
				return
			}
			p.o.Log.Warn("extension restart failed", "extension", p.name, "error", err)
		}
	}
}

// connection waits for a configured connection, ctx, a permanent failure or
// close.
func (p *Process) connection(ctx context.Context) (*jsonrpc.Conn, error) {
	for {
		p.mu.Lock()
		conn, fatal, closed, changed := p.conn, p.fatal, p.closed, p.changed
		p.mu.Unlock()
		switch {
		case closed:
			return nil, ErrStopped
		case fatal != nil:
			return nil, fatal
		case conn != nil:
			return conn, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// AwaitGeneration waits until the process has started after generation gen
// (a restart), ctx ends or the process fails permanently.
func (p *Process) AwaitGeneration(ctx context.Context, gen uint64) error {
	for {
		p.mu.Lock()
		cur, conn, fatal, closed, changed := p.gen, p.conn, p.fatal, p.closed, p.changed
		p.mu.Unlock()
		switch {
		case closed:
			return ErrStopped
		case fatal != nil:
			return fatal
		case cur > gen && conn != nil:
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Call makes a host-to-extension call the manifest enables. It waits for a
// running process (bounded by ctx) but never retries: a crash mid-call is
// protocol.ErrTransport, and the caller applies the method's crash
// semantics.
func (p *Process) Call(ctx context.Context, method string, params, out any) error {
	m, ok := protocol.Lookup(method)
	if !ok || m.Direction == protocol.ExtensionToHost || !p.enabled[method] {
		return protocol.Errorf(protocol.CodeUndeclared, "method not enabled by the manifest")
	}
	conn, err := p.connection(ctx)
	if err != nil {
		return err
	}
	return conn.Call(ctx, method, params, out)
}

// CallInTx makes a bounded in-transaction call: a hard deadline of
// protocol.InTransactionDeadlineMillis, no waiting for a restart, and any
// callback from the extension refused with reentrant_call while it runs. A
// timeout or transport failure is returned for the caller to abort its
// transaction.
func (p *Process) CallInTx(ctx context.Context, method string, params, out any) error {
	m, ok := protocol.Lookup(method)
	if !ok || !m.InTransaction || !p.enabled[method] {
		return protocol.Errorf(protocol.CodeUndeclared, "not an enabled in-transaction method")
	}
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return protocol.ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(protocol.InTransactionDeadlineMillis)*time.Millisecond)
	defer cancel()
	p.intx.Add(1)
	defer p.intx.Add(-1)
	return conn.Call(ctx, method, params, out)
}

// Kill terminates the process group abruptly (tests and fault drills); the
// supervisor restarts it.
func (p *Process) Kill() {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Close shuts the extension down: shutdown, then stdin closed, SIGTERM
// after a grace period and SIGKILL to the process group. It does not
// restart; it returns once the process has exited.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.exited
		return nil
	}
	p.closed = true
	conn := p.conn
	p.notify()
	p.mu.Unlock()
	if conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Call(ctx, "shutdown", struct{}{}, nil)
		cancel()
	}
	close(p.stop)
	<-p.exited
	return nil
}

// terminate ends the current process (the supervisor's, so a process
// launched concurrently with Close is never left behind): stdin closed,
// which the SDK treats as the host going away, then SIGTERM and SIGKILL.
func (p *Process) terminate(exited chan struct{}) {
	p.mu.Lock()
	conn, cmd := p.conn, p.cmd
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != 0 && cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, sig)
		}
		select {
		case <-exited:
			return
		case <-time.After(2 * time.Second):
		}
	}
	<-exited
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
