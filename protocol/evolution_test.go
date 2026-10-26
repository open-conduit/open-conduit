package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

func evolving(versions ...int) protocol.Manifest {
	return protocol.Manifest{Protocol: []int{1}, Name: "chat", Version: "1", Command: []string{"./chat"}, Provides: protocol.Provides{
		Families: []protocol.Family{{Type: "chat.message.posted", ReDerivable: true}},
		Schemas:  []protocol.SchemaDecl{{Name: "chat.message", Versions: versions}},
	}}
}

func TestPayloadSchemaStatus(t *testing.T) {
	m := evolving(2, 3)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for schema, want := range map[string]protocol.SchemaStatus{
		"":                  protocol.SchemaForeign,
		"core.config.v1":    protocol.SchemaForeign,
		"github.issue.v1":   protocol.SchemaForeign,
		"chatx.message.v1":  protocol.SchemaForeign,
		"chat.message.v3":   protocol.SchemaCurrent,
		"chat.message.v2":   protocol.SchemaUpcast,
		"chat.message.v1":   protocol.SchemaRetired,
		"chat.message.v4":   protocol.SchemaNewer,
		"chat.reaction.v1":  protocol.SchemaUndeclared,
		"chat.message":      protocol.SchemaUndeclared,
		"chat.message.v0":   protocol.SchemaUndeclared,
		"chat.message.vtwo": protocol.SchemaUndeclared,
	} {
		got := m.PayloadStatus(domain.Payload{Schema: schema})
		if got != want {
			t.Errorf("%q: %s, want %s", schema, got, want)
		}
		readable := want == protocol.SchemaForeign || want == protocol.SchemaCurrent || want == protocol.SchemaUpcast
		if got.Readable() != readable {
			t.Errorf("%q: readable %v", schema, got.Readable())
		}
	}
	if err := m.CheckWrite(domain.Payload{Schema: "chat.message.v3", Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckWrite(domain.Payload{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"chat.message.v2", "chat.message.v4", "github.issue.v1", "chat.other.v1"} {
		if err := m.CheckWrite(domain.Payload{Schema: s, Data: []byte(`{}`)}); !errors.Is(err, protocol.ErrSchema) {
			t.Errorf("write %q: %v", s, err)
		}
	}
}

func TestSchemaDeclarationsAreValidated(t *testing.T) {
	for name, versions := range map[string][]int{"empty": nil, "unordered": {2, 1}, "duplicate": {1, 1}, "zero": {0, 1}} {
		if err := evolving(versions...).Validate(); !errors.Is(err, protocol.ErrManifest) {
			t.Errorf("%s versions accepted", name)
		}
	}
	m := evolving(1)
	m.Provides.Schemas[0].Name = "chat.message.v2"
	if err := m.Validate(); !errors.Is(err, protocol.ErrManifest) {
		t.Error("versioned base accepted")
	}
	m.Provides.Schemas[0].Name = "github.message"
	if err := m.Validate(); !errors.Is(err, protocol.ErrManifest) {
		t.Error("foreign schema base accepted")
	}
	m = evolving(1)
	m.KVVersion = -1
	if err := m.Validate(); !errors.Is(err, protocol.ErrManifest) {
		t.Error("negative kv_version accepted")
	}
}

func TestKVLayoutTransitions(t *testing.T) {
	for _, c := range []struct {
		stored, declared int
		migrate, refuse  bool
	}{{0, 0, false, false}, {0, 1, true, false}, {1, 3, true, false}, {2, 2, false, false}, {3, 2, false, true}} {
		migrate, err := protocol.CheckKV(c.stored, c.declared)
		if migrate != c.migrate || errors.Is(err, protocol.ErrKVDowngrade) != c.refuse {
			t.Errorf("stored %d declared %d: migrate %v err %v", c.stored, c.declared, migrate, err)
		}
	}
}

func TestErrorCodesRoundTrip(t *testing.T) {
	reset := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, err := range []error{extension.ErrInvalid, extension.ErrUnsupported, extension.ErrUnauthorized, extension.ErrUndeclared, storage.ErrNotFound, storage.ErrConflict, extension.ErrNotAdoptable, extension.ErrNotAccepting, extension.ErrNothingToAttach, extension.ErrResourceSetup, extension.ErrResourceNotFound, extension.ErrResourceExhausted} {
		e := protocol.Encode(errors.Join(errors.New("context"), err))
		if got := protocol.Decode(e.Code, e.Message, e.ResetAt); !errors.Is(got, err) {
			t.Errorf("%v crossed as %v", err, got)
		}
	}
	// A capacity refusal keeps the provider's advice across the boundary.
	full := protocol.Encode(fmt.Errorf("%w: not enough disk space; free space, then retry", extension.ErrResourceExhausted))
	if got := protocol.Decode(full.Code, full.Message, nil); full.Code != protocol.CodeResourceExhausted || !errors.Is(got, extension.ErrResourceExhausted) || !strings.Contains(got.Error(), "free space, then retry") {
		t.Errorf("capacity refusal crossed as %+v", full)
	}
	if e := protocol.Encode(storage.ErrLeaseLost); e.Code != protocol.CodeConflict {
		t.Errorf("lease loss crossed as %s", e.Code)
	}
	op := protocol.Encode(&extension.OperationError{Code: "already_merged", Message: "PR #12 is merged"})
	var oe *extension.OperationError
	if got := protocol.Decode(op.Code, op.Message, nil); !errors.As(got, &oe) || oe.Code != "already_merged" || oe.Message != "PR #12 is merged" {
		t.Errorf("operation error crossed as %v", got)
	}
	hint := protocol.Encode(&protocol.Error{Code: protocol.CodeRetryAfter, ResetAt: &reset})
	var p extension.RetryHintProvider
	if got := protocol.Decode(hint.Code, hint.Message, hint.ResetAt); !errors.As(got, &p) || !p.RetryHint().ResetAt.Equal(reset) {
		t.Errorf("retry hint crossed as %v", got)
	}
	if e := protocol.Encode(errors.New("secret detail")); e.Code != protocol.CodeInternal || e.Message != "internal error" {
		t.Errorf("unauthored error leaked: %+v", e)
	}
	if got := protocol.Decode(protocol.CodeCancelled, "", nil); !errors.Is(got, context.Canceled) {
		t.Errorf("cancelled crossed as %v", got)
	}
}

func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWireTypesRoundTripTheContracts(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	key := domain.CorrelationKey{Source: "chat", ExternalID: "T1"}
	ev := domain.Event{ID: "e", Type: "chat.message.posted", Source: "chat/acct", DeliveryID: "d", OccurredAt: now, ReceivedAt: now, CorrelationKey: &key, CausationID: "c", WorkID: "w", ExecutionID: "x", Data: domain.Payload{Schema: "chat.message.v1", Data: []byte(`{"text":"hi"}`)}}
	if got := roundTrip(t, protocol.FromEvent(ev)).Domain(); !reflect.DeepEqual(got, ev) {
		t.Errorf("event:\n%+v\n%+v", got, ev)
	}
	push := extension.PushResult{Observations: []extension.Observation{{ID: "i", Version: "v", Event: ev}}, Events: []domain.Event{ev}}
	if got := roundTrip(t, protocol.FromPushResult(push)).Extension(); !reflect.DeepEqual(got, push) {
		t.Errorf("push result differs")
	}
	snap := extension.Snapshot{ScanID: "s", Observations: push.Observations, NextCursor: "n", Checkpoint: "c"}
	if got := roundTrip(t, protocol.FromSnapshot(snap)).Extension(); !reflect.DeepEqual(got, snap) {
		t.Errorf("snapshot differs")
	}
	cost, tokens := 0.5, uint64(7)
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "limit"}, Usage: extension.Usage{InputTokens: &tokens, Cost: &cost, Currency: "USD"}, Artifacts: []domain.ArtifactRef{{ID: "a", Size: 3, MediaType: "text/plain"}}, RetryHint: &extension.RetryHint{ResetAt: &now}, ConsumedInputs: []domain.ID{"e"}, Workspace: &extension.WorkspaceRecord{Kind: "git", Ref: "r", Dir: "/w", Summary: "s", Data: domain.Payload{Schema: "git.ws.v1", Data: []byte(`{}`)}}}
	if got := roundTrip(t, protocol.FromResult(result)).Extension(); !reflect.DeepEqual(got, result) {
		t.Errorf("result:\n%+v\n%+v", got, result)
	}
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"chat.message.posted"}}, Timeout: &now, AfterSequence: 4}, Dependencies: []extension.Dependency{{ID: "d", SourceScopeID: "s", SourceKey: key, Types: []string{"t.x"}, AliasType: "chat.alias", Filter: domain.Payload{Schema: "chat.filter.v1", Data: []byte(`{}`)}, Label: "l", FromNow: true}}, Output: domain.Payload{Schema: "chat.out.v1", Data: []byte(`{}`)}}
	if got := roundTrip(t, protocol.FromWaitPlan(plan)).Extension(); !reflect.DeepEqual(got, plan) {
		t.Errorf("wait plan differs")
	}
	a := extension.Authority{ScopeID: "s", WorkID: "w", ExecutionID: "x", Lease: domain.LeaseRef{ExecutionID: "x", Owner: "o", Epoch: 3}, ExpiresAt: now, Operations: []string{"emit"}, EventTypes: []string{"chat.message.posted"}, TriggerID: "t"}
	if got := roundTrip(t, protocol.FromAuthority(a)).Extension(); !reflect.DeepEqual(got, a) {
		t.Errorf("authority differs")
	}
	scope := domain.Scope{ID: "s", Name: "n", SecretRefs: map[string]string{"token": "cmd:secret"}}
	if raw, _ := json.Marshal(protocol.FromScope(scope)); containsSecret(raw) {
		t.Error("scope secret references crossed the wire")
	}
	d := extension.Delivery{ID: "d", ReceivedAt: now, ContentType: "application/json", Body: []byte{0, 1, 2, 255}}
	if got := roundTrip(t, protocol.FromDelivery(d)).Extension(); !reflect.DeepEqual(got, d) {
		t.Errorf("delivery bytes differ")
	}
}

func containsSecret(raw []byte) bool {
	return strings.Contains(string(raw), "cmd:secret") || strings.Contains(string(raw), "secret_refs")
}
