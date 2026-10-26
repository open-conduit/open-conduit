package introspection

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/introspection/host"
	"github.com/open-conduit/open-conduit/storage"
)

// Extension-originated reports (05-03). An adapter of the extension itself,
// not an execution, can report a defect: the optional host watch reports a
// reading crossing its threshold. Such a report is a root Event in the
// scope's report namespace (it has no causal parent; it is its own origin)
// with the same index record and the same `introspection` route as any
// other, so its processing, any triage of it and any fix it leads to trace
// back to it through causation, under the same core limits.

// OriginHostWatch names the host watch as a report's origin.
const OriginHostWatch = "host-watch"

// OriginReport builds an extension-originated report Event and its record.
// delivery identifies the occurrence within origin, so a repeated attempt
// to report it is deduplicated by acceptance.
func OriginReport(scope domain.ID, origin, delivery, kind, title, body string, now time.Time) (domain.Event, Report) {
	id := digest(string(scope), origin, delivery)[:20]
	key := ReportKey(scope, id)
	r := Report{ID: id, Scope: string(scope), Kind: kind, Title: title, Body: body, Key: key, Origin: origin}
	data, _ := json.Marshal(r)
	e := domain.Event{ID: domain.ID("introspection-" + digest(string(scope), origin, delivery, "report")[:40]), Type: TypeReported, Source: SourceNamespace(scope),
		DeliveryID: origin + "/" + delivery, OccurredAt: now.UTC(), ReceivedAt: now.UTC(), CorrelationKey: &key, Data: domain.Payload{Schema: SchemaReport, Data: data}}
	return e, r
}

// PutOriginInTx writes an accepted extension-originated report's record.
// Call it only when acceptance newly added the Event.
func PutOriginInTx(tx storage.Tx, accepted domain.AcceptedEvent, r Report) error {
	r.Event, r.Root, r.Depth = string(accepted.Event.ID), string(accepted.Event.ID), 0
	r.At, r.State = accepted.Event.ReceivedAt.UTC(), StateOpen
	value, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return tx.Put(Namespace, recordKey(domain.ID(r.Scope), accepted.Sequence, r.ID), value)
}

// recoverAt is the share of a threshold a reading must fall back under
// before it counts as recovered (v1), so one hovering at the line doesn't
// alert on every reading.
const recoverAt = 0.95

// HostAlert is a reading crossing its threshold: going over it, or back
// under.
type HostAlert struct {
	Metric    string // load, memory or disk
	Path      string // the disk's path
	Value     float64
	Threshold float64
	Over      bool
}

// Key identifies the reading.
func (a HostAlert) Key() string {
	if a.Path != "" {
		return a.Metric + ":" + a.Path
	}
	return a.Metric
}

func (a HostAlert) String() string {
	what := a.Metric
	if a.Path != "" {
		what += " " + a.Path
	}
	value, threshold := fmt.Sprintf("%.0f%%", a.Value), fmt.Sprintf("%.0f%%", a.Threshold)
	if a.Metric == "load" {
		value, threshold = fmt.Sprintf("%.2f per CPU", a.Value), fmt.Sprintf("%.2f", a.Threshold)
	}
	if a.Over {
		return fmt.Sprintf("host: %s is at %s, over the %s threshold", what, value, threshold)
	}
	return fmt.Sprintf("host: %s is back to %s, under the %s threshold", what, value, threshold)
}

// HostReport is the extension-originated report of a reading going over its
// threshold. at (the reading's time) makes each crossing its own report.
func HostReport(scope domain.ID, a HostAlert, at time.Time) (domain.Event, Report) {
	body := fmt.Sprintf("%s.\n\nThe host watch took this reading at %s. Agents, their tools and their workspaces share this machine, so every scope on it slows or stalls until it recovers. Free what is filling it, or raise the threshold in the introspection entry's host_watch if this level is expected.", a, at.UTC().Format(time.RFC3339))
	return OriginReport(scope, OriginHostWatch, a.Key()+"/"+at.UTC().Format(time.RFC3339), "env", a.String(), body, at)
}

// HostWatcher remembers which readings are over their thresholds (v1
// hostWatch). It is in memory: after a restart a reading still over is a new
// crossing.
type HostWatcher struct {
	mu     sync.Mutex
	over   map[string]bool
	sample *host.Sample
}

// Update compares a reading with the thresholds and returns what crossed
// since the last.
func (w *HostWatcher) Update(s host.Sample, h HostWatch) []HostAlert {
	load, memory, disk := h.Thresholds()
	type reading struct {
		key, metric, path string
		value, threshold  float64
	}
	var rs []reading
	if s.HasLoad {
		rs = append(rs, reading{"load", "load", "", s.LoadPerCPU(), load})
	}
	if s.MemTotal > 0 {
		rs = append(rs, reading{"memory", "memory", "", s.MemoryUsedPct(), memory})
	}
	for _, d := range s.Disks {
		rs = append(rs, reading{"disk:" + d.Path, "disk", d.Path, d.UsedPct(), disk})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over == nil {
		w.over = map[string]bool{}
	}
	sample := s
	w.sample = &sample
	var out []HostAlert
	for _, r := range rs {
		was := w.over[r.key]
		now := r.value > r.threshold || (was && r.value > r.threshold*recoverAt)
		if now != was {
			w.over[r.key] = now
			out = append(out, HostAlert{Metric: r.metric, Path: r.path, Value: r.value, Threshold: r.threshold, Over: now})
		}
	}
	return out
}

// Over lists the readings over their thresholds now, e.g. "disk / 93%".
func (w *HostWatcher) Over() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sample == nil {
		return nil
	}
	s := *w.sample
	var out []string
	if w.over["load"] {
		out = append(out, fmt.Sprintf("load %.1f/cpu", s.LoadPerCPU()))
	}
	if w.over["memory"] {
		out = append(out, fmt.Sprintf("memory %d%%", int(math.Round(s.MemoryUsedPct()))))
	}
	for _, d := range s.Disks {
		if w.over["disk:"+d.Path] {
			out = append(out, fmt.Sprintf("disk %s %d%%", d.Path, int(math.Round(d.UsedPct()))))
		}
	}
	return out
}

// HostProps are a reading's telemetry properties (v1 hostProps): each disk
// by path, plus the fullest under disk_*.
func HostProps(s host.Sample) map[string]any {
	props := map[string]any{"cpus": s.CPUs}
	if s.HasLoad {
		props["load_1"], props["load_5"], props["load_15"] = s.Load[0], s.Load[1], s.Load[2]
		props["load_per_cpu"] = s.LoadPerCPU()
	}
	if s.MemTotal > 0 {
		props["memory_total_bytes"] = s.MemTotal
		props["memory_available_bytes"] = s.MemAvailable
		props["memory_used_pct"] = s.MemoryUsedPct()
	}
	disks := append([]host.Disk(nil), s.Disks...)
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].UsedPct() > disks[j].UsedPct() })
	var list []map[string]any
	for _, d := range disks {
		list = append(list, map[string]any{"path": d.Path, "total_bytes": d.Total, "free_bytes": d.Free, "used_pct": d.UsedPct()})
	}
	if len(disks) > 0 {
		props["disk_path"] = disks[0].Path
		props["disk_total_bytes"] = disks[0].Total
		props["disk_free_bytes"] = disks[0].Free
		props["disk_used_pct"] = disks[0].UsedPct()
		props["disks"] = list
	}
	return props
}
