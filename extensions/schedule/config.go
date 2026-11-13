// Package schedule is the first-party schedule extension (05-01): scheduled
// jobs expressed as timer Events and ordinary Work. A scope's `schedule`
// entry declares its jobs (every N minutes, at UTC times or a cron in a time
// zone, plus webhook, code-host event and manual triggers). Timed jobs are
// configured independent roots on durable core timers whose calendar
// recurrence and six-hour catch-up are this package's pure RecurrenceFunc; a
// one-shot webhook ingress accepts custom POSTs; the deterministic `schedule`
// route turns every trigger into input of one standing `schedule.job` Work per
// job, so a job runs only through event → route → Work → Execution.
//
// It depends only on public contracts (plus the cron parser). It has
// no executor: a job's command is the route's binding.
package schedule

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/robfig/cron/v3"
)

// Type is the registered extension and route handler name.
const Type = "schedule"

// WorkKind is the standing Work one job owns.
const WorkKind = "schedule.job"

// Event types. Every trigger correlates to its job's key.
const (
	// TypeSlot is a timed occurrence (every, at or cron), fired by a core
	// timer as a configured independent root.
	TypeSlot = "schedule.slot"
	// TypeWebhook is a custom POST to the job's webhook URL (one-shot).
	TypeWebhook = "schedule.webhook"
	// TypeEvent is a code-host delivery (for example a GitHub push) that a
	// job's `on.events` selects, derived from the delivery it was read from.
	TypeEvent = "schedule.event"
)

// SchemaTrigger is the payload schema of every schedule trigger.
const SchemaTrigger = "schedule.trigger.v1"

// CatchUp is how late a timed run may start: the latest slot strictly within
// six hours that has not run runs once; a slot missed by six hours or more
// waits for the next.
const CatchUp = 6 * time.Hour

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ExtensionConfig is the opaque config of a `schedule` extension entry.
type ExtensionConfig struct {
	// Namespace is the stable source namespace of the entry's triggers and
	// job keys. It must not change while its jobs have history.
	Namespace string `json:"namespace"`
	// Scope must be the containing scope (ingress target).
	Scope string `json:"scope"`
	// WebhookSecret names the selected secret that signs job webhook URLs.
	// Required when any job takes webhooks.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	Jobs          []Job  `json:"jobs"`
}

// Job is one scheduled job's triggers. What it runs is the binding of the
// `schedule` route that selects it.
type Job struct {
	Name string `json:"name"`
	// EveryMinutes runs the job every N minutes.
	EveryMinutes int `json:"every_minutes,omitempty"`
	// At lists UTC times of day (HH:MM).
	At []string `json:"at,omitempty"`
	// Cron is a standard five-field schedule read in TimeZone (UTC when
	// empty).
	Cron     string `json:"cron,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	// On adds delivery triggers.
	On *Trigger `json:"on,omitempty"`
}

// Trigger runs a job on deliveries. Events are the code host's event names,
// optionally with an action ("push", "release.published"); Branches limits
// ref-carrying events to branch globs; Paths limits a push to those changing
// a matching file (GitHub Actions rules, ! negates); Conclusions limits a
// finished check suite or run to those ending one of these ways ("failure",
// "timed_out"). Webhook accepts custom POSTs at the job's signed URL.
type Trigger struct {
	Events      []string `json:"events,omitempty"`
	Branches    []string `json:"branches,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	Conclusions []string `json:"conclusions,omitempty"`
	Webhook     bool     `json:"webhook,omitempty"`
}

// Conclusions are the ways GitHub says a check suite or run can end, what
// on.conclusions may list.
var Conclusions = []string{"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure"}

// Settings is a validated entry.
type Settings struct {
	Namespace     string
	Scope         string
	WebhookSecret string
	Jobs          []Job
}

// Timed reports whether the job runs at set times (at or cron).
func (j Job) Timed() bool { return len(j.At) > 0 || j.Cron != "" }

// Recurring reports whether the job has a core timer.
func (j Job) Recurring() bool { return j.EveryMinutes > 0 || j.Timed() }

// Webhook reports whether the job takes custom POSTs.
func (j Job) Webhook() bool { return j.On != nil && j.On.Webhook }

// Location is the zone the job's cron is read in.
func (j Job) Location() (*time.Location, error) {
	if j.TimeZone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(j.TimeZone)
}

// Schedule is the job's cron, parsed as standard cron.
func (j Job) Schedule() (cron.Schedule, error) { return cron.ParseStandard(j.Cron) }

