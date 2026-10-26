package introspection

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/open-conduit/open-conduit/domain"
)

// DefaultNotes is the coordination KV namespace of the agents' notes the
// sweep and triage read when an entry names none: the GitHub tools'
// corrections and problems (03-04). It is a data contract (a namespace and
// a JSON record shape), not an import.
const DefaultNotes = "github.notes"

// Config is an entry's opaque config. An empty object enables reporting
// alone (03-08); each optional block adds one feature.
type Config struct {
	// Notes are the KV namespaces whose records (Note) sweep and triage
	// read; default [DefaultNotes].
	Notes []string `json:"notes,omitempty"`
	// Sweep and Triage schedule the daily runs (absent: not scheduled; each
	// can still run by hand as ordinary Work).
	Sweep  *Schedule `json:"sweep,omitempty"`
	Triage *Schedule `json:"triage,omitempty"`
	// HostWatch takes readings of the machine (absent: off).
	HostWatch *HostWatch `json:"host_watch,omitempty"`
	// Telemetry writes the extension's telemetry events (reports, fixes,
	// host readings) to its telemetry log.
	Telemetry bool `json:"telemetry,omitempty"`
}

// Land modes: how a sweep or triage lands its commit (v1's Land line).
const (
	LandPush = "push"
	LandPR   = "pr"
)

// Schedule is when one of the daily runs happens and what its message says.
type Schedule struct {
	// Cron is a standard five-field cron (v1 ran sweep at 41 8 * * * and
	// triage at 51 8 * * * UTC); TimeZone is an IANA name (default UTC).
	Cron     string `json:"cron"`
	TimeZone string `json:"time_zone,omitempty"`
	// Rules are the sweep's rule files (CLAUDE.md, AGENTS.md, rule skills)
	// and Guide the file saying which rule goes where.
	Rules []string `json:"rules,omitempty"`
	Guide string   `json:"guide,omitempty"`
	// Land is push (default) or pr; Remote defaults to origin.
	Land   string `json:"land,omitempty"`
	Remote string `json:"remote,omitempty"`
	// FileIssues tells the triage to open or update an issue for each cause
	// it cannot fix (v1 OpenIssues) instead of only writing them up.
	FileIssues bool `json:"file_issues,omitempty"`
}

// HostWatch configures the host readings (v1 settings.host).
type HostWatch struct {
	// IntervalSeconds between readings (default 60).
	IntervalSeconds int `json:"interval_seconds,omitempty"`
	// Load is the 1-minute load per CPU, Memory and Disk the percentages in
	// use, above which a reading is over (defaults 1.5, 90, 90).
	Load   float64 `json:"load,omitempty"`
	Memory float64 `json:"memory,omitempty"`
	Disk   float64 `json:"disk,omitempty"`
	// Disks are paths whose filesystems are watched besides the state
	// directory's.
	Disks []string `json:"disks,omitempty"`
}

// Thresholds are the watch's thresholds with defaults for any left out.
func (h HostWatch) Thresholds() (load, memory, disk float64) {
	pick := func(v, d float64) float64 {
		if v > 0 {
			return v
		}
		return d
	}
	return pick(h.Load, 1.5), pick(h.Memory, 90), pick(h.Disk, 90)
}

// Interval is the time between readings.
func (h HostWatch) Interval() time.Duration {
	if h.IntervalSeconds > 0 {
		return time.Duration(h.IntervalSeconds) * time.Second
	}
	return time.Minute
}

// NoteNamespaces are the namespaces sweep and triage read.
func (c Config) NoteNamespaces() []string {
	if len(c.Notes) == 0 {
		return []string{DefaultNotes}
	}
	return c.Notes
}

// ParseConfig decodes and validates an entry's config.
func ParseConfig(data json.RawMessage) (Config, error) {
	var c Config
	if err := strict(data, &c); err != nil {
		return c, err
	}
	for _, ns := range c.Notes {
		if ns == "" || len(ns) > 128 || strings.HasPrefix(ns, "core.") || strings.ContainsAny(ns, " /\t\r\n") {
			return c, fmt.Errorf("notes: invalid namespace %q", ns)
		}
	}
	for name, s := range map[string]*Schedule{WorkSweep: c.Sweep, WorkTriage: c.Triage} {
		if s == nil {
			continue
		}
		if err := s.validate(); err != nil {
			return c, fmt.Errorf("%s: %w", name, err)
		}
		if name == WorkTriage && (len(s.Rules) > 0 || s.Guide != "") {
			return c, errors.New("triage: rules and guide are sweep settings")
		}
		if name == WorkSweep && s.FileIssues {
			return c, errors.New("sweep: file_issues is a triage setting")
		}
	}
	if h := c.HostWatch; h != nil {
		if h.IntervalSeconds < 0 || h.IntervalSeconds > 0 && h.IntervalSeconds < 5 || h.Load < 0 || h.Memory < 0 || h.Memory > 100 || h.Disk < 0 || h.Disk > 100 {
			return c, errors.New("host_watch: interval_seconds is at least 5, thresholds are positive and percentages at most 100")
		}
		for _, d := range h.Disks {
			if d == "" {
				return c, errors.New("host_watch: empty disk path")
			}
		}
	}
	return c, nil
}

func (s *Schedule) validate() error {
	if _, err := cron.ParseStandard(s.Cron); err != nil || strings.HasPrefix(strings.TrimSpace(s.Cron), "@every") {
		return fmt.Errorf("cron %q is not a standard five-field cron", s.Cron)
	}
	if _, err := s.Location(); err != nil {
		return fmt.Errorf("time_zone %q: %w", s.TimeZone, err)
	}
	if s.Land != "" && s.Land != LandPush && s.Land != LandPR {
		return fmt.Errorf("land must be %s or %s", LandPush, LandPR)
	}
	if strings.ContainsAny(s.Remote, " \t\r\n") || strings.ContainsAny(s.Guide, "\r\n") || slices.ContainsFunc(s.Rules, func(r string) bool { return r == "" || strings.ContainsAny(r, "\r\n") }) {
		return errors.New("remote, guide and rules must be single non-empty lines")
	}
	return nil
}

// Location is the schedule's time zone.
func (s Schedule) Location() (*time.Location, error) {
	if s.TimeZone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(s.TimeZone)
}

// ValidateExtension validates an `introspection` entry: its config and no
// secrets.
func ValidateExtension(config json.RawMessage, secretNames []string) error {
	if len(secretNames) > 0 {
		return errors.New("introspection takes no secrets")
	}
	_, err := ParseConfig(config)
	return err
}

// scopeEntry is the effective `introspection` entry of a scope
// (core.config.scope.v1 metadata): ok only when the scope is enabled and has
// one.
func scopeEntry(scope domain.Scope) (Config, bool) {
	var meta struct {
		Enabled    bool `json:"enabled"`
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil || !meta.Enabled {
		return Config{}, false
	}
	for _, x := range meta.Extensions {
		if x.Type == Type {
			c, err := ParseConfig(x.Config)
			if err != nil {
				return Config{}, true
			}
			return c, true
		}
	}
	return Config{}, false
}

// Enabled reports whether the effective scope (core.config.scope.v1
// metadata) is enabled and has an `introspection` entry.
func Enabled(scope domain.Scope) bool {
	_, ok := scopeEntry(scope)
	return ok
}
