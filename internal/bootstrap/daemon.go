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
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/backup"
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
	// PolicyPath is the operator's policy above the config (--policy,
	// OPEN_CONDUIT_POLICY), read once at start; empty is none.
	PolicyPath string
	// Socket defaults to StateDir/management.sock.
	Socket string
	// ToolbeltDir contains the `os` binary (default: beside the executable).
	ToolbeltDir string
	// LifecycleLog receives one JSON line per lifecycle record (default
	// StateDir/logs/lifecycle.jsonl, appended across restarts and rotated
	// by size; logfile.go).
	LifecycleLog string
	// Log receives daemon diagnostics (default stderr).
	Log io.Writer
	// PollInterval paces the host loop (default 1s).
	PollInterval time.Duration
	// LeaseDuration is the execution lease (default 1m). After a crash a new
	// daemon recovers a running attempt only once its lease has expired.
	LeaseDuration time.Duration
	// RetentionInterval paces the retention sweep (kept_executions and
	// kept_finished_days, default 10m).
	RetentionInterval time.Duration
	// BackupInterval paces the scheduled online backups (default 24h;
	// negative turns them off). BackupDir receives them (default
	// StateDir/backups) and BackupsKept is how many are kept (default 7).
	BackupInterval time.Duration
	BackupDir      string
	BackupsKept    int
	// UpgradeBundlesKept is how many pre-upgrade bundles are kept in
	// BackupDir (default 3) and UpgradeBundlesMaxAge how long (zero: no
	// limit); the newest is always kept. NoUpgradeBundle starts on an older
	// database without taking one (preupgrade.go).
	UpgradeBundlesKept   int
	UpgradeBundlesMaxAge time.Duration
	NoUpgradeBundle      bool
	// ArtifactGrace is how old unreferenced artifact content must be before
	// the retention pass collects it (default 24h).
	ArtifactGrace time.Duration
	// ResourceInterval paces resource lease reconciliation (default 5s).
	ResourceInterval time.Duration
	// CallTimeout bounds each call the host loop makes into an extension
	// or tool it can't vouch for: a route's decision (route.match) and a
	// container runtime's list and inspect (default 5s).
	CallTimeout time.Duration
	// SweepInterval paces the spare-workspace sweep (default 6h).
	// The first sweep runs one interval after the first start; its due
	// time is durable, so restarts don't put it off.
	SweepInterval time.Duration
	// Ready, when set, is called once the management socket is listening.
	Ready func()
	// instanceLease, instanceRenew and instanceRetry pace the instance lease
	// on shared state (default instanceTTL, instanceRenewEvery and
	// instanceRetry). Tests shorten them.
	instanceLease, instanceRenew, instanceRetry time.Duration
	// afterUpgradeBundle, in tests, runs between keeping a pre-upgrade bundle
	// and raising the state it preserved.
	afterUpgradeBundle func()
	// WebhookAddr enables the webhook listener on this TCP address (for
	// example 127.0.0.1:8787). Empty falls back to settings.webhooks, and
	// with neither the daemon is reconcile-only.
	WebhookAddr string
	// Tunnel exposes the webhook listener (optional). Any tunnel field
	// replaces the stored settings.webhooks tunnel; Binary applies to both.
	Tunnel TunnelOptions
	// ProbeAddr enables the probe listener on this TCP address (for
	// example 0.0.0.0:8080): /livez and /readyz with no credential, for an
	// orchestrator's liveness and readiness probes (health.go). Empty
	// leaves it off.
	ProbeAddr string
	// StallAfter is how long the host loop may go without completing a
	// pass before its host_loop component is stalled and /livez fails
	// (default DefaultStallAfter).
	StallAfter time.Duration
	// MetricReaders collect the daemon's meter beside the host's own
	// reader, which sends each collection to the metrics exporter packages
	// (metricexport.go) every MetricInterval (default
	// DefaultMetricInterval). Tests read the meter through them.
	MetricReaders  []sdkmetric.Reader
	MetricInterval time.Duration
	// NoTunnel disables the listener and tunnel (reconcile-only).
	NoTunnel bool
	// DryRun makes no external writes: GitHub projections, tool writes and
	// webhook management report dry_run, and Claude Code turns run as
	// stubs on a cheap model.
	DryRun bool
	// Management is the opt-in management listener on a TCP address
	// (management.NetworkConfig): off by default, so the owner-only socket
	// is the only way in. Half-configured is a startup error.
	Management management.NetworkConfig
	// NoTelemetry turns off the anonymous usage reports (usage.go).
	NoTelemetry bool
	// Releases installs releases of the daemon's binary for an admin
	// (management.Options.Releases); nil, for a binary managed some other
	// way (serve --no-updates), leaves that out.
	Releases management.Releases
	// TelemetryURL and TelemetryKey override where usage reports go
	// (tests). A dev build reports only when TelemetryURL is set.
	TelemetryURL, TelemetryKey string
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
	// policy is the operator's policy above the config, read once at start.
	policy *config.Policy
	o      Options
	// configHandles holds the config texts extensions refer to by handle
	// (configHost).
	configHandles configHandles
	// defaultDoc is the first-run setup's configuration (DefaultConfig),
	// built once the packages have started.
	defaultDoc []byte
	log        *slog.Logger
	now        func() time.Time
	owner      string
	coord      storage.Store
	artifacts  storage.ArtifactStore
	// events is the event archive, which holds what the live log retired.
	events storage.EventArchive
	// compactor gives the database\'s free space back, when its backend can.
	compactor storage.Compactor
	// sharedState is set when this daemon holds its state under the instance
	// lease, so a successor on another host may take it over.
	sharedState bool
	backups     backupState
	duties      dutyClock
	// retaining is set while a retention pass runs. startup is this start's
	// timing, recorded once its first pass is done.
	retaining     atomic.Bool
	startup       startupClock
	configs       *management.ConfigService
	resolver      *config.Resolver
	pipeline      *telemetry.Pipeline
	ingress       *runtime.Ingress
	telemetry     telemetryFanout
	telemetryFile *rotatingLog
	resources     resourceState
	executorState executorStateSweep
	sandboxes     sandboxState
	containers    containerState
	// sites records the sites the host gave each resource type.
	sites     siteLedger
	hooks     webhookState
	external  externalState
	endpoint  string
	endpoints *management.Endpoints
	// git is the toolbelt's git transport, one across config generations
	// so a reload doesn't end a git command's session mid-push.
	git *management.GitTransport
	// served are the secret values the toolbelt served each execution,
	// in memory only, scrubbed from what the execution publishes.
	served    runtime.ServedSecrets
	cursorKey []byte
	current   atomic.Pointer[components]

	ctx      context.Context
	mu       sync.Mutex
	inflight map[attemptKey]domain.LeaseRef
	// leftForNext are the leases of attempts whose attendance ended with
	// the daemon's shutdown, still running, for releaseAll to release so
	// the next daemon adopts them at once.
	leftForNext []domain.LeaseRef
	// stopping are the attempts a stop follow-up is stopping, off the loop.
	stopping map[attemptKey]bool
	// from are the executable and toolbelt the daemon started from.
	from     []string
	reported map[string]string
	// reportedAt is when each reported key was last reported, so
	// pruneReported can drop per-record keys nobody reports any more.
	reportedAt map[string]time.Time
	wg         sync.WaitGroup
	nudge      chan struct{}
	// workers are the sources and Work the pass's workers are busy with.
	workers workerState
	// passTimes records how long each pass and its phases take.
	passTimes *passTimes
	// txTimes records how long write transactions wait for and hold the
	// writer, when the store reports it.
	txTimes *txTimes
	// health is whether the daemon is alive and ready (health.go).
	health healthState
	// metricExports is how the metrics exporter packages' last exports
	// went (metricexport.go).
	metricExports metricExports
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

