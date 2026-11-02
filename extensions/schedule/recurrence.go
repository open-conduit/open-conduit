package schedule

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// RecurrenceKind names this package's RecurrenceFunc in the runtime's timer
// options.
const RecurrenceKind = "schedule.v1"

// Trigger kinds in TriggerPayload.Kind.
const (
	KindTimer   = "timer"
	KindWebhook = "webhook"
	KindEvent   = "event"
	KindManual  = "manual"
)

// TriggerPayload is the schedule.trigger.v1 payload of every trigger the
// extension produces.
type TriggerPayload struct {
	Job  string `json:"job"`
	Kind string `json:"kind"`
	// Trigger is the run's label, as v1 recorded it ("cron 2026-10-25 01:30
	// Europe/London", "at 2026-10-25 05:17 UTC", "every 60m", "webhook",
	// "push abc1234").
	Trigger string `json:"trigger"`
	// Slot is a timed occurrence's wall-clock name and At its instant.
	Slot string     `json:"slot,omitempty"`
	At   *time.Time `json:"at,omitempty"`
	// Spec is the timing hash the timer was armed with; Last is the name of
	// the slot that ran before this one. Both are timer bookkeeping.
	Spec string `json:"spec,omitempty"`
	Last string `json:"last,omitempty"`
	// Code-host delivery fields (TypeEvent).
	Event  string `json:"event,omitempty"`
	Action string `json:"action,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Before string `json:"before,omitempty"`
	SHA    string `json:"sha,omitempty"`
	// Conclusion is how a finished check suite or run ended.
	Conclusion string   `json:"conclusion,omitempty"`
	Files      []string `json:"files,omitempty"`
	Delivery   string   `json:"delivery,omitempty"`
	// Webhook body (TypeWebhook): JSON when the body is JSON, else Text.
	ContentType string          `json:"content_type,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Text        string          `json:"text,omitempty"`
}

// Payload decodes a schedule trigger's payload.
func Payload(e domain.Event) (TriggerPayload, bool) {
	var p TriggerPayload
	if e.Data.Schema != SchemaTrigger || json.Unmarshal(e.Data.Data, &p) != nil {
		return p, false
	}
	return p, true
}

// Spec is the opaque recurrence spec stored with a job's timer: its timing
// only, so editing a job's other triggers never rearms it.
type Spec struct {
	Namespace string `json:"namespace"`
	Job       Job    `json:"job"`
	Hash      string `json:"hash"`
}

func timing(j Job) Job {
	return Job{Name: j.Name, EveryMinutes: j.EveryMinutes, At: append([]string(nil), j.At...), Cron: j.Cron, TimeZone: j.TimeZone}
}

// TimingHash identifies a job's timing; a change rearms its timer.
func TimingHash(namespace string, j Job) string {
	return digest(namespace, j.Name, strconv.Itoa(j.EveryMinutes), strings.Join(j.At, ","), j.Cron, j.TimeZone)[:16]
}

// NewSpec is the recurrence spec of a timed job.
func NewSpec(namespace string, j Job) Spec {
	return Spec{Namespace: namespace, Job: timing(j), Hash: TimingHash(namespace, j)}
}

// SlotEvent is the trigger Event of one timed occurrence. Its identity is the
// occurrence's instant, so a replayed firing or a repeated catch-up of the
// same occurrence is deduplicated by acceptance.
func (sp Spec) SlotEvent(s Slot, last string) domain.Event {
	instant := s.At.UTC().Format(time.RFC3339)
	at := s.At.UTC()
	p := TriggerPayload{Job: sp.Job.Name, Kind: KindTimer, Trigger: sp.Job.Label(s), Slot: s.Name, At: &at, Spec: sp.Hash, Last: last}
	data, _ := json.Marshal(p)
	return domain.Event{ID: domain.ID("schedule-" + digest(sp.Namespace, sp.Job.Name, instant)[:40]), Type: TypeSlot, Source: sp.Namespace,
		DeliveryID: "slot/" + sp.Job.Name + "/" + instant, OccurredAt: at, ReceivedAt: at,
		CorrelationKey: &domain.CorrelationKey{Source: sp.Namespace, ExternalID: JobKey(sp.Job.Name)},
		Data:           domain.Payload{Schema: SchemaTrigger, Data: data}}
}

