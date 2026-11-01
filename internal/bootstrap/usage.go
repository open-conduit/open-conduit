package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/observability"
)

// Anonymous usage telemetry: a daemon reports that it runs, how many scopes
// it has, which first-party extensions and executors its config uses and how
// many runs finished in each state, to the project's PostHog. It sends no
// names, paths, repositories, prompts, outcomes or error text, and a type an
// installed (third-party) package registers is counted, never named. It is on
// by default; serve --no-telemetry, OPEN_CONDUIT_NO_TELEMETRY or DO_NOT_TRACK
// turn it off, and a dev build (no release version) never sends.

// usageKey is the PostHog project API key the reports go to, set at release
// (.goreleaser.yaml, from the POSTHOG_TELEMETRY_KEY repository secret). It
// is PostHog's public "phc_" key, which can send events but never read them.
// Empty sends nothing.
var usageKey = ""

// usageURL is PostHog's EU capture endpoint, where usageKey's project lives.
const usageURL = "https://eu.i.posthog.com/i/v0/e/"

// usageInterval paces the heartbeat a running daemon sends.
const usageInterval = 24 * time.Hour

// usageReporter sends the events and counts the runs between them. It is an
// observability.Exporter, so it counts runs behind the Pipeline's isolation.
type usageReporter struct {
	url, key, id    string
	version, commit string
	client          *http.Client
	now             func() time.Time
	// installed is set when this start created the install ID.
	installed bool
	// known are the first-party extension, connection and executor types.
	known map[string]bool

	mu   sync.Mutex
	runs map[string]int // finished runs by state since the last report
	work int            // Work created since the last report
}

// newUsageReporter returns nil when telemetry is off for this daemon.
func newUsageReporter(o Options) (*usageReporter, error) {
	if o.NoTelemetry {
		return nil, nil
	}
	url, key := o.TelemetryURL, o.TelemetryKey
	if url == "" {
		// Only a release build reports to the project; a dev build (and
		// every test) needs an explicit endpoint.
		if o.Version == "" || o.Version == "dev" {
			return nil, nil
		}
		url = usageURL
	}
	if key == "" {
		key = usageKey
	}
	if key == "" {
		return nil, nil
	}
	id, created, err := installID(o.StateDir)
	if err != nil {
		return nil, err
	}
	return &usageReporter{url: url, key: key, id: id, installed: created, version: o.Version, commit: o.Commit,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now, runs: map[string]int{}}, nil
}

// installID reads the state directory's random install ID, creating it on
// first use. It identifies an install, never a person or machine.
func installID(stateDir string) (id string, created bool, err error) {
	path := filepath.Join(stateDir, "telemetry-id")
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	id = hex.EncodeToString(raw)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", false, err
	}
	return id, true, os.WriteFile(path, []byte(id+"\n"), 0o600)
}

func (u *usageReporter) Export(_ context.Context, records []observability.Record) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, r := range records {
		if r.Name == "work.created" {
			u.work++
			continue
		}
		state, ok := strings.CutPrefix(r.Name, "execution.")
		if ok && domain.ExecutionState(state).Terminal() {
			u.runs[state]++
		}
	}
	return nil
}

func (*usageReporter) Shutdown(context.Context) error { return nil }

// take returns and resets the counts since the last report.
func (u *usageReporter) take() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	out := map[string]any{"work_created": u.work}
	for _, s := range []domain.ExecutionState{domain.ExecutionCompleted, domain.ExecutionFailed, domain.ExecutionWaiting, domain.ExecutionHandoff, domain.ExecutionCancelled} {
		n := u.runs[string(s)]
		out["runs_"+string(s)] = n
		total += n
	}
	out["runs"] = total
	u.runs, u.work = map[string]int{}, 0
	return out
}

// shape is what a configuration uses: scope counts and the sorted
// first-party types, with anything else only counted.
func (u *usageReporter) shape(s *config.Snapshot) map[string]any {
	out := map[string]any{"scopes": 0, "scopes_enabled": 0, "extensions": []string{}, "connections": []string{}, "executors": []string{}, "third_party_types": 0}
	if s == nil {
		return out
	}
	exts, conns, execs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	other := map[string]bool{}
	add := func(set map[string]bool, t string) {
		if t == "" {
			return
		}
		if u.known[t] {
			set[t] = true
		} else {
			other[t] = true
		}
	}
	enabled := 0
	for _, sc := range s.Scopes {
		if sc.Enabled {
			enabled++
		}
		for _, x := range sc.Extensions {
			add(exts, x.Type)
		}
		for _, b := range sc.Bindings {
			add(execs, b.Executor)
		}
	}
	for _, c := range s.Connections {
		add(conns, c.Type)
	}
	sorted := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	out["scopes"], out["scopes_enabled"] = len(s.Scopes), enabled
	out["extensions"], out["connections"], out["executors"] = sorted(exts), sorted(conns), sorted(execs)
	out["third_party_types"] = len(other)
	return out
}

// send captures one event; failures are dropped (telemetry never affects the
// daemon).
func (u *usageReporter) send(ctx context.Context, event string, props map[string]any) error {
	// $geoip_disable stops PostHog adding a location looked up from the
	// sender's IP address.
	all := map[string]any{"version": u.version, "commit": u.commit, "os": goruntime.GOOS, "arch": goruntime.GOARCH,
		"$process_person_profile": false, "$geoip_disable": true, "$lib": "open-conduit"}
	for k, v := range props {
		all[k] = v
	}
	body, err := json.Marshal(map[string]any{"api_key": u.key, "event": event, "distinct_id": u.id, "timestamp": u.now().UTC(), "properties": all})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := u.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode/100 != 2 {
		return errors.New(res.Status)
	}
	return nil
}

// run reports the install (once), the start and a heartbeat every interval
// until ctx ends.
func (u *usageReporter) run(ctx context.Context, interval time.Duration, snapshot func() *config.Snapshot) {
	if u.installed {
		_ = u.send(ctx, "install", nil)
	}
	_ = u.send(ctx, "daemon_started", u.shape(snapshot()))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = u.send(ctx, "daemon_heartbeat", u.report(snapshot()))
		}
	}
}

// stop reports the daemon stopping, with the runs since the last report and
// whether it stopped on an error (its kind only, never its text).
func (u *usageReporter) stop(s *config.Snapshot, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	props := u.report(s)
	switch {
	case err == nil:
		props["error"] = ""
	case errors.Is(err, ErrInstallRemoved):
		props["error"] = "install_removed"
	default:
		props["error"] = "other"
	}
	_ = u.send(ctx, "daemon_stopped", props)
}

// report is a heartbeat's properties: the counts since the last one and the
// configuration's shape.
func (u *usageReporter) report(s *config.Snapshot) map[string]any {
	props := u.take()
	for k, v := range u.shape(s) {
		props[k] = v
	}
	return props
}

// knownTypes are the first-party types a report may name.
func knownTypes(reg config.Registry) map[string]bool {
	out := map[string]bool{}
	for t := range reg.Extensions {
		out[t] = true
	}
	for t := range reg.Connections {
		out[t] = true
	}
	for t := range reg.Executors {
		out[t] = true
	}
	return out
}
