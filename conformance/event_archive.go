package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// The event archive suite proves a storage.EventArchiveBackend (#704): an
// Event's whole record survives leaving the live log, a pass that crashed
// can run again, nothing that has already lost its payload is taken for the
// fact, and reads come back in sequence order however the archive is
// partitioned.

// EventArchiveHarness is what the suite needs from a backend's own test.
type EventArchiveHarness struct {
	Backend storage.EventArchiveBackend
	// Location names a fresh, empty archive, using dir (a private temporary
	// directory) for any files.
	Location func(t *testing.T, dir string) string
}

// EventArchiveCase is one archive guarantee.
type EventArchiveCase struct {
	Name  string
	Check func(*testing.T, EventArchiveHarness) error
}

// RunEventArchive runs every archive case against h.
func RunEventArchive(t *testing.T, h EventArchiveHarness) {
	t.Helper()
	for _, c := range EventArchiveCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// EventArchiveCases lists the suite.
func EventArchiveCases() []EventArchiveCase {
	return []EventArchiveCase{
		{"keeps_the_whole_record", archiveRoundTrip},
		{"put_again_is_nil_and_a_different_record_conflicts", archiveIdempotent},
		{"refuses_a_tombstone_or_unrouted_event_and_stores_none_of_the_batch", archiveRefuses},
		{"reads_merge_periods_in_sequence_order", archiveOrder},
		{"work_events_follow_naming_and_routing", archiveWorkEvents},
		{"records_survive_reopen", archiveReopen},
	}
}

var archiveCtx = context.Background()

var archiveMonth = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func archived(id domain.ID, sequence uint64, received time.Time) domain.AcceptedEvent {
	e := event(id, received)
	e.Data = domain.Payload{Schema: "test.archived.v1", Data: []byte(`{"id":"` + string(id) + `"}`)}
	return domain.AcceptedEvent{Event: e, Sequence: sequence, Routed: true, Disposition: "unmatched", ScopeID: "scope"}
}

func openArchive(t *testing.T, h EventArchiveHarness) (storage.EventArchive, string, error) {
	t.Helper()
	location := h.Location(t, t.TempDir())
	a, err := h.Backend.Open(archiveCtx, location)
	if err != nil {
		return nil, "", err
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, location, nil
}

func archiveIDs(events []domain.AcceptedEvent) []domain.ID {
	out := make([]domain.ID, 0, len(events))
	for _, e := range events {
		out = append(out, e.Event.ID)
	}
	return out
}

func archiveRoundTrip(t *testing.T, h EventArchiveHarness) error {
	a, _, err := openArchive(t, h)
	if err != nil {
		return err
	}
	want := archived("whole", 7, archiveMonth)
	key := domain.CorrelationKey{Source: "test", ExternalID: "thread"}
	want.Event.CorrelationKey, want.Event.Actor, want.RoutedWorkID = &key, "operator:alice", "work"
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{want}); err != nil {
		return err
	}
	got, err := a.Event(archiveCtx, "whole")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("archived record changed: got %+v, want %+v", got, want)
	}
	if _, err = a.Event(archiveCtx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("unknown event: got %v, want not found", err)
	}
	return nil
}

func archiveIdempotent(t *testing.T, h EventArchiveHarness) error {
	a, _, err := openArchive(t, h)
	if err != nil {
		return err
	}
	first := archived("first", 1, archiveMonth)
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{first}); err != nil {
		return err
	}
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{first, first}); err != nil {
		return fmt.Errorf("the same record again: %w", err)
	}
	// A crashed pass can run again under another binary or live backend,
	// which reads the same instants back in another zone.
	moved := first
	zone := time.FixedZone("elsewhere", 5*60*60)
	moved.Event.OccurredAt, moved.Event.ReceivedAt = first.Event.OccurredAt.In(zone), first.Event.ReceivedAt.In(zone)
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{moved}); err != nil {
		return fmt.Errorf("the same record in another zone: %w", err)
	}
	changed := first
	changed.Event.Data = domain.Payload{Schema: "test.archived.v1", Data: []byte(`{"id":"other"}`)}
	bystander := archived("bystander", 2, archiveMonth)
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{bystander, changed}); !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("a different record under a held ID: got %v, want conflict", err)
	}
	if got, e := a.Event(archiveCtx, "first"); e != nil || !reflect.DeepEqual(got, first) {
		return fmt.Errorf("held record overwritten: got %+v, %v", got, e)
	}
	if _, e := a.Event(archiveCtx, "bystander"); !errors.Is(e, storage.ErrNotFound) {
		return fmt.Errorf("a refused batch stored part of itself: %v", e)
	}
	// In another period, so a partition can't answer the check alone.
	sameSequence := archived("other", 1, archiveMonth.AddDate(0, 2, 0))
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{sameSequence}); !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("a different record under a held sequence: got %v, want conflict", err)
	}
	return nil
}

