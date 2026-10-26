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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/executors/shell"
	"github.com/alsiola/open-conduit/extension"
	bb "github.com/alsiola/open-conduit/extensions/bitbucket"
	gh "github.com/alsiola/open-conduit/extensions/github"
	is "github.com/alsiola/open-conduit/extensions/introspection"
	sc "github.com/alsiola/open-conduit/extensions/schedule"
	sl "github.com/alsiola/open-conduit/extensions/slack"
	"github.com/alsiola/open-conduit/extensions/tracker"
	"github.com/alsiola/open-conduit/internal/artifacts"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/management"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
	"github.com/alsiola/open-conduit/internal/telemetry"
	"github.com/alsiola/open-conduit/observability"
	"github.com/alsiola/open-conduit/storage"
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
	// ResourceInterval paces resource lease reconciliation (default 5s).
	ResourceInterval time.Duration
	// ExecutorInterval paces executor housekeeping such as the Claude Code
	// handoff feedback questions and reactions (default 30s).
	ExecutorInterval time.Duration
	// Ready, when set, is called once the management socket is listening.
	Ready func()
	// WebhookAddr enables the webhook listener on this TCP address (for
	// example 127.0.0.1:8787). Empty means reconcile-only.
	WebhookAddr string
	// Tunnel exposes the webhook listener (optional).
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
	configs        *management.ConfigService
	resolver       *config.Resolver
	pipeline       *telemetry.Pipeline
	ingress        *runtime.Ingress
	github         githubSources
	slack          slackSources
	trackers       trackerSources
	posthog        posthogState
	schedule       scheduleState
	introspection  introspectionState
	resources      resourceState
	claude         claudeState
	hooks          webhookState
	tools          *gh.Tools
	slackTools     *sl.Tools
	reports        *is.Tools
	trackerTools   map[string]*tracker.Tools
	bitbucketTools *bb.Tools
	external       externalState
	endpoint       string
	cursorKey      []byte
	current        atomic.Pointer[components]

	ctx      context.Context
	mu       sync.Mutex
	inflight map[attemptKey]domain.LeaseRef
	reported map[string]string
	wg       sync.WaitGroup
	nudge    chan struct{}
}