// stableToolbelt points <state>/toolbelt/os at the `os` in dir and returns
// <state>/toolbelt, the toolbelt directory executions are given. A package
// manager installs each version in a directory of its own and removes the
// old one on upgrade (Homebrew's Caskroom/open-conduit/<version>), so a run
// whose PATH, git transport or MCP server named that directory
// would lose its toolbelt mid-turn. The link outlives the install; each
// daemon start re-points it, atomically, at the install it runs from. With
// no toolbelt (dir is "") it returns "".
func stableToolbelt(stateDir, dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	target := filepath.Join(dir, "os")
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("toolbelt %s is not an absolute path", target)
	}
	stable := filepath.Join(stateDir, "toolbelt")
	if err := os.MkdirAll(stable, 0o755); err != nil {
		return "", err
	}
	link := filepath.Join(stable, "os")
	if got, err := os.Readlink(link); err == nil && got == target {
		return stable, nil
	}
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return stable, nil
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
	if o.BackupInterval == 0 {
		o.BackupInterval = 24 * time.Hour
	}
	if o.BackupDir == "" {
		o.BackupDir = filepath.Join(o.StateDir, "backups")
	}
	if o.BackupsKept == 0 {
		o.BackupsKept = 7
	}
	if o.UpgradeBundlesKept == 0 {
		o.UpgradeBundlesKept = defaultUpgradeBundlesKept
	}
	if !filepath.IsAbs(o.BackupDir) || o.BackupsKept < 1 || o.UpgradeBundlesKept < 1 || o.UpgradeBundlesMaxAge < 0 {
		return errors.New("backups need an absolute directory and at least one kept")
	}
	if o.ArtifactGrace == 0 {
		o.ArtifactGrace = 24 * time.Hour
	}
	if o.ResourceInterval == 0 {
		o.ResourceInterval = 5 * time.Second
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = runtime.DefaultRouteTimeout
	}
	if o.SweepInterval == 0 {
		o.SweepInterval = 6 * time.Hour
	}
	if o.StallAfter == 0 {
		o.StallAfter = DefaultStallAfter
	}
	if o.MetricInterval == 0 {
		o.MetricInterval = DefaultMetricInterval
	}
	faultPace(o)
	if o.instanceLease == 0 {
		o.instanceLease = instanceTTL
	}
	if o.instanceRenew == 0 {
		o.instanceRenew = instanceRenewEvery
	}
	if o.instanceRetry == 0 {
		o.instanceRetry = instanceRetry
	}
	if o.PollInterval <= 0 || o.LeaseDuration <= 0 || o.LeaseDuration > 24*time.Hour || o.RetentionInterval <= 0 || o.ArtifactGrace <= 0 || o.ResourceInterval <= 0 || o.CallTimeout <= 0 || o.SweepInterval <= 0 || o.StallAfter <= 0 || o.MetricInterval <= 0 {
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
	d := &daemon{o: o, log: slog.New(slog.NewTextHandler(o.Log, nil)), now: daemonClock(), inflight: map[attemptKey]domain.LeaseRef{}, reported: map[string]string{}, reportedAt: map[string]time.Time{}, nudge: make(chan struct{}, 1), resolver: &config.Resolver{SecretsFile: config.SecretsPath(o.ConfigPath)}}
	if d.policy, err = config.LoadPolicy(o.PolicyPath); err != nil {
		return fmt.Errorf("policy %s: %w", o.PolicyPath, err)
	}
	d.resolver.Policy = d.policy
	suffix := make([]byte, 4)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}
	host, _ := os.Hostname()
	d.owner = fmt.Sprintf("open-conduit/%s/%d/%s", host, os.Getpid(), hex.EncodeToString(suffix))
	d.health.phase.Store(management.PhaseStarting)
	d.startup.began = d.now()
	// A listener that would serve plaintext to the network, or is
	// half-configured, stops the start before anything is touched.
	if err = o.Management.Check(); err != nil {
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
	// An in-place restore that stopped part way leaves the directory with
	// some of each timeline: never serve it.
	if err = restoreInterrupted(o.StateDir); err != nil {
		return err
	}
	// List cursors are signed with a key kept in the state directory, so a
	// client's cursor survives a restart.
	if d.cursorKey, err = management.CursorKey(filepath.Join(o.StateDir, "private", "cursor.key")); err != nil {
		return err
	}
	// Executions get the toolbelt through a link this daemon re-points, so
	// one still running when an upgrade removes the old install finds the
	// new `os` once the daemon restarts on it.
	if dir, err := stableToolbelt(o.StateDir, o.ToolbeltDir); err != nil {
		d.log.Warn("executions get the install's own toolbelt directory, which an upgrade removes", "error", err)
	} else {
		d.o.ToolbeltDir, o.ToolbeltDir = dir, dir
	}
	// Each execution's toolbelt is served on a socket of its own in its run
	// directory, answered by the current generation's server.
	d.git = management.NewGitTransport(nil, nil)
	if d.endpoints, err = management.NewEndpoints(lock, o.StateDir, "", func(id domain.ID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d.current.Load().server.ExecutionHandler(id).ServeHTTP(w, r)
		})
	}); err != nil {
		return err
	}
	// The probe listener answers from here on, so an orchestrator sees a
	// standby as alive and not ready rather than restarting it. It is the only
	// listener a standby opens, and it reads no state until the daemon runs.
	stopProbes, err := d.startProbes()
	if err != nil {
		return err
	}
	defer stopProbes()
	// The state directory's storage choice selects the backend.
	// On state other hosts can reach, a live holder makes this daemon a
	// standby, here, until it releases the lease or lets it expire.
	// Opening raises the stored format, after which an older binary refuses
	// the database: a bundle of it as it stands is kept first, once no other
	// instance is changing it.
	backend, store, err := openServing(ctx, o.StateDir, d.log, o.instanceLease, o.instanceRetry, d.preserveBeforeUpgrade, func() {
		d.health.phase.Store(management.PhaseStandby)
	})
	if err != nil {
		return err
	}
	defer store.Close()
	d.health.phase.CompareAndSwap(management.PhaseStandby, management.PhaseStarting)
	if held, ok := store.(*heldDatabase); ok {
		d.sharedState = true
		// Superseded, this daemon has no authority left: it stops, and
		// comes back as a standby if something restarts it.
		var lose context.CancelCauseFunc
		ctx, lose = context.WithCancelCause(ctx)
		defer lose(nil)
		defer func() {
			if cause := context.Cause(ctx); errors.Is(cause, storage.ErrInstanceFenced) {
				err = fmt.Errorf("another instance took over this state: %w", cause)
			}
		}()
		held.onLost(func(cause error) {
			d.log.Error("another instance took over this state; stopping")
			// Ingress belongs to the successor from now on: refuse it and
			// report unready at once, not when the loop has wound down.
			d.health.superseded.Store(true)
			d.health.phase.Store(management.PhaseStopping)
			lose(cause)
		})
		//duty:instance_renewal
		ticks := time.NewTicker(o.instanceRenew)
		defer ticks.Stop()
		kept := make(chan struct{})
		keepCtx, stopKeeping := context.WithCancel(ctx)
		go func() { defer close(kept); held.keep(keepCtx, ticks.C, d.log) }()
		defer func() { stopKeeping(); <-kept }()
	}
	d.recordBuild(ctx, store)
	archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(o.StateDir, backup.ArchiveDir))
	if err != nil {
		return fmt.Errorf("event archive: %w", err)
	}
	defer archive.Close()
	d.events = archive
	d.compactor = compactorOf(store)

	logFile, err := openLog(o.LifecycleLog)
	if err != nil {
		return err
	}
	defer logFile.Close()
	// The host's own reader collects for the metrics exporter packages.
	collector := sdkmetric.NewManualReader()
	readers := []sdkmetric.Option{sdkmetric.WithReader(collector)}
	for _, r := range o.MetricReaders {
		readers = append(readers, sdkmetric.WithReader(r))
	}
	tracer, meter, logger := sdktrace.NewTracerProvider(), sdkmetric.NewMeterProvider(readers...), sdklog.NewLoggerProvider()
	otel, err := telemetry.NewOTel(telemetry.OTelOptions{Tracer: tracer, Meter: meter, Logger: logger})
	if err != nil {
		return err
	}
	if d.passTimes, err = newPassTimes(meter.Meter(telemetry.Scope)); err != nil {
		return err
	}
	if timer, ok := rawDatabase(store).(storage.TransactionTimer); ok {
		if d.txTimes, err = newTxTimes(meter.Meter(telemetry.Scope)); err != nil {
			return err
		}
		timer.TimeTransactions(d.txTimes.record)
		defer timer.TimeTransactions(nil)
	}
	if err = d.openTelemetry(); err != nil {
		return err
	}
	defer d.closeTelemetry()
	exporters := append(append([]observability.Exporter{telemetry.NewLogExporter(logFile), otel}, d.packageExporters()...), extensionExporter{d})
	usage, err := newUsageReporter(o)
	if err != nil {
		return err
	}
	if usage != nil {
		exporters = append(exporters, usage)
	}
	d.pipeline, err = telemetry.New(telemetry.Options{Exporters: exporters, FlushInterval: 200 * time.Millisecond, Secrets: d.served.All, Diagnostic: func(x telemetry.Diagnostic) {
		d.log.Warn("telemetry export degraded", "exporter", x.Exporter, "code", x.Code, "records", x.Records)
	}})
	if err != nil {
		return err
	}
	d.resolver.Providers, d.resolver.Observe, d.resolver.Cache = d.secretProvider, d.observeSecret, &config.SecretCache{Now: d.now}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.pipeline.Shutdown(shutdown)
		_ = tracer.Shutdown(shutdown)
		_ = meter.Shutdown(shutdown)
		_ = logger.Shutdown(shutdown)
	}()
	coord := storage.Retry(faultStore(store, d.log), d.retryPolicy())
	if d.coord, err = runtime.Instrument(coord, runtime.TelemetryOptions{Observer: d.pipeline, Diagnostic: func(code string) { d.report("telemetry", "", code) }}); err != nil {
		return err
	}
	// Before anything else can write: a rebuild holds the write lock throughout,
	// and packages write from their handshake.
	d.rebuildAtStart(ctx, d.compactor, d.storedSettings(ctx))
	// The store is the state directory's chosen artifact backend; an
	// unknown one stops the daemon rather than open an empty store.
	if _, d.artifacts, err = openArtifacts(context.Background(), o.StateDir); err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	defer d.artifacts.Close()
	d.backups.source = backend
	if err = d.startBackups(backend.name, store); err != nil {
		return fmt.Errorf("backups: %w", err)
	}
	if err = os.MkdirAll(filepath.Join(o.StateDir, "executors"), 0o700); err != nil {
		return err
	}
	// The files a container provider kept in process under a directory of
	// its own (the ssh provider's pinned host keys) move to its package's
	// directory before any package starts.
	if moved, err := adoptLegacyContainerDirs(o.StateDir); err != nil {
		return fmt.Errorf("adopting in-process container files: %w", err)
	} else if moved > 0 {
		d.log.Info("moved in-process container files to their providers' directories", "files", moved)
	}
	// The playbook plugin exists before the executors that read it start.
	if err = d.preparePlaybooks(); err != nil {
		return err
	}
	if usage != nil {
		usage.known = firstPartyTypes()
	}
	defer d.stopRunning()
	d.openResources()
	d.openSandboxes()
	if err = d.openContainers(ctx); err != nil {
		return err
	}
	d.sites.dir = filepath.Join(d.o.StateDir, "sites")
	// The packages are catalogued before the configuration opens, so their
	// validators check it (07-03), and those the generation it opens uses
	// start first, all at once, so they validate it themselves (#734).
	d.openExtensions(ctx)
	d.external.change.Lock()
	d.startPackages(ctx, d.startingPackages(ctx))
	d.external.change.Unlock()
	// The packages' playbooks join the plugin the executor already reads.
	if err = d.preparePlaybooks(); err != nil {
		return err
	}
	reg := d.configRegistry()
	d.defaultDoc = d.defaultConfig()
	_, leased := store.(*heldDatabase)
	if err = logDrainInForce(ctx, d.coord, d.now(), d.log, o.StateDir, leased); err != nil {
		return err
	}
	if d.configs, err = management.OpenConfig(ctx, d.coord, d.now, reg, o.ConfigPath, d.defaultDoc); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if a := d.configs.Adopted; a != nil {
		d.log.Warn("stored configuration generation no longer validates; adopted the config file", "stored_generation", a.Stored, "path", o.ConfigPath, "cause", a.Cause.Error())
	}
	if d.ingress, err = runtime.NewIngress(d.coord, d.now, runtime.IngressOptions{}); err != nil {
		return err
	}
	snapshot, generation := d.configs.Current()
	// The configuration has the last word on which packages run: those its
	// generation uses that weren't foreseen start now, and those started
	// needlessly stop once it is active and configured.
	d.external.change.Lock()
	d.startWanted(ctx, snapshot)
	d.publishGeneration(snapshot, generation)
	c, err := d.build(snapshot, generation)
	if err != nil {
		d.external.change.Unlock()
		return err
	}
	d.current.Store(c)
	// Extensions are configured once the generation is active, so a
	// configure can arm timers through it, as every later apply can.
	d.configureExtensions(ctx)
	d.stopUnused(ctx, snapshot, snapshot)
	d.external.change.Unlock()
	// A terminal lives in this process, so any on record lost its client
	// with the last one. Its record keeps the Work from starting until what
	// it ran in is gone, so this needs the container providers up.
	d.sweepTerminals(ctx, true)
	// A committed reload swaps generation-pinned components; attempts already
	// running keep their pinned generation through HistoricalExecutors.
	d.configs.OnApply = func(_ context.Context, s *config.Snapshot, g uint64) {
		// The packages the generation uses start before it is built, so it
		// composes them. Those it no longer uses stop only once it is active
		// and they have had its configure, which lets them let go of what
		// they held for their entries; a failed build stops nothing.
		d.external.change.Lock()
		defer d.external.change.Unlock()
		prev, _ := d.extensionSnapshot()
		d.startWanted(d.lifetime(), s)
		next, err := d.build(s, g)
		if err != nil {
			d.log.Error("configuration generation not activated in this process; restart required", "generation", g)
			return
		}
		d.current.Store(next)
		// Nothing a provider served under the last generation outlives it.
		if d.resolver.Cache != nil {
			d.resolver.Cache.Reset()
		}
		d.publishGeneration(s, g)
		d.configureExtensions(d.ctx)
		d.log.Info("configuration generation activated", "generation", g)
		d.stopUnused(d.lifetime(), prev, s)
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
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { d.current.Load().server.ServeHTTP(w, r) })
	srv := &http.Server{Handler: api, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	stopNetwork, err := d.startNetwork(api)
	if err != nil {
		_ = srv.Close()
		return err
	}
	defer stopNetwork()
	d.log.Info("open-conduit serving", "version", o.Version, "commit", o.Commit, "endpoint", d.endpoint, "generation", generation, "owner", d.owner, "dry_run", o.DryRun)
	// A restart resumes the stored generation, never the file: say so when
	// they differ, or an edit looks applied when it is not.
	switch d.configs.DiskStatus() {
	case "default":
		d.log.Info("no config file: running the default configuration, a setup scope; the first apply writes the file", "generation", generation, "config", o.ConfigPath)
	case "pending_edit":
		d.log.Warn("config file differs from the active generation and was not applied; restarting does not apply it", "generation", generation, "config", o.ConfigPath, "apply_with", fmt.Sprintf("open-conduit config apply %s", o.ConfigPath))
	case "invalid":
		d.log.Warn("config file is invalid; the active generation keeps running", "generation", generation, "config", o.ConfigPath, "check_with", "open-conduit config validate "+o.ConfigPath)
	}
	d.startup.listening = d.now()
	if o.Ready != nil {
		o.Ready()
	}
	current := func() *config.Snapshot { s, _ := d.configs.Current(); return s }
	if usage != nil {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			usage.run(loopCtx, usageInterval, current)
		}()
	}

	if err = d.openMetrics(meter); err != nil {
		return err
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.exportMetrics(loopCtx, collector)
	}()
	loopErr := d.loop(loopCtx)
	d.health.phase.Store(management.PhaseStopping)

	// Graceful shutdown: stop serving, stop observing (processes survive) and
	// release live leases so the next daemon can recover them immediately.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	stopNetwork()
	d.endpoints.Shutdown(shutdown)
	stopLoop()
	d.wg.Wait()
	d.releaseAll(shutdown)
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) && loopErr == nil {
		loopErr = err
	}
	if usage != nil {
		usage.stop(current(), loopErr)
	}
	d.log.Info("open-conduit stopped")
	return loopErr
}

