// Package bootstrap composes the Open Conduit daemon (`open-conduit serve`).
// It owns no domain logic: it wires SQLite coordination behind lifecycle
// instrumentation, local artifacts, the shell and Claude Code executors, configuration, the
// router, scheduler, timers, toolbelt and the local management server, and
// drives them with one host loop (routing, polling, starting, observation,
// lease renewal, physical-stop confirmation and retention).
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/internal/telemetry"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

// Options are resolved by the `serve` command. Paths must be absolute.
type Options struct {
	Version, Commit string
	StateDir        string
	ConfigPath      string
	// Socket defaults to StateDir/management.sock.
	Socket string
	// ToolbeltDir contains the `os` binary (default: beside the executable).
	ToolbeltDir string
	// LifecycleLog receives one JSON line per lifecycle record (default
	// StateDir/logs/lifecycle.jsonl, appended across restarts).
	LifecycleLog string
	// Log receives daemon diagnostics (default stderr).
	Log io.Writer
	// PollInterval paces the host loop (default 1s).
	PollInterval time.Duration
	// LeaseDuration is the execution lease (default 1m). After a crash a new
	// daemon recovers a running attempt only once its lease has expired.
	LeaseDuration time.Duration
	// RetentionInterval paces the kept_executions sweep (default 10m).
	RetentionInterval time.Duration
	// ArtifactGrace is how old unreferenced artifact content must be before
	// the retention pass collects it (default 24h).
	ArtifactGrace time.Duration
	// ResourceInterval paces resource lease reconciliation (default 5s).
	ResourceInterval time.Duration
	// SweepInterval paces the spare-workspace sweep (default 6h, as in v1).
	// The first sweep runs one interval after start.
	SweepInterval time.Duration
	// ResourceDiskSpace overrides the git provider's free-space probe (tests).
	ResourceDiskSpace gitws.DiskSpaceFunc
	// ExecutorInterval paces executor housekeeping such as the Claude Code
	// handoff feedback questions and reactions (default 30s).
	ExecutorInterval time.Duration
	// Ready, when set, is called once the management socket is listening.
	Ready func()
	// WebhookAddr enables the webhook listener on this TCP address (for
	// example 127.0.0.1:8787). Empty falls back to settings.webhooks, and
	// with neither the daemon is reconcile-only.
	WebhookAddr string
	// Tunnel exposes the webhook listener (optional). Any tunnel field
	// replaces the stored settings.webhooks tunnel; Binary applies to both.
	Tunnel TunnelOptions
	// NoTunnel disables the listener and tunnel (reconcile-only).
	NoTunnel bool
	// DryRun makes no external writes: GitHub projections, tool writes and
	// webhook management report dry_run, and Claude Code turns run as
	// stubs on a cheap model.
	DryRun bool
	// ClaudeUsage lets doctor read the Claude subscription usage windows
	// with the machine's Claude login (off by default).
	ClaudeUsage bool
	// ClaudeUsageURL and ClaudeUsageToken override the usage endpoint and
	// token source (tests).
	ClaudeUsageURL   string
	ClaudeUsageToken func(context.Context) (string, error)
}

// components are rebuilt for every committed configuration generation; the
// shared stores, executors and listener survive a reload.
type components struct {
	generation string
	router     *runtime.Router
	toolbelt   *runtime.Toolbelt
	scheduler  *runtime.Scheduler
	timers     *runtime.Timers
	control    *runtime.Control
	server     *management.Server
}

type attemptKey struct {
	id    domain.ID
	epoch uint64
}

