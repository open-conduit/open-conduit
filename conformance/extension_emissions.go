package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// ExtensionEmitter is the KV an extension reaches through the host, whose
// batches also return Events for the host to accept in the same
// transaction (kv.commit Emissions, #428): the protocol's form of an
// in-process extension's AcceptInTx and AcceptDerivedInTx.
type ExtensionEmitter interface {
	ExtensionKV
	CommitBatch(ctx context.Context, batch protocol.KVCommitParams) (protocol.KVCommitResult, error)
}

// ExtensionEmissionsFixture is a fresh emitter for an extension called
// Name, configured in Scope. Event builds a valid Event of one of its
// families with the given delivery ID; Parent is an accepted Event in
// Scope. Accepted and Peek are trusted reads that bypass the extension's
// scoping: whether an Event ID is accepted, and a KV value.
type ExtensionEmissionsFixture struct {
	KV       ExtensionEmitter
	Name     string
	Scope    string
	Parent   string
	Event    func(delivery string) protocol.Event
	Accepted func(id string) bool
	Peek     func(namespace, key string) ([]byte, error)
}

type ExtensionEmissionsCase struct {
	Name  string
	Check func(ExtensionEmissionsFixture) error
}

// nothingApplied checks a refused batch left neither its Event nor its
// write behind.
func nothingApplied(f ExtensionEmissionsFixture, delivery, key string) error {
	if id := protocol.EmittedEventID(f.Event(delivery).Source, delivery); f.Accepted(id) {
		return fmt.Errorf("a refused batch accepted its Event %s", id)
	}
	if _, err := f.Peek(f.Name, key); !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("a refused batch wrote %q: %v", key, err)
	}
	return nil
}

func ExtensionEmissionsCases() []ExtensionEmissionsCase {
	ctx := context.Background()
	return []ExtensionEmissionsCase{
		{"emissions_commit_with_writes", func(f ExtensionEmissionsFixture) error {
			root, derived := f.Event("root-1"), f.Event("derived-1")
			r, err := f.KV.CommitBatch(ctx, protocol.KVCommitParams{
				Puts:      []protocol.KVPut{put(f.Name, "emitted", "1")},
				Emissions: []protocol.Emission{{ScopeID: f.Scope, Event: root}, {ScopeID: f.Scope, Parent: f.Parent, Event: derived}},
			})
			if err != nil {
				return err
			}
			if len(r.Emissions) != 2 || !r.Emissions[0].Added || !r.Emissions[1].Added {
				return fmt.Errorf("emission results %+v, want two added", r.Emissions)
			}
			for i, e := range []protocol.Event{root, derived} {
				id := protocol.EmittedEventID(e.Source, e.DeliveryID)
				if r.Emissions[i].Event.Event.ID != id || !f.Accepted(id) {
					return fmt.Errorf("emission %d: answered %q, want %q accepted", i, r.Emissions[i].Event.Event.ID, id)
				}
			}
			if v, err := f.Peek(f.Name, "emitted"); err != nil || string(v) != "1" {
				return fmt.Errorf("the batch's write: %q %v", v, err)
			}
			return nil
		}},
		{"emission_without_writes_commits", func(f ExtensionEmissionsFixture) error {
			e := f.Event("alone-1")
			if _, err := f.KV.CommitBatch(ctx, protocol.KVCommitParams{Emissions: []protocol.Emission{{ScopeID: f.Scope, Event: e}}}); err != nil {
				return err
			}
			if !f.Accepted(protocol.EmittedEventID(e.Source, e.DeliveryID)) {
				return fmt.Errorf("an emission alone was not accepted")
			}
			return nil
		}},
		{"failed_check_applies_nothing", func(f ExtensionEmissionsFixture) error {
			if err := f.KV.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{put(f.Name, "guard", "1")}}); err != nil {
				return err
			}
			_, err := f.KV.CommitBatch(ctx, protocol.KVCommitParams{
				Checks:    []protocol.KVCheck{{Namespace: f.Name, Key: "guard", Absent: true}},
				Puts:      []protocol.KVPut{put(f.Name, "after-conflict", "1")},
				Emissions: []protocol.Emission{{ScopeID: f.Scope, Event: f.Event("conflict-1")}},
			})
			if !errors.Is(err, storage.ErrConflict) {
				return fmt.Errorf("a stale check: %v, want ErrConflict", err)
			}
			return nothingApplied(f, "conflict-1", "after-conflict")
		}},
		{"refused_emission_applies_nothing", func(f ExtensionEmissionsFixture) error {
			foreign := f.Event("foreign-1")
			foreign.Type = "undeclared.family.v1"
			_, err := f.KV.CommitBatch(ctx, protocol.KVCommitParams{
				Puts:      []protocol.KVPut{put(f.Name, "after-refusal", "1")},
				Emissions: []protocol.Emission{{ScopeID: f.Scope, Event: f.Event("beside-foreign-1")}, {ScopeID: f.Scope, Event: foreign}},
			})
			if err == nil {
				return fmt.Errorf("an undeclared family was accepted")
			}
			return nothingApplied(f, "beside-foreign-1", "after-refusal")
		}},
		{"foreign_parent_refused", func(f ExtensionEmissionsFixture) error {
			_, err := f.KV.CommitBatch(ctx, protocol.KVCommitParams{
				Puts:      []protocol.KVPut{put(f.Name, "after-parent", "1")},
				Emissions: []protocol.Emission{{ScopeID: f.Scope, Parent: "no-such-event", Event: f.Event("orphan-1")}},
			})
			if err == nil {
				return fmt.Errorf("an emission derived from no accepted Event was accepted")
			}
			return nothingApplied(f, "orphan-1", "after-parent")
		}},
		{"emission_is_idempotent", func(f ExtensionEmissionsFixture) error {
			b := protocol.KVCommitParams{Emissions: []protocol.Emission{{ScopeID: f.Scope, Event: f.Event("again-1")}}}
			first, err := f.KV.CommitBatch(ctx, b)
			if err != nil {
				return err
			}
			second, err := f.KV.CommitBatch(ctx, b)
			if err != nil {
				return err
			}
			if len(second.Emissions) != 1 || second.Emissions[0].Added || second.Emissions[0].Event.Event.ID != first.Emissions[0].Event.Event.ID {
				return fmt.Errorf("emitted again: %+v after %+v, want the same Event not added", second.Emissions, first.Emissions)
			}
			return nil
		}},
	}
}

// RunExtensionEmissions runs the emission cases, one fresh fixture each.
func RunExtensionEmissions(t *testing.T, factory func() ExtensionEmissionsFixture) {
	t.Helper()
	for _, c := range ExtensionEmissionsCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
