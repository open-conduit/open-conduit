package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// The inbox is the codex executor's inject channel (the generic inject
// acknowledgement). `codex exec` takes no input once a turn has started,
// so an injected Event becomes a follow-up turn on the same thread within
// the same segment. A segment's run directory holds inbox/. Inject writes
// each accepted Event there as <order>-<id digest>.json. When a turn ends,
// the segment driver reads what is pending, starts `codex exec resume`
// with it and, once that turn has run, acknowledges each event with
// inbox/consumed/<id digest>. The result reports acknowledged events, and
// only those core put in the inbox, as Result.ConsumedInputs. The inbox
// lives on disk, so an adopted segment keeps receiving input; once the
// result is recorded nothing more is accepted.

const inboxDir = "inbox"

func eventDigest(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:12])
}

// Inject delivers an accepted Event to the running segment's inbox. Success
// means the file is durably in the inbox, not that the process read it.
func (e *Executor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev.ID == "" {
		return extension.ErrInvalid
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return extension.ErrNotAccepting
	}
	// The finalize lock orders every delivery before or after the result.
	unlock, err := lockFile(filepath.Join(rn.dir, "finalize.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok, err := readResult(rn.dir); err != nil {
		return err
	} else if ok {
		return extension.ErrNotAccepting
	}
	dir := filepath.Join(rn.dir, inboxDir)
	if err := os.MkdirAll(filepath.Join(dir, "consumed"), 0o700); err != nil {
		return err
	}
	digest := eventDigest(ev.ID)
	entries, err := inboxEntries(dir)
	if err != nil {
		return err
	}
	for _, name := range entries {
		if strings.HasSuffix(strings.TrimSuffix(name, ".json"), "-"+digest) {
			return nil // a retried dispatch of the same Event
		}
	}
	return writeJSON(filepath.Join(dir, fmt.Sprintf("%06d-%s.json", len(entries)+1, digest)), toInput(ev))
}

// inboxEntries lists delivered event files in delivery order.
func inboxEntries(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".json") && !strings.HasPrefix(f.Name(), ".") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// consumedInputs is the proof for a finishing segment: the delivered events
// the process acknowledged, in delivery order. An acknowledgement for anything
// not delivered is ignored.
func consumedInputs(runDir string) []domain.ID {
	dir := filepath.Join(runDir, inboxDir)
	names, err := inboxEntries(dir)
	if err != nil {
		return nil
	}
	var out []domain.ID
	for _, name := range names {
		digest := name[strings.LastIndex(name, "-")+1 : len(name)-len(".json")]
		if _, err := os.Stat(filepath.Join(dir, "consumed", digest)); err != nil {
			continue
		}
		var ev inputEvent
		if readJSON(filepath.Join(dir, name), &ev) != nil || ev.ID == "" || eventDigest(domain.ID(ev.ID)) != digest {
			continue
		}
		out = append(out, domain.ID(ev.ID))
	}
	return out
}

type inputEvent struct {
	ID             string                 `json:"id"`
	Type           string                 `json:"type"`
	Source         string                 `json:"source"`
	DeliveryID     string                 `json:"delivery_id"`
	OccurredAt     time.Time              `json:"occurred_at"`
	CorrelationKey *domain.CorrelationKey `json:"correlation_key,omitempty"`
	CausationID    string                 `json:"causation_id,omitempty"`
	Data           *inputPayload          `json:"data,omitempty"`
}
type inputPayload struct {
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

func toInput(ev domain.Event) inputEvent {
	out := inputEvent{ID: string(ev.ID), Type: ev.Type, Source: ev.Source, DeliveryID: ev.DeliveryID, OccurredAt: ev.OccurredAt, CorrelationKey: ev.CorrelationKey, CausationID: string(ev.CausationID)}
	if ev.Data.Schema != "" || len(ev.Data.Data) > 0 {
		out.Data = &inputPayload{Schema: ev.Data.Schema, Data: ev.Data.Data}
	}
	return out
}

func (in inputEvent) event() domain.Event {
	ev := domain.Event{ID: domain.ID(in.ID), Type: in.Type, Source: in.Source, DeliveryID: in.DeliveryID, OccurredAt: in.OccurredAt}
	if in.Data != nil {
		ev.Data = domain.Payload{Schema: in.Data.Schema, Data: in.Data.Data}
	}
	return ev
}

// pendingInbox lists the delivered events not yet acknowledged, in
// delivery order, with their digests.
func pendingInbox(dir string) ([]domain.Event, []string) {
	names, err := inboxEntries(dir)
	if err != nil {
		return nil, nil
	}
	var events []domain.Event
	var digests []string
	for _, name := range names {
		digest := name[strings.LastIndex(name, "-")+1 : len(name)-len(".json")]
		if _, err := os.Stat(filepath.Join(dir, "consumed", digest)); err == nil {
			continue
		}
		var in inputEvent
		if readJSON(filepath.Join(dir, name), &in) != nil || in.ID == "" || eventDigest(domain.ID(in.ID)) != digest {
			continue
		}
		events = append(events, in.event())
		digests = append(digests, digest)
	}
	return events, digests
}
