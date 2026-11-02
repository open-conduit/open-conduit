package conformance_test

import (
	"context"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// memoryEmitter is a minimal emitter for the suite's own controls, over
// memoryKV: atomic false models a host that accepts a batch's Events before
// checking the batch (outside its transaction), checked false one that
// accepts an Event of any family or parent.
type memoryEmitter struct {
	*memoryKV
	mu              sync.Mutex
	events          map[string]bool
	family, parent  string
	atomic, checked bool
}

func (m *memoryEmitter) CommitBatch(ctx context.Context, b protocol.KVCommitParams) (protocol.KVCommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out protocol.KVCommitResult
	accept := func() {
		for _, em := range b.Emissions {
			id := protocol.EmittedEventID(em.Event.Source, em.Event.DeliveryID)
			added := !m.events[id]
			m.events[id] = true
			out.Emissions = append(out.Emissions, protocol.EmissionResult{Event: protocol.AcceptedEvent{Event: protocol.Event{ID: id}}, Added: added})
		}
	}
	if !m.atomic {
		accept()
	}
	if m.checked {
		for _, em := range b.Emissions {
			if em.Event.Type != m.family || em.Parent != "" && em.Parent != m.parent {
				return protocol.KVCommitResult{}, extension.ErrUnauthorized
			}
		}
	}
	if err := m.writeKV(b); err != nil {
		return protocol.KVCommitResult{}, err
	}
	if m.atomic {
		accept()
	}
	return out, nil
}

// writeKV checks the batch's preconditions and applies its writes.
func (m *memoryEmitter) writeKV(b protocol.KVCommitParams) error {
	kv := m.memoryKV
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for _, c := range b.Checks {
		v, ok := kv.data[c.Namespace+"\x00"+c.Key]
		if c.Absent && ok || !c.Absent && (!ok || digest(v) != c.Version) {
			return storage.ErrConflict
		}
	}
	for _, p := range b.Puts {
		if err := kv.own(p.Namespace, p.Key); err != nil {
			return err
		}
	}
	for _, p := range b.Puts {
		kv.data[p.Namespace+"\x00"+p.Key] = p.Value
	}
	for _, d := range b.Deletes {
		delete(kv.data, d.Namespace+"\x00"+d.Key)
	}
	return nil
}

func emissionsFixture(atomic, checked bool) conformance.ExtensionEmissionsFixture {
	kv := kvFixture(true, true)
	m := &memoryEmitter{memoryKV: kv.KV.(*memoryKV), events: map[string]bool{"parent": true}, family: "fixture.changed.v1", parent: "parent", atomic: atomic, checked: checked}
	return conformance.ExtensionEmissionsFixture{KV: m, Name: kv.Name, Scope: "scope", Parent: "parent",
		Event: func(delivery string) protocol.Event {
			return protocol.Event{Type: "fixture.changed.v1", Source: "fixture", DeliveryID: delivery}
		},
		Accepted: func(id string) bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.events[id]
		},
		Peek: kv.Peek}
}

func TestExtensionEmissionsReference(t *testing.T) {
	conformance.RunExtensionEmissions(t, func() conformance.ExtensionEmissionsFixture { return emissionsFixture(true, true) })
}

// TestWeakenedExtensionEmissionsAreRejected is the suite's negative
// control: a host that accepts a batch's Events outside its transaction,
// or without checking their family and parent, fails it.
func TestWeakenedExtensionEmissionsAreRejected(t *testing.T) {
	for name, f := range map[string]func() conformance.ExtensionEmissionsFixture{
		"non_atomic": func() conformance.ExtensionEmissionsFixture { return emissionsFixture(false, true) },
		"unchecked":  func() conformance.ExtensionEmissionsFixture { return emissionsFixture(true, false) },
	} {
		failed := false
		for _, c := range conformance.ExtensionEmissionsCases() {
			if c.Check(f()) != nil {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("%s emitter passed the extension emissions suite", name)
		}
	}
}