type daemon struct {
	o              Options
	log            *slog.Logger
	now            func() time.Time
	owner          string
	coord          storage.Store
	artifacts      *artifacts.Store
	shell          *shell.Executor
	executors      map[string]extension.Executor
	inproc         []inproc.Extension
	configs        *management.ConfigService
	resolver       *config.Resolver
	pipeline       *telemetry.Pipeline
	ingress        *runtime.Ingress
	github         githubSources
	slack          slackSources
	trackers       trackerSources
	schedule       scheduleState
	telemetry      telemetryFanout
	telemetryFile  *os.File
	resources      resourceState
	sandboxes      sandboxState
	containers     containerState
	claude         claudeState
	hooks          webhookState
	tools          *gh.Tools
	slackTools     *sl.Tools
	trackerTools   map[string]*tracker.Tools
	bitbucketTools *bb.Tools
	external       externalState
	endpoint       string
	endpoints      *management.Endpoints
	cursorKey      []byte
	current        atomic.Pointer[components]

	ctx      context.Context
	mu       sync.Mutex
	inflight map[attemptKey]domain.LeaseRef
	// from are the executable and toolbelt the daemon started from.
	from     []string
	reported map[string]string
	wg       sync.WaitGroup
	nudge    chan struct{}
}

// toolbeltBeside finds the directory holding the `os` toolbelt next to the
// executable at self. A link to the executable (Homebrew links only
// open-conduit onto PATH, and the install script does the same) is resolved
// first, so the toolbelt is found in the real install directory, never an
// unrelated `os` that happens to sit beside the link. It returns "" when
// neither directory has one; the executors then run without a toolbelt.
func toolbeltBeside(self string) string {
	dirs := []string{}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		dirs = append(dirs, filepath.Dir(real))
	}
	dirs = append(dirs, filepath.Dir(self))
	for _, dir := range dirs {
		if info, err := os.Stat(filepath.Join(dir, "os")); err == nil && info.Mode().IsRegular() {
			return dir
		}
	}
	return ""
}

// ErrInstallRemoved reports that the daemon's own executable or its
// toolbelt went away while it ran, as an upgrade does when it replaces the
// install directory. The daemon stops rather than give executions a toolbelt
// that is no longer there; a service manager restarts it on the new install.
var ErrInstallRemoved = errors.New("the installed open-conduit was removed or replaced; restart to run the new version")

// runningFrom is what the daemon runs from: its executable, links resolved,
// and the toolbelt's `os`. Only paths present at startup are watched.
func runningFrom(toolbelt string) []string {
	var paths []string
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			paths = append(paths, real)
		}
	}
	if toolbelt != "" {
		paths = append(paths, filepath.Join(toolbelt, "os"))
	}
	return slices.DeleteFunc(paths, func(p string) bool { _, err := os.Stat(p); return err != nil })
}

// removed is the first of paths that no longer exists, or "".
func removed(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
	}
	return ""
}

