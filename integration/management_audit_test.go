package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/management"
)

// TestManagementAuditLedger checks the audit ledger records each kind of
// administrative call once, with who made it, what it acted on and how it
// ended: a credential issued and revoked, a config applied, a control (by
// its Event), a role refused and a revoked credential tried. It reads back
// through GET /v1/audit in pages that chain, and needs admin.
func TestManagementAuditLedger(t *testing.T) {
	h := newManagementHarnessWith(t, grantsConfig, nil)
	ctx := context.Background()
	owner := h.operatorClient()

	viewer, err := owner.AddOperator(ctx, "vera", "view")
	if err != nil {
		t.Fatal(err)
	}
	vera, err := client.New(h.endpoint, viewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error, status int, code string) {
		t.Helper()
		var api *client.Error
		if !errors.As(err, &api) || api.Status != status || api.Code != code {
			t.Fatalf("%s: want %d %s, got %v", what, status, code, err)
		}
	}
	// A view operator can neither apply the config nor read the ledger.
	v, err := owner.ValidateConfig(ctx, json.RawMessage(grantsConfig))
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	_, err = vera.ApplyConfig(ctx, json.RawMessage(grantsConfig), v.CandidateHash, "1", "vera-apply")
	refused("a viewer's apply", err, 403, "forbidden_role")
	_, err = vera.Audit(ctx, 0, 0)
	refused("a viewer's audit read", err, 403, "forbidden_role")

	applied, err := owner.ApplyConfig(ctx, json.RawMessage(strings.Replace(grantsConfig, `"name": "team-b"`, `"name": "Team B"`, 1)), "", "1", "owner-apply")
	if err == nil {
		t.Fatalf("an apply with the wrong hash applied: %+v", applied)
	}
	next := json.RawMessage(strings.Replace(grantsConfig, `"name": "team-b"`, `"name": "Team B"`, 1))
	if v, err = owner.ValidateConfig(ctx, next); err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if applied, err = owner.ApplyConfig(ctx, next, v.CandidateHash, "1", "owner-apply-2"); err != nil {
		t.Fatal(err)
	}

	created, err := owner.CreateWork(ctx, client.WorkRequest{ScopeID: "team-a", Kind: "manual.task", Binding: "run"}, "make-a")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := owner.CancelWork(ctx, created.Work.ID, created.Work.Revision, "not needed", "cancel-a")
	if err != nil {
		t.Fatal(err)
	}
	// A replay is the same call: the ledger has it once.
	if _, err = owner.CancelWork(ctx, created.Work.ID, created.Work.Revision, "not needed", "cancel-a"); err != nil {
		t.Fatal(err)
	}

	if _, err = owner.Revoke(ctx, viewer.TokenID, "left the team", "revoke-vera"); err != nil {
		t.Fatal(err)
	}
	_, err = vera.Config(ctx)
	refused("a revoked credential", err, 401, "unauthenticated")

	page, err := owner.Audit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = domain.VerifyAuditChain(0, "", page.Records); err != nil {
		t.Fatalf("the ledger does not chain over the API: %v", err)
	}
	find := func(what string, match func(domain.AuditRecord) bool) domain.AuditRecord {
		t.Helper()
		var found []domain.AuditRecord
		for _, r := range page.Records {
			if match(r) {
				found = append(found, r)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%s: %d records, want 1, in %+v", what, len(found), page.Records)
		}
		return found[0]
	}
	issued := find("credential issued", func(r domain.AuditRecord) bool { return r.Action == "POST /v1/auth/operators" })
	if issued.Actor != "operator:local" || issued.Outcome != domain.AuditOK || issued.Target != "credential/"+viewer.TokenID || issued.Detail["operator"] != "operator:vera" || issued.Detail["role"] != "view" {
		t.Errorf("credential issued: %+v", issued)
	}
	find("viewer's apply refused", func(r domain.AuditRecord) bool {
		return r.Action == "POST /v1/config/apply" && r.Actor == "operator:vera" && r.Outcome == "forbidden_role"
	})
	find("viewer's audit read refused", func(r domain.AuditRecord) bool {
		return r.Action == "GET /v1/audit" && r.Actor == "operator:vera" && r.Outcome == "forbidden_role"
	})
	find("apply with the wrong hash", func(r domain.AuditRecord) bool {
		return r.Action == "POST /v1/config/apply" && r.Actor == "operator:local" && r.Outcome == "candidate_hash_mismatch"
	})
	apply := find("config applied", func(r domain.AuditRecord) bool {
		return r.Action == "POST /v1/config/apply" && r.Outcome == domain.AuditOK
	})
	if apply.Actor != "operator:local" || apply.Detail["generation"] != applied.Generation || apply.Detail["hash"] != applied.Hash {
		t.Errorf("config applied: %+v, want generation %s", apply, applied.Generation)
	}
	cancel := find("work cancelled", func(r domain.AuditRecord) bool { return r.Action == "core.manual.cancel" })
	if cancel.Actor != "operator:local" || string(cancel.EventID) != cancelled.EventID || cancel.Target != "work/"+created.Work.ID {
		t.Errorf("work cancelled: %+v, want event %s", cancel, cancelled.EventID)
	}
	revoked := find("credential revoked", func(r domain.AuditRecord) bool { return r.Action == "POST /v1/auth/revoke" })
	if revoked.Target != "credential/"+viewer.TokenID || revoked.Detail["reason"] != "left the team" || revoked.Detail["operator"] != "operator:vera" {
		t.Errorf("credential revoked: %+v", revoked)
	}
	tried := find("revoked credential tried", func(r domain.AuditRecord) bool { return r.Outcome == "unauthenticated" })
	if tried.Actor != "" || tried.Action != "GET /v1/config" || tried.Detail["credential"] != viewer.TokenID || tried.Detail["operator"] != "operator:vera" || tried.Detail["reason"] != "credential revoked" {
		t.Errorf("revoked credential tried: %+v", tried)
	}
	// Reads that succeed and calls that change nothing are not recorded.
	if slices.ContainsFunc(page.Records, func(r domain.AuditRecord) bool {
		return r.Action == "POST /v1/config/validate" || r.Action == "GET /v1/config" && r.Outcome == domain.AuditOK
	}) {
		t.Errorf("a read or validate was recorded: %+v", page.Records)
	}

	// Pages continue the chain from where the last one ended.
	first, err := owner.Audit(ctx, 0, 2)
	if err != nil || len(first.Records) != 2 || first.NextAfter != 2 {
		t.Fatalf("first page: %+v %v", first, err)
	}
	after, prev, err := domain.VerifyAuditChain(0, "", first.Records)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := owner.Audit(ctx, first.NextAfter, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = domain.VerifyAuditChain(after, prev, rest.Records); err != nil || len(rest.Records) < len(page.Records)-2 {
		t.Fatalf("second page: %d records, %v", len(rest.Records), err)
	}
}

// TestManagementAuditUnauthenticatedBudget checks that someone with no
// credential can't grow the audit ledger without bound. Each client's
// refused credentials are recorded as 401s up to the auth-failure limit
// per window: a network client's throttled requests (429) are never
// recorded, nor is anything a request that authenticated no operator did
// beyond its 401, even on an audited route. The owner's socket, which has
// no throttle, has the same budget.
func TestManagementAuditUnauthenticatedBudget(t *testing.T) {
	h := newManagementHarness(t)
	dir := t.TempDir()
	networkCerts(t, dir)
	at := func(name string) string { return filepath.Join(dir, name) }
	endpoint := serveNetwork(t, h, management.NetworkConfig{CertFile: at("server.pem"), KeyFile: at("server.key")})
	ctx := context.Background()
	owner := h.operatorClient()
	wrong, err := client.NewWithOptions(endpoint, "not-a-credential", client.Options{CAFile: at("ca.pem")})
	if err != nil {
		t.Fatal(err)
	}
	local, err := client.New(h.endpoint, "not-a-credential-either")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := func() []domain.AuditRecord {
		t.Helper()
		page, err := owner.Audit(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.AuditRecord
		for _, r := range page.Records {
			if r.Actor == "" {
				if r.Outcome != "unauthenticated" {
					t.Fatalf("a request that authenticated no one was recorded as %s: %+v", r.Outcome, r)
				}
				out = append(out, r)
			}
		}
		return out
	}
	cancel := func(c *client.Client, i int) error {
		_, err := c.CancelWork(ctx, "work/none", "", "flood", fmt.Sprint("flood-", i))
		return err
	}
	for i := 0; i < 3*management.AuthFailureLimit; i++ {
		if err = cancel(wrong, i); apiCode(err) != "unauthenticated" && apiCode(err) != "too_many_auth_failures" {
			t.Fatalf("network flood %d = %v", i, err)
		}
	}
	if got := unauthenticated(); len(got) != management.AuthFailureLimit || got[0].Action != "POST /v1/work/{id}/cancel" {
		t.Fatalf("after a throttled flood the ledger has %d refusals, want %d: %+v", len(got), management.AuthFailureLimit, got)
	}
	for i := 0; i < 3*management.AuthFailureLimit; i++ {
		if err = cancel(local, i); apiCode(err) != "unauthenticated" {
			t.Fatalf("socket flood %d = %v", i, err)
		}
	}
	if got := unauthenticated(); len(got) != 2*management.AuthFailureLimit {
		t.Fatalf("after a flood on the socket the ledger has %d refusals, want %d", len(got), 2*management.AuthFailureLimit)
	}
	// The next window brings a fresh budget.
	h.clock.Advance(management.AuthFailureWindow)
	if err = cancel(local, 0); apiCode(err) != "unauthenticated" {
		t.Fatal(err)
	}
	if got := unauthenticated(); len(got) != 2*management.AuthFailureLimit+1 {
		t.Fatalf("after the window the ledger has %d refusals, want %d", len(got), 2*management.AuthFailureLimit+1)
	}
}