// Timer is the job's recurrence timer armed for slot s.
func (sp Spec) Timer(s Slot, last string) domain.Timer {
	return domain.Timer{ID: domain.ID(TimerID(sp.Namespace, sp.Job.Name)), FireAt: s.At.UTC(), Event: sp.SlotEvent(s, last)}
}

func everySlot(t time.Time) Slot {
	t = t.UTC()
	return Slot{At: t, Name: t.Format(time.RFC3339)}
}

// Recurrence is the RecurrenceFunc of RecurrenceKind. It runs inside the
// core firing transaction and is pure.
//
// At and cron: the firing publishes the latest slot strictly within six hours
// of now, unless that slot's name is the one that ran last; otherwise it
// publishes nothing (the slot is skipped). Older missed slots are skipped
// either way. The timer then rearms for the next slot after now whose name
// differs from the one that ran last.
//
// Every N minutes: the firing publishes its own occurrence (v1 ran an
// interval job at start too) and rearms N minutes after it, or N minutes
// after now when that is already past, so an outage yields one run rather
// than a burst.
func Recurrence(spec []byte, fired domain.Timer, now time.Time) (*domain.Event, *domain.Timer, error) {
	var sp Spec
	if err := json.Unmarshal(spec, &sp); err != nil || sp.Namespace == "" || sp.Job.Name == "" {
		return nil, nil, errors.New("schedule: invalid recurrence spec")
	}
	p, _ := Payload(fired.Event)
	job := sp.Job
	if job.EveryMinutes > 0 {
		occurrence := fired.Event
		interval := time.Duration(job.EveryMinutes) * time.Minute
		next := fired.FireAt.Add(interval)
		if !next.After(now) {
			next = now.Truncate(time.Second).Add(interval)
		}
		timer := sp.Timer(everySlot(next), "")
		return &occurrence, &timer, nil
	}
	if !job.Timed() {
		return nil, nil, errors.New("schedule: recurrence spec has no timing")
	}
	last := p.Last
	var occurrence *domain.Event
	if l, ok := Latest(job, now); ok && l.Name != last {
		e := sp.SlotEvent(l, last)
		occurrence, last = &e, l.Name
	}
	n, ok := Next(job, now, last)
	if !ok {
		return occurrence, nil, nil
	}
	timer := sp.Timer(n, last)
	return occurrence, &timer, nil
}

// Desired is the timer a job should have at now, given its current timer
// (nil when it has none). changed is false when the current timer already
// matches the job's timing; a nil timer with changed true means the job can
// have no timer (its cron never fires) and the current one goes. It ports
// v1's timer start rules:
//
//   - A new timed job waits for its next slot rather than running a slot
//     someone may already have run (v1 "first start").
//   - A new interval job runs at once, as v1's ticker did.
//   - An edited timing rearms: an interval job runs at once; a timed job runs
//     its latest slot within six hours if that slot's name is not the one
//     that ran last (v1 kept LastSlot across a timer restart), else waits.
//
// An unchanged timer is left alone across restarts, so a missed slot is
// caught up by the timer's own firing.
func Desired(namespace string, j Job, now time.Time, current *domain.Timer) (timer *domain.Timer, spec []byte, changed bool) {
	sp := NewSpec(namespace, j)
	spec, _ = json.Marshal(sp)
	var last string
	if current != nil {
		p, _ := Payload(current.Event)
		if p.Spec == sp.Hash {
			return current, spec, false
		}
		last = p.Last
	}
	if j.EveryMinutes > 0 {
		t := sp.Timer(everySlot(now.Truncate(time.Second)), "")
		return &t, spec, true
	}
	if current == nil {
		if l, ok := Latest(j, now); ok {
			last = l.Name
		}
	} else if l, ok := Latest(j, now); ok && l.Name != last {
		t := sp.Timer(l, last)
		return &t, spec, true
	}
	n, ok := Next(j, now, last)
	if !ok {
		return nil, spec, current != nil
	}
	t := sp.Timer(n, last)
	return &t, spec, true
}