func (o *Options) defaults() error {
	if !filepath.IsAbs(o.StateDir) || !filepath.IsAbs(o.ConfigPath) {
		return errors.New("state and config paths must be absolute")
	}
	if o.Socket == "" {
		o.Socket = filepath.Join(o.StateDir, "management.sock")
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
	if o.ResourceInterval == 0 {
		o.ResourceInterval = 5 * time.Second
	}
	if o.ExecutorInterval == 0 {
		o.ExecutorInterval = 30 * time.Second
	}
	if o.PollInterval <= 0 || o.LeaseDuration <= 0 || o.LeaseDuration > 24*time.Hour || o.RetentionInterval <= 0 || o.ResourceInterval <= 0 || o.ExecutorInterval <= 0 {
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
	d := &daemon{o: o, log: slog.New(slog.NewTextHandler(o.Log, nil)), now: time.Now, inflight: map[attemptKey]domain.LeaseRef{}, reported: map[string]string{}, nudge: make(chan struct{}, 1), resolver: &config.Resolver{SecretsFile: filepath.Join(filepath.Dir(o.ConfigPath), "secrets.json")}}
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

	// The exclusive lock precedes every touch of the database or socket.
	lock, err := management.AcquireLock(o.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
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
	introspectionTelemetry, err := d.openIntrospectionTelemetry()
	if err != nil {
		return err
	}
	defer d.closeIntrospectionTelemetry()
	d.pipeline, err = telemetry.New(telemetry.Options{Exporters: []observability.Exporter{telemetry.NewLogExporter(logFile), otel, introspectionTelemetry, d.posthogExporter(), extensionExporter{d}}, FlushInterval: 200 * time.Millisecond, Diagnostic: func(x telemetry.Diagnostic) {
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
	if err = d.openTools(); err != nil {
		return err
	}
	if err = d.openResources(); err != nil {
		return err
	}
	// Installed extension packages start before the configuration opens,
	// so their validators check it (07-03).
	d.openExtensions(ctx)
	defer d.closeExtensions()
	reg := registry(d.executors)
	d.extensionRegistry(&reg)
	if d.configs, err = management.OpenConfig(ctx, d.coord, d.now, reg, o.ConfigPath); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	d.posthog.configs.Store(d.configs)
	if d.ingress, err = runtime.NewIngress(d.coord, d.now, runtime.IngressOptions{}); err != nil {
		return err
	}
	snapshot, generation := d.configs.Current()
	d.registerGitHub(ctx, snapshot)
	d.registerTrackers(ctx, snapshot)
	d.registerSlack(ctx, snapshot)
	d.registerSchedule(ctx, snapshot)
	d.publishGeneration(snapshot, generation)
	d.configureExtensions(ctx)
	c, err := d.build(snapshot, generation)
	if err != nil {
		return err
	}
	d.current.Store(c)
	d.introspection.telemetry.Store(introspectionTelemetryOn(snapshot))
	d.ensureSchedules(ctx, c, snapshot, true)
	d.ensureIntrospection(ctx, c, snapshot, true)
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
		d.introspection.telemetry.Store(introspectionTelemetryOn(s))
		d.ensureSchedules(d.ctx, next, s, true)
		d.ensureIntrospection(d.ctx, next, s, true)
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
	stopWebhooks, err := d.startWebhooks(loopCtx)
	if err != nil {
		return err
	}
	defer stopWebhooks()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { d.current.Load().server.ServeHTTP(w, r) }), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	d.log.Info("open-conduit serving", "version", o.Version, "commit", o.Commit, "endpoint", d.endpoint, "generation", generation, "owner", d.owner, "dry_run", o.DryRun)
	if o.Ready != nil {
		o.Ready()
	}

	loopErr := d.loop(loopCtx)

	// Graceful shutdown: stop serving, stop observing (processes survive) and
	// release live leases so the next daemon can recover them immediately.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
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
	d.extensionHandlers(routeHandlers)
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
	if err = d.registerTools(belt, s); err != nil {
		return nil, err
	}
	if err = d.registerExtensionTools(belt); err != nil {
		return nil, err
	}
	c.toolbelt = belt
	// One binary registers the same executor implementations for every
	// generation, so earlier pinned attempts are reinstalled exactly.
	history := map[string]map[string]extension.Executor{}
	for g := uint64(1); g < gen; g++ {
		history[strconv.FormatUint(g, 10)] = d.executors
	}
	kinds := workKinds()
	d.extensionWorkKinds(kinds)
	recurring := recurrences()
	d.extensionRecurrences(recurring)
	delays := make([]time.Duration, len(set.RetryDelaysSeconds))
	for i, seconds := range set.RetryDelaysSeconds {
		delays[i] = time.Duration(seconds) * time.Second
	}
	if c.scheduler, err = runtime.NewScheduler(d.coord, d.now, c.router, runtime.SchedulerOptions{
		Owner: d.owner, LeaseDuration: d.o.LeaseDuration, Generation: c.generation,
		Executors: d.executors, HistoricalExecutors: history,
		GlobalLimit: set.Concurrency, Retry: runtime.RetryPolicy{Delays: delays}, CausalLimits: limits,
		Timeout:  time.Duration(set.TimeoutSeconds) * time.Second,
		Toolbelt: belt, ToolbeltEndpoint: d.endpoint, Secrets: d.configs.Secrets(d.resolver),
		Resources: d.bindResources, KeptExecutions: set.KeptExecutions, Artifacts: d.artifacts,
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
	pipelineCheck := func(context.Context) runtime.Check {
		status, code, detail := d.pipeline.Health()
		return runtime.Check{Name: "telemetry", Status: status, Code: code, Detail: detail}
	}
	if c.server, err = management.NewServer(management.Options{Store: d.coord, Artifacts: d.artifacts, Now: d.now, Control: control, Toolbelt: belt, Config: d.configs, Version: d.o.Version, InstanceID: d.owner, CursorKey: d.cursorKey, Ingress: d.ingress, Scheduler: c.scheduler, Resolver: d.resolver, Checks: []func(context.Context) runtime.Check{pipelineCheck, d.githubCheck, d.trackerCheck, d.posthogCheck, d.slackCheck, d.capabilityCheck, d.webhookCheck, d.claudeCheck, d.resourceCheck, d.scheduleCheck, d.introspectionCheck, d.extensionCheck},
		DeepChecks: []func(context.Context) runtime.Check{d.githubAuthCheck, d.trackerAuthCheck, d.posthogAuthCheck, d.slackAuthCheck, d.scheduleWebhookCheck, d.extensionDeepCheck}, Annotate: d.annotate, Attach: d.attach, Nudge: d.poke}); err != nil {
		return nil, err
	}
	return c, nil
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