// Describe says when the job runs, in words: "every 30 min; on push to
// main". A job with no triggers runs only by hand.
func (j Job) Describe() string {
	var parts []string
	switch m := j.EveryMinutes; {
	case m == 0:
	case m%1440 == 0:
		parts = append(parts, every(m/1440, "day"))
	case m%60 == 0:
		parts = append(parts, every(m/60, "hour"))
	default:
		parts = append(parts, every(m, "minute"))
	}
	if len(j.At) > 0 {
		parts = append(parts, "daily at "+strings.Join(j.At, ", ")+" UTC")
	}
	if j.Cron != "" {
		zone := j.TimeZone
		if zone == "" {
			zone = "UTC"
		}
		parts = append(parts, "cron "+j.Cron+" ("+zone+")")
	}
	if j.On != nil {
		if len(j.On.Events) > 0 {
			on := "on " + strings.Join(j.On.Events, ", ")
			if len(j.On.Branches) > 0 {
				on += " to " + strings.Join(j.On.Branches, ", ")
			}
			if len(j.On.Paths) > 0 {
				on += " changing " + strings.Join(j.On.Paths, ", ")
			}
			if len(j.On.Conclusions) > 0 {
				on += " ending " + strings.Join(j.On.Conclusions, " or ")
			}
			parts = append(parts, on)
		}
		if j.On.Webhook {
			parts = append(parts, "on its webhook")
		}
	}
	if len(parts) == 0 {
		return "by hand only"
	}
	return strings.Join(parts, "; ")
}

func every(n int, unit string) string {
	if n == 1 {
		return "every " + unit
	}
	return "every " + strconv.Itoa(n) + " " + unit + "s"
}

// DescribeRoute says which jobs and triggers a route config takes.
func DescribeRoute(data []byte) string {
	c, err := ParseRoute(data)
	if err != nil {
		return ""
	}
	jobs := "every job"
	if len(c.Jobs) > 0 {
		jobs = strings.Join(c.Jobs, ", ")
	}
	if len(c.Triggers) > 0 {
		return jobs + " (" + strings.Join(c.Triggers, ", ") + ")"
	}
	return jobs
}

// Job finds a job by name.
func (s Settings) Job(name string) (Job, bool) {
	for _, j := range s.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}