func (o *Options) defaults() error {
	if !filepath.IsAbs(o.StateDir) || !filepath.IsAbs(o.ConfigPath) {
		return errors.New("state and config paths must be absolute")
	}
	if o.Socket == "" {
		o.Socket = filepath.Join(o.StateDir, "management.sock")
	}
	if o.ToolbeltDir == "" {
		if self, err := os.Executable(); err == nil {
			o.ToolbeltDir = toolbeltBeside(self)
		}
	}
	if o.LifecycleLog == "" {
		o.LifecycleLog = filepath.Join(o.StateDir, "logs", "lifecycle.jsonl")
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.PollInterval == 0 {
		o.PollInterval = time.Second
	}
	if o.LeaseDuration == 0 {
		o.LeaseDuration = time.Minute
	}
	if o.RetentionInterval == 0 {
		o.RetentionInterval = 10 * time.Minute
	}
	if o.ArtifactGrace == 0 {
		o.ArtifactGrace = 24 * time.Hour
	}
	if o.ResourceInterval == 0 {
		o.ResourceInterval = 5 * time.Second
	}
	if o.ExecutorInterval == 0 {
		o.ExecutorInterval = 30 * time.Second
	}
	if o.SweepInterval == 0 {
		o.SweepInterval = 6 * time.Hour
	}
	if o.PollInterval <= 0 || o.LeaseDuration <= 0 || o.LeaseDuration > 24*time.Hour || o.RetentionInterval <= 0 || o.ArtifactGrace <= 0 || o.ResourceInterval <= 0 || o.ExecutorInterval <= 0 || o.SweepInterval <= 0 {
		return errors.New("intervals must be positive")
	}
	return nil
}

// Serve runs the daemon until ctx is cancelled. Detached executor processes
// keep running across a shutdown or crash; the next daemon adopts them.
func Serve(ctx context.Context, o Options) (err error) {
	if err = o.defaults(); err != nil {
		return err
	}
	d := &daemon{o: o, log: slog.New(slog.NewTextHandler(o.Log, nil)), now: time.Now, inproc: firstParty(), inflight: map[attemptKey]domain.LeaseRef{}, reported: map[string]string{}, nudge: make(chan struct{}, 1), resolver: &config.Resolver{SecretsFile: filepath.Join(filepath.Dir(o.ConfigPath), "secrets.json")}}
	suffix := make([]byte, 4)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}
	host, _ := os.Hostname()
	d.owner = fmt.Sprintf("open-conduit/%s/%d/%s", host, os.Getpid(), hex.EncodeToString(suffix))
	d.cursorKey = make([]byte, 32)
	if _, err = rand.Read(d.cursorKey); err != nil {
		return err
	}
	d.endpoint = "unix://" + o.Socket
	d.from = runningFrom(o.ToolbeltDir)

	// The exclusive lock precedes every touch of the database or socket.
	lock, err := management.AcquireLock(o.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Each execution's toolbelt is served on a socket of its own in its run
	// directory, answered by the current generation's server.
	if d.endpoints, err = management.NewEndpoints(lock, o.StateDir, "", func(id domain.ID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d.current.Load().server.ExecutionHandler(id).ServeHTTP(w, r)
		})
	}); err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, filepath.Join(o.StateDir, "conduit.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	logFile, err := openLog(o.LifecycleLog)
	if err != nil {
		return err
	}
	defer logFile.Close()
	tracer, meter, logger := sdktrace.NewTracerProvider(), sdkmetric.NewMeterProvider(), sdklog.NewLoggerProvider()
	otel, err := telemetry.NewOTel(telemetry.OTelOptions{Tracer: tracer, Meter: meter, Logger: logger})
	if err != nil {
		return err
	}
	if err = d.openTelemetry(); err != nil {
		return err
	}
	defer d.closeTelemetry()
	d.pipeline, err = telemetry.New(telemetry.Options{Exporters: append(append([]observability.Exporter{telemetry.NewLogExporter(logFile), otel}, d.inprocExporters()...), extensionExporter{d}), FlushInterval: 200 * time.Millisecond, Diagnostic: func(x telemetry.Diagnostic) {
		d.log.Warn("telemetry export degraded", "exporter", x.Exporter, "code", x.Code, "records", x.Records)
	}})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.pipeline.Shutdown(shutdown)
		_ = tracer.Shutdown(shutdown)
		_ = meter.Shutdown(shutdown)
		_ = logger.Shutdown(shutdown)
	}()
	var coord storage.Store = faultStore(store, d.log)
	if d.coord, err = runtime.Instrument(coord, runtime.TelemetryOptions{Observer: d.pipeline, Diagnostic: func(code string) { d.report("telemetry", "", code) }}); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(o.StateDir, "artifacts"), 0o700); err != nil {
		return err
	}
	if d.artifacts, err = artifacts.Open(filepath.Join(o.StateDir, "artifacts")); err != nil {
		return err
	}
	defer d.artifacts.Close()
	// The state lock is held and nothing writes yet: pending streams are a
	// killed process's unfinished writes.
	if err = d.artifacts.Recover(ctx); err != nil {
		return fmt.Errorf("artifact recovery: %w", err)
	}
	if err = os.MkdirAll(filepath.Join(o.StateDir, "executors"), 0o700); err != nil {
		return err
	}
	if d.shell, err = shell.New(shell.Options{StateDir: filepath.Join(o.StateDir, "executors", "shell"), Artifacts: d.artifacts, ToolbeltDir: o.ToolbeltDir}); err != nil {
		return err
	}
	if err = d.openClaudeCode(); err != nil {
		return err
	}
	defer d.closeClaudeCode()
	legacy, err := sc.NewLegacyExecutor(sc.LegacyOptions{Inner: d.shell, StateDir: filepath.Join(o.StateDir, "executors", sc.LegacyExecutorName), Artifacts: d.artifacts})
	if err != nil {
		return err
	}
	d.executors = map[string]extension.Executor{"shell": d.shell, ClaudeCodeExecutor: d.claude.executor, gh.HoldExecutor: gh.NewHoldExecutor(), sc.LegacyExecutorName: legacy}
	if err = d.startInproc(ctx); err != nil {
		return err
	}
	defer d.stopInproc()
	if err = d.openTools(); err != nil {
		return err
	}
	if err = d.openResources(); err != nil {
		return err
	}
	d.openSandboxes()
	d.openContainers()
	// Installed extension packages start before the configuration opens,
	// so their validators check it (07-03).
	d.openExtensions(ctx)
	reg := registry(d.executors)
	d.installedRegistry(&reg)
	if d.configs, err = management.OpenConfig(ctx, d.coord, d.now, reg, o.ConfigPath, DefaultConfig()); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if a := d.configs.Adopted; a != nil {
		d.log.Warn("stored configuration generation no longer validates; adopted the config file", "stored_generation", a.Stored, "path", o.ConfigPath, "cause", a.Cause.Error())
	}
	if d.ingress, err = runtime.NewIngress(d.coord, d.now, runtime.IngressOptions{}); err != nil {
		return err
	}
	snapshot, generation := d.configs.Current()
	d.registerGitHub(ctx, snapshot)
	d.registerTrackers(ctx, snapshot)
	d.registerSlack(ctx, snapshot)
	d.registerSchedule(ctx, snapshot)
	d.publishGeneration(snapshot, generation)
	c, err := d.build(snapshot, generation)
	if err != nil {
		return err
	}
	d.current.Store(c)
	// Extensions are configured once the generation is active, so a
	// configure can arm timers through it, as every later apply can.
	d.configureExtensions(ctx)
	d.ensureSchedules(ctx, c, snapshot, true)
	// A committed reload swaps generation-pinned components; attempts already
	// running keep their pinned generation through HistoricalExecutors.
	d.configs.OnApply = func(_ context.Context, s *config.Snapshot, g uint64) {
		next, err := d.build(s, g)
		if err != nil {
			d.log.Error("configuration generation not activated in this process; restart required", "generation", g)
			return
		}
		d.current.Store(next)
		d.registerGitHub(d.ctx, s)
		d.registerTrackers(d.ctx, s)
		d.registerSlack(d.ctx, s)
		d.registerSchedule(d.ctx, s)
		d.publishGeneration(s, g)
		d.configureExtensions(d.ctx)
		d.ensureSchedules(d.ctx, next, s, true)
		d.log.Info("configuration generation activated", "generation", g)
		d.poke()
	}

	tokenPath := filepath.Join(o.StateDir, "private", "operator.token")
	if err = management.BootstrapOperator(ctx, d.coord, d.now, tokenPath); err != nil && !errors.Is(err, management.ErrCredentialExists) {
		return err
	}
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	d.ctx = loopCtx
	listener, err := management.Listen(lock, o.Socket)
	if err != nil {
		return err
	}
	stopWebhooks, err := d.startWebhooks(loopCtx, snapshot.Settings.Webhooks)
	if err != nil {
		return err
	}
	defer stopWebhooks()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { d.current.Load().server.ServeHTTP(w, r) }), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	d.log.Info("open-conduit serving", "version", o.Version, "commit", o.Commit, "endpoint", d.endpoint, "generation", generation, "owner", d.owner, "dry_run", o.DryRun)
	// A restart resumes the stored generation, never the file: say so when
	// they differ, or an edit looks applied when it is not.
	switch d.configs.DiskStatus() {
	case "default":
		d.log.Info("no config file: running the default configuration, a setup scope; the first apply writes the file", "generation", generation, "config", o.ConfigPath)
	case "pending_edit":
		d.log.Warn("config file differs from the active generation and was not applied; restarting does not apply it", "generation", generation, "config", o.ConfigPath, "apply_with", fmt.Sprintf("open-conduit config apply %s --generation %d", o.ConfigPath, generation))
	case "invalid":
		d.log.Warn("config file is invalid; the active generation keeps running", "generation", generation, "config", o.ConfigPath, "check_with", "open-conduit config validate "+o.ConfigPath)
	}
	if o.Ready != nil {
		o.Ready()
	}

	loopErr := d.loop(loopCtx)

	// Graceful shutdown: stop serving, stop observing (processes survive) and
	// release live leases so the next daemon can recover them immediately.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	d.endpoints.Shutdown(shutdown)
	stopLoop()
	d.wg.Wait()
	d.releaseAll(shutdown)
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) && loopErr == nil {
		loopErr = err
	}
	d.log.Info("open-conduit stopped")
	return loopErr
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func (d *daemon) build(s *config.Snapshot, gen uint64) (*components, error) {
	if s == nil || gen == 0 {
		return nil, storage.ErrInvalid
	}
	c := &components{generation: strconv.FormatUint(gen, 10)}
	set := s.Settings
	limits := runtime.CausalLimits{MaxDepth: set.CausalMaxDepth, PerMinute: set.CausalPerMinute}
	bindings := map[string]runtime.RouteBinding{}
	for name, e := range d.executors {
		bindings[name] = runtime.RouteBinding{Executor: e}
	}
	var err error
	routeHandlers := handlers()
	inprocHandlers(routeHandlers, d.inproc)
	if c.router, err = runtime.NewRouter(d.coord, d.now, runtime.RouterOptions{Generation: c.generation, Handlers: routeHandlers, Bindings: bindings, CausalLimits: limits}); err != nil {
		return nil, err
	}
	var belt *runtime.Toolbelt
	grant := d.configs.Grant(func() []string { return belt.EventTypes() })
	// The configured causal limits bound toolbelt emissions too (03-08 fix:
	// they were applied only to routing, timers and control, so an execution's
	// emissions pinned the default limits on a new root).
	belt, err = runtime.NewToolbelt(d.coord, d.now, c.router, runtime.ToolbeltOptions{CausalLimits: limits, Authorize: d.configs.Authorize, Grant: func(ctx context.Context, state runtime.AttemptState) (runtime.ToolbeltGrant, error) {
		g, err := grant(ctx, state)
		g.Operations = registeredOnly(g.Operations, belt.Operations())
		return g, err
	}, Observer: d.pipeline, AuthorizeAlias: d.authorizeAlias})
	if err != nil {
		return nil, err
	}
	if err = d.registerTools(belt); err != nil {
		return nil, err
	}
	if err = d.registerInprocTools(belt, s); err != nil {
		return nil, err
	}
	c.toolbelt = belt
	// One binary registers the same executor implementations for every
	// generation, so earlier pinned attempts are reinstalled exactly.
	history := map[string]map[string]extension.Executor{}
	for g := uint64(1); g < gen; g++ {
		history[strconv.FormatUint(g, 10)] = d.executors
	}
	kinds := workKinds(d.inproc)
	d.extensionWorkKinds(kinds)
	recurring := recurrences(d.inproc)
	d.extensionRecurrences(recurring)
	delays := make([]time.Duration, len(set.RetryDelaysSeconds))
	for i, seconds := range set.RetryDelaysSeconds {
		delays[i] = time.Duration(seconds) * time.Second
	}
	if c.scheduler, err = runtime.NewScheduler(d.coord, d.now, c.router, runtime.SchedulerOptions{
		Owner: d.owner, LeaseDuration: d.o.LeaseDuration, Generation: c.generation,
		Executors: d.executors, HistoricalExecutors: history,
		GlobalLimit: set.Concurrency, ExecutorPools: jobExecutors(), PoolLimits: map[string]int{jobPool: set.JobConcurrency},
		ScopeLimits: scopeLimits(s), Retry: runtime.RetryPolicy{Delays: delays}, CausalLimits: limits,
		Timeout:  time.Duration(set.TimeoutSeconds) * time.Second,
		Toolbelt: belt, ToolbeltEndpoint: d.endpoint, ToolbeltEndpoints: d.endpoints.Serve, Secrets: d.configs.Secrets(d.resolver),
		Resources: d.bindResources, Sandbox: d.bindSandbox, Container: d.bindContainer, Signalled: d.containerSignalled, KeptExecutions: set.KeptExecutions, Artifacts: d.artifacts,
		AuthorizeAlias: d.authorizeAlias, AliasFilter: belt.DependencyFilter,
		WorkKinds: kinds,
	}); err != nil {
		return nil, err
	}
	if c.timers, err = runtime.NewTimers(d.coord, d.now, runtime.TimerOptions{CausalLimits: limits, Recurrences: recurring}); err != nil {
		return nil, err
	}
	control, err := runtime.NewControl(d.coord, d.now, c.router, c.scheduler, limits)
	if err != nil {
		return nil, err
	}
	c.control = control
	if c.server, err = management.NewServer(management.Options{Store: d.coord, Artifacts: d.artifacts, Now: d.now, Control: control, AdmitTrace: d.admitTrace, ConfigCandidate: d.configCandidate, Toolbelt: belt, Config: d.configs, Version: d.o.Version, InstanceID: d.owner, CursorKey: d.cursorKey, Ingress: d.ingress, Scheduler: c.scheduler, Resolver: d.resolver, Checks: d.checks(),
		DeepChecks: append(append([]func(context.Context) runtime.Check{d.githubAuthCheck, d.trackerAuthCheck}, d.inprocChecks(true)...), d.slackAuthCheck, d.scheduleWebhookCheck, d.extensionDeepCheck), Capacity: d.capacityPools, Annotate: d.annotate, Actions: d.actions, Attach: d.attach, ContainerAttach: d.containerAttach, ContainerDetach: d.containerDetach, Live: d.live, Nudge: d.poke}); err != nil {
		return nil, err
	}
	return c, nil
}