func archiveRefuses(t *testing.T, h EventArchiveHarness) error {
	a, _, err := openArchive(t, h)
	if err != nil {
		return err
	}
	tombstone := archived("tombstone", 2, archiveMonth)
	tombstone.Retired, tombstone.Event.Data = true, domain.Payload{}
	unrouted := archived("unrouted", 3, archiveMonth)
	unrouted.Routed, unrouted.Disposition = false, ""
	unsequenced := archived("unsequenced", 0, archiveMonth)
	for name, bad := range map[string]domain.AcceptedEvent{"a tombstone": tombstone, "an unrouted event": unrouted, "an event with no sequence": unsequenced} {
		good := archived("good", 1, archiveMonth)
		if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{good, bad}); !errors.Is(err, storage.ErrInvalid) {
			return fmt.Errorf("archiving %s: got %v, want invalid", name, err)
		}
		if _, err = a.Event(archiveCtx, "good"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("a batch with %s stored part of itself: %v", name, err)
		}
	}
	return nil
}

func archiveOrder(t *testing.T, h EventArchiveHarness) error {
	a, _, err := openArchive(t, h)
	if err != nil {
		return err
	}
	// Sequence and time disagree, as they do after a clock step.
	batch := []domain.AcceptedEvent{
		archived("c", 3, archiveMonth),
		archived("a", 1, archiveMonth.AddDate(0, 2, 0)),
		archived("d", 4, archiveMonth.AddDate(0, 1, 0)),
		archived("b", 2, archiveMonth.AddDate(0, 1, 0)),
	}
	if err = a.PutEvents(archiveCtx, batch); err != nil {
		return err
	}
	for _, q := range []struct {
		after uint64
		limit int
		want  []domain.ID
	}{
		{0, 0, []domain.ID{"a", "b", "c", "d"}},
		{0, 2, []domain.ID{"a", "b"}},
		{2, 1, []domain.ID{"c"}},
		{4, 0, []domain.ID{}},
	} {
		got, err := a.Events(archiveCtx, q.after, q.limit)
		if err != nil {
			return err
		}
		if ids := archiveIDs(got); !reflect.DeepEqual(ids, q.want) {
			return fmt.Errorf("events after %d limit %d out of sequence order: got %v, want %v", q.after, q.limit, ids, q.want)
		}
	}
	return nil
}

func archiveWorkEvents(t *testing.T, h EventArchiveHarness) error {
	a, _, err := openArchive(t, h)
	if err != nil {
		return err
	}
	named := archived("named", 1, archiveMonth)
	named.Event.WorkID = "work"
	routed := archived("routed", 2, archiveMonth.AddDate(0, 1, 0))
	routed.RoutedWorkID = "work"
	other := archived("other", 3, archiveMonth)
	other.Event.WorkID = "another"
	if err = a.PutEvents(archiveCtx, []domain.AcceptedEvent{named, routed, other}); err != nil {
		return err
	}
	got, err := a.WorkEvents(archiveCtx, "work", 0, 0)
	if err != nil {
		return err
	}
	if ids := archiveIDs(got); !reflect.DeepEqual(ids, []domain.ID{"named", "routed"}) {
		return fmt.Errorf("work events: got %v, want named and routed", ids)
	}
	if got, err = a.WorkEvents(archiveCtx, "work", 1, 1); err != nil {
		return err
	}
	if ids := archiveIDs(got); !reflect.DeepEqual(ids, []domain.ID{"routed"}) {
		return fmt.Errorf("work events after a cursor: got %v, want routed", ids)
	}
	return nil
}

func archiveReopen(t *testing.T, h EventArchiveHarness) error {
	a, location, err := openArchive(t, h)
	if err != nil {
		return err
	}
	want := []domain.AcceptedEvent{archived("older", 1, archiveMonth), archived("newer", 2, archiveMonth.AddDate(0, 1, 0))}
	if err = a.PutEvents(archiveCtx, want); err != nil {
		return err
	}
	if err = a.Close(); err != nil {
		return err
	}
	if _, err = a.Events(archiveCtx, 0, 0); !errors.Is(err, storage.ErrInvalid) {
		return fmt.Errorf("read after close: got %v, want invalid", err)
	}
	again, err := h.Backend.Open(archiveCtx, location)
	if err != nil {
		return err
	}
	defer again.Close()
	got, err := again.Events(archiveCtx, 0, 0)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("records after reopen: got %v, want %v", archiveIDs(got), archiveIDs(want))
	}
	return nil
}