func strict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func parseHM(at string) (int, int, bool) {
	h, m, ok := strings.Cut(at, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || len(h) < 1 || len(h) > 2 || len(m) != 2 || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// Validate is what is wrong with the job, each at its field's JSON
// pointer within the job (the run settings that bindings own are not
// checked here). Messages never quote a configured value.
func (j Job) Validate() *extension.ConfigError {
	ce := &extension.ConfigError{}
	if !namePattern.MatchString(j.Name) {
		ce.Add("/name", "invalid_name", "every job needs a name matching [a-z][a-z0-9_-]{0,62}")
	}
	if j.EveryMinutes < 0 || j.EveryMinutes > 525600 {
		ce.Add("/every_minutes", "out_of_range", "every_minutes must be 1 to 525600")
	}
	if j.EveryMinutes > 0 && j.Timed() {
		ce.Add("/every_minutes", "conflicting_fields", "set every_minutes or at/cron, not both")
	}
	if len(j.At) > 0 && j.Cron != "" {
		ce.Add("/at", "conflicting_fields", "set at or cron, not both")
	}
	seen := map[string]bool{}
	for i, at := range j.At {
		if _, _, ok := parseHM(at); !ok {
			ce.Add(extension.ConfigPointer("at", i), "invalid_time", "at times must be HH:MM in UTC, such as 09:30")
		} else if seen[at] {
			ce.Add(extension.ConfigPointer("at", i), "duplicate_time", "this time is already in at; list each time once")
		}
		seen[at] = true
	}
	if j.Cron != "" {
		if strings.HasPrefix(j.Cron, "TZ=") || strings.HasPrefix(j.Cron, "CRON_TZ=") {
			ce.Add("/cron", "time_zone_in_cron", "set the time zone with time_zone, not in cron")
		} else if _, err := j.Schedule(); err != nil {
			ce.Add("/cron", "invalid_cron", "cron must be a standard five-field schedule (minute hour day-of-month month day-of-week), such as 0 9 * * 1-5")
		}
	}
	if j.TimeZone != "" {
		if j.Cron == "" {
			ce.Add("/time_zone", "time_zone_without_cron", "time_zone is for cron; at times are UTC")
		}
		if _, err := j.Location(); err != nil {
			ce.Add("/time_zone", "unknown_time_zone", "time_zone must be an IANA time zone name, such as Europe/London")
		}
	}
	if j.On != nil {
		if len(j.On.Events) == 0 && !j.On.Webhook {
			ce.Add("/on", "missing_trigger", "on needs events or webhook")
		}
		for i, e := range j.On.Events {
			ev, action, has := strings.Cut(e, ".")
			if ev == "" || has && action == "" {
				ce.Add(extension.ConfigPointer("on", "events", i), "empty_event", "an event is a code-host event name, optionally with an action, such as push or release.published")
			}
		}
		for i, b := range j.On.Branches {
			if b == "" {
				ce.Add(extension.ConfigPointer("on", "branches", i), "empty_branch", "a branch pattern must not be empty")
			}
		}
		for i, p := range j.On.Paths {
			if strings.TrimPrefix(p, "!") == "" {
				ce.Add(extension.ConfigPointer("on", "paths", i), "empty_path", "a path pattern must not be empty")
			}
		}
		for i, c := range j.On.Conclusions {
			if !slices.Contains(Conclusions, strings.ToLower(c)) {
				ce.Add(extension.ConfigPointer("on", "conclusions", i), "unknown_conclusion", "not a check conclusion; use "+strings.Join(Conclusions, ", "))
			}
		}
		if (len(j.On.Branches) > 0 || len(j.On.Paths) > 0 || len(j.On.Conclusions) > 0) && len(j.On.Events) == 0 {
			ce.Add("/on/events", "filter_without_events", "branches, paths and conclusions filter events; add events")
		}
	}
	return ce
}

// ParseExtension decodes and validates an entry. secretNames are the entry's
// selected secret names. Its refusal is an *extension.ConfigError.
func ParseExtension(data []byte, secretNames []string) (Settings, error) {
	var c ExtensionConfig
	if err := extension.DecodeConfig(data, &c); err != nil {
		return Settings{}, err
	}
	ce := &extension.ConfigError{}
	if c.Namespace == "" || len(c.Namespace) > 200 || strings.HasPrefix(c.Namespace, "core.") || strings.ContainsAny(c.Namespace, " \t\r\n\x00") {
		ce.Add("/namespace", "invalid_namespace", "namespace must be a nonempty stable identifier of at most 200 characters, without spaces, not starting core.")
	}
	if !namePattern.MatchString(c.Scope) {
		ce.Add("/scope", "invalid_scope", "scope must be the containing scope id")
	}
	names := map[string]bool{}
	webhooks := false
	for i, j := range c.Jobs {
		ce.Merge(extension.ConfigPointer("jobs", i), j.Validate())
		if names[j.Name] {
			ce.Add(extension.ConfigPointer("jobs", i, "name"), "duplicate_name", "another job has this name; give each job its own")
		}
		names[j.Name] = true
		webhooks = webhooks || j.Webhook()
	}
	if c.WebhookSecret != "" {
		if !slices.Contains(secretNames, c.WebhookSecret) {
			ce.Add("/webhook_secret", "secret_not_selected", "webhook_secret must name one of the entry's selected secrets (secret_names)")
		}
	} else if webhooks {
		ce.Add("/webhook_secret", "missing_webhook_secret", "a job takes webhooks: set webhook_secret to one of the entry's selected secrets")
	}
	if err := ce.Err(); err != nil {
		return Settings{}, err
	}
	return Settings{Namespace: c.Namespace, Scope: c.Scope, WebhookSecret: c.WebhookSecret, Jobs: c.Jobs}, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}

// RouteConfig is the opaque config of a `schedule` route: the jobs it
// selects (all jobs of every schedule entry in the scope when empty). One
// route per binding lets each job run its own command. Triggers narrows the
// route to trigger kinds (timer, webhook, event, manual; all when empty), so
// two routes to one binding can give kinds different active policies: a
// detached job can queue a timer slot behind the run going but started
// nothing for a delivery or a manual run (05-02).
type RouteConfig struct {
	Jobs     []string `json:"jobs,omitempty"`
	Triggers []string `json:"triggers,omitempty"`
}

// ParseRoute decodes a route config.
func ParseRoute(data []byte) (RouteConfig, error) {
	var c RouteConfig
	if err := extension.DecodeConfig(data, &c); err != nil {
		return c, err
	}
	ce := &extension.ConfigError{}
	for i, j := range c.Jobs {
		if !namePattern.MatchString(j) {
			ce.Add(extension.ConfigPointer("jobs", i), "invalid_name", "jobs must be job names, matching [a-z][a-z0-9_-]{0,62}")
		}
	}
	for i, k := range c.Triggers {
		switch k {
		case KindTimer, KindWebhook, KindEvent, KindManual:
		default:
			ce.Add(extension.ConfigPointer("triggers", i), "unknown_trigger", "triggers must be timer, webhook, event or manual")
		}
	}
	return c, ce.Err()
}

// ValidateRoute is the config.Registry route handler config validator.
func ValidateRoute(data json.RawMessage) error {
	_, err := ParseRoute(data)
	return err
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// JobKey is the external ID of a job's correlation key in its namespace.
func JobKey(job string) string { return "job/" + job }

// ParseJobKey returns the job named by a key external ID.
func ParseJobKey(id string) (string, bool) {
	name, ok := strings.CutPrefix(id, "job/")
	return name, ok && namePattern.MatchString(name)
}

// WorkID is the stable ID of a job's standing Work.
func WorkID(namespace, job string) string {
	return "schedule/" + digest(namespace, job)[:24] + "/" + job
}

// TimerID is the stable ID of a job's recurrence timer.
func TimerID(namespace, job string) string { return "schedule/" + digest(namespace, job)[:32] }