// checks are doctor's ordinary checks: none resolves a secret or calls a
// service (GET /doctor without check_secrets, and setup's doctor probe).
func (d *daemon) checks() []func(context.Context) runtime.Check {
	pipelineCheck := func(context.Context) runtime.Check {
		status, code, detail := d.pipeline.Health()
		return runtime.Check{Name: "telemetry", Status: status, Code: code, Detail: detail}
	}
	return append(append([]func(context.Context) runtime.Check{pipelineCheck, d.githubCheck, d.trackerCheck}, d.inprocChecks(false)...), d.slackCheck, d.capabilityCheck, d.webhookCheck, d.claudeCheck, d.resourceCheck, d.sandboxCheck, d.containerCheck, d.capacityCheck, d.scheduleCheck, d.extensionCheck)
}

// scopeLimits gives the scheduler each scope whose limits.concurrency
// narrows its parent's (or settings.concurrency at the root). The scheduler
// checks every ancestor, so an inherited limit needs no entry of its own.
func scopeLimits(s *config.Snapshot) map[domain.ID]int {
	out := map[domain.ID]int{}
	for id, sc := range s.Scopes {
		inherited := s.Settings.Concurrency
		if p, ok := s.Scopes[sc.ParentID]; ok {
			inherited = p.Concurrency
		}
		if sc.Concurrency > 0 && sc.Concurrency < inherited {
			out[id] = sc.Concurrency
		}
	}
	return out
}

func (d *daemon) poke() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// report logs a diagnostic once per subject until its code changes, so a
// persistently ineligible item does not flood the log on every pass.
func (d *daemon) report(kind string, subject domain.ID, code string) {
	key := kind + "\x00" + string(subject)
	d.mu.Lock()
	same := d.reported[key] == code
	d.reported[key] = code
	d.mu.Unlock()
	if !same && code != "" {
		d.log.Info("runtime diagnostic", "kind", kind, "id", string(subject), "code", code)
	}
}

func (d *daemon) clear(kind string, subject domain.ID) {
	d.mu.Lock()
	delete(d.reported, kind+"\x00"+string(subject))
	d.mu.Unlock()
}