func (d *daemon) build(s *config.Snapshot, gen uint64) (*components, error) {
	if s == nil || gen == 0 {
		return nil, storage.ErrInvalid
	}
	c := &components{generation: strconv.FormatUint(gen, 10)}
	set := s.Settings
	limits := runtime.CausalLimits{MaxDepth: set.CausalMaxDepth, PerMinute: set.CausalPerMinute}
	executors, pools := d.executorMap(), d.poolMap()
	bindings := map[string]runtime.RouteBinding{}
	for name, e := range executors {
		bindings[name] = runtime.RouteBinding{Executor: e}
	}
	var err error
	routeHandlers := handlers()
	running := d.extensionPackages()
	packageHandlers(routeHandlers, running)
	if c.router, err = runtime.NewRouter(d.coord, d.now, runtime.RouterOptions{Generation: c.generation, Handlers: routeHandlers, Bindings: bindings, CausalLimits: limits, RouteTimeout: d.o.CallTimeout}); err != nil {
		return nil, err
	}
	var belt *runtime.Toolbelt
	grant := d.configs.Grant(func() []string { return belt.EventTypes() }, describeWorkKind(d.configs.Registry()))
	// The configured causal limits bound toolbelt emissions too (03-08 fix:
	// they were applied only to routing, timers and control, so an execution's
	// emissions pinned the default limits on a new root).
	belt, err = runtime.NewToolbelt(d.coord, d.now, c.router, runtime.ToolbeltOptions{CausalLimits: limits, Authorize: d.configs.Authorize, Served: &d.served, Grant: func(ctx context.Context, state runtime.AttemptState) (runtime.ToolbeltGrant, error) {
		g, err := grant(ctx, state)
		g.Operations = registeredOnly(g.Operations, belt.Operations())
		return g, err
	}, Observer: d.pipeline, AuthorizeAlias: d.authorizeAlias, Refusal: d.configs.Refusal})
	if err != nil {
		return nil, err
	}
	if err = d.registerPackageTools(belt, s); err != nil {
		return nil, err
	}
	c.toolbelt = belt
	// One binary registers the same executor implementations for every
	// generation, so earlier pinned attempts are reinstalled exactly.
	history := map[string]map[string]extension.Executor{}
	for g := uint64(1); g < gen; g++ {
		history[strconv.FormatUint(g, 10)] = executors
	}
	kinds := workKinds(running)
	recurring := recurrences(running)
	delays := make([]time.Duration, len(set.RetryDelaysSeconds))
	for i, seconds := range set.RetryDelaysSeconds {
		delays[i] = time.Duration(seconds) * time.Second
	}
	if c.scheduler, err = runtime.NewScheduler(d.coord, d.now, c.router, runtime.SchedulerOptions{
		Owner: d.owner, LeaseDuration: d.o.LeaseDuration, Generation: c.generation,
		Executors: executors, HistoricalExecutors: history, EventArchive: d.eventArchive(),
		GlobalLimit: set.Concurrency, ExecutorPools: pools, PoolLimits: map[string]int{jobPool: set.JobConcurrency},
		ScopeLimits: scopeLimits(s), AdmissionLimits: admissionLimits(s), Retry: runtime.RetryPolicy{Delays: delays}, CausalLimits: limits,
		Timeout:  time.Duration(set.TimeoutSeconds) * time.Second,
		Toolbelt: belt, ToolbeltEndpoints: d.endpoints.Serve, ToolbeltDir: d.o.ToolbeltDir, PlaybookDir: d.pluginDir(), Secrets: d.configs.Secrets(d.resolver), Served: &d.served, Environment: d.runEnvironment,
		Resources: d.bindResources, Sandbox: d.bindSandbox, Container: d.bindContainer, Isolation: d.configs.Isolation, Signalled: d.containerSignalled, KeptExecutions: set.KeptExecutions, KeptFinished: keptFinished(set.KeptFinishedDays), Artifacts: d.artifacts,
		AuthorizeAlias: d.authorizeAlias, AliasFilter: belt.DependencyFilter,
		WorkKinds: kinds, Budgets: budgets(set.Budgets),
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
	if c.server, err = management.NewServer(management.Options{Store: d.coord, History: storage.EventHistory{Archive: d.eventArchive()}, Artifacts: d.artifacts, Now: d.now, Control: control, AdmitTrace: d.admitTrace, ConfigCandidate: d.configCandidate, Pages: d.pages, Extensions: d.installedExtensions, ExtensionKind: d.packageNamed, Toolbelt: belt, Git: d.git, Config: d.configs, Version: d.o.Version, InstanceID: d.owner, CursorKey: d.cursorKey, Ingress: d.ingress, Scheduler: c.scheduler, Resolver: d.resolver, Secrets: &config.SecretStore{File: d.resolver.SecretsFile}, Verifiers: d.verifier, StartVerifier: d.startVerifier, Checks: d.checks(), Releases: d.o.Releases,
		DeepChecks: d.packageChecks(true), Capacity: d.capacityPools, Annotate: d.annotate, Annotators: d.annotators, Actions: d.actions, Observer: d.pipeline, Attach: d.attach, ContainerAttach: d.containerAttach, ContainerDetach: d.containerDetach, StartTerminal: StartTerminal, Live: d.live, Nudge: d.poke, Components: d.healthComponents, Phase: d.phase, Quiescence: d.quiescence, Drain: daemonDrain{d}, Portability: d.portability, RequestServed: d.requestServed, Diagnostic: func(code string) { d.report("audit", "", code); d.clear("audit", "") }}); err != nil {
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
	checks := append(append([]func(context.Context) runtime.Check{pipelineCheck}, d.packageChecks(false)...), d.filesystemCheck, d.webhookCheck, d.backupCheck, d.recoveryCheck, d.eventArchiveCheck, d.resourceCheck, d.executorStateCheck, d.capacityCheck, d.loopCheck, d.transactionsCheck, d.metricsCheck, d.extensionCheck, d.trustCheck, d.grantCheck)
	if d.policy != nil {
		checks = append(checks, d.policyCheck)
	}
	return checks
}

// scopeLimits gives the scheduler each scope whose limits.concurrency
// narrows its parent's (or settings.concurrency at the root). The scheduler
// checks every ancestor, so an inherited limit needs no entry of its own.
// budgets maps settings.budgets onto the runtime's admission budgets.
func budgets(in []config.Budget) []runtime.Budget {
	out := make([]runtime.Budget, 0, len(in))
	for _, b := range in {
		out = append(out, runtime.Budget{Unit: b.Unit, Per: b.Per, Period: b.Period, ScopeID: domain.ID(b.Scope), Max: b.Max, WarnAt: b.WarnAt, PinnedMax: b.PinnedMax})
	}
	return out
}

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

// admissionLimits gives the scheduler each scope's admission_limit, as
// validation narrowed it down the scope tree from settings.admission_limit.
// The field has no default, so a config that sets it nowhere caps nothing. The scheduler counts a scope's Work against every ancestor
// too, so a parent's limit bounds its descendants together.
func admissionLimits(s *config.Snapshot) map[domain.ID]int {
	out := map[domain.ID]int{}
	for id, sc := range s.Scopes {
		if sc.AdmissionLimit > 0 {
			out[id] = sc.AdmissionLimit
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

// retryPolicy is storage.DefaultRetry for the daemon's store, logging a
// transaction that ran out of attempts: SQLite stayed busy past its busy
// timeout on every run, which is contention worth an operator's notice.
func (d *daemon) retryPolicy() storage.RetryPolicy {
	p := storage.DefaultRetry
	p.Observe = func(n int, final bool, err error) {
		if final {
			d.log.Warn("storage transaction aborted on every attempt", "attempts", n, "error", err.Error())
		}
	}
	return p
}

// report logs a diagnostic once per subject until its code changes, so a
// persistently ineligible item does not flood the log on every pass. A
// cause (runtime.FailureCause: a refusal's code and message) is logged with
// it, so the log says what went wrong, not only that something did.
func (d *daemon) report(kind string, subject domain.ID, code string, cause ...string) {
	key := kind + "\x00" + string(subject)
	d.mu.Lock()
	same := d.reported[key] == code
	if code == "" {
		delete(d.reported, key)
		delete(d.reportedAt, key)
	} else {
		d.reported[key] = code
		d.reportedAt[key] = d.now()
	}
	d.mu.Unlock()
	if !same && code != "" {
		attrs := []any{"kind", kind, "id", string(subject), "code", code}
		if len(cause) > 0 && cause[0] != "" {
			attrs = append(attrs, "cause", cause[0])
		}
		d.log.Info("runtime diagnostic", attrs...)
	}
}

// goSafe runs fn on its own goroutine, counted in d.wg. A panic in it (in
// an extension's projection or task, say) is recovered and logged with its
// stack instead of taking the daemon down; the next pass runs it again.
func (d *daemon) goSafe(kind string, fn func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			if v := recover(); v != nil {
				d.log.Error("recovered from a panic", "kind", kind, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				d.report(kind, "", "panic")
			}
		}()
		fn()
	}()
}

func (d *daemon) clear(kind string, subject domain.ID) {
	d.mu.Lock()
	delete(d.reported, kind+"\x00"+string(subject))
	delete(d.reportedAt, kind+"\x00"+string(subject))
	d.mu.Unlock()
}

// reportedPerRecord are the diagnostic kinds whose subject is one Event,
// Work, execution or intent. There is no end to those, so their keys are
// pruned once they go unreported; every other kind's subject is a
// connection, extension or the daemon itself, a set the config bounds.
var reportedPerRecord = map[string]bool{"route": true, "start": true, "stop": true, "renew": true, "observe": true, "poll": true, "control": true}

// reportedTTL is how long a per-record diagnostic is remembered after it was
// last reported. A condition that persists is reported again on every pass
// and stays; one that stopped (the Work finished, the Event routed) goes.
const reportedTTL = time.Hour

// pruneReported drops per-record diagnostics not reported within
// reportedTTL, so the map follows the live problems, not every Event and
// Work the daemon has ever had trouble with.
func (d *daemon) pruneReported() {
	cutoff := d.now().Add(-reportedTTL)
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, at := range d.reportedAt {
		kind, subject, _ := strings.Cut(key, "\x00")
		if subject != "" && reportedPerRecord[kind] && at.Before(cutoff) {
			delete(d.reported, key)
			delete(d.reportedAt, key)
		}
	}
}

// eventArchive is the open event archive, a nil interface when there is none
// (a daemon built by a test), which every reader treats as no archive.
func (d *daemon) eventArchive() storage.EventArchive {
	if d.events == nil {
		return nil
	}
	return d.events
}

// keptFinished converts settings.kept_finished_days to a duration, capped at
// a century so the largest setting the config allows doesn't overflow.
func keptFinished(days int) time.Duration {
	return time.Duration(min(days, 36500)) * 24 * time.Hour
}
