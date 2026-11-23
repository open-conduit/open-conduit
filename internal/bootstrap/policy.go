package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// offlinePolicy is the daemon's policy for a command that runs without it
// (config validate, extension install, a restore's checks). Those have no
// --policy of their own and read OPEN_CONDUIT_POLICY. An unreadable policy
// is an error, never no policy.
func offlinePolicy() (*config.Policy, error) {
	return config.LoadPolicy(os.Getenv("OPEN_CONDUIT_POLICY"))
}

// restorePolicy is the policy a restore is checked against, however it was
// supplied. It is loaded before the restore changes anything.
func restorePolicy(o cli.RestoreOptions) (*config.Policy, error) {
	p, err := config.LoadPolicy(o.PolicyPath)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", o.PolicyPath, err)
	}
	return p, nil
}

// policyCheck is doctor's view of the daemon's policy: which file is in
// force, its digest and what it is holding down.
func (d *daemon) policyCheck(context.Context) runtime.Check {
	p := d.policy
	check := runtime.Check{Name: "policy", Status: runtime.CheckOK, Code: "enforced", Detail: p.Path + " sha256:" + p.Digest}
	warn := func(code, item string) {
		if check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckWarn, code
		}
		check.Items = append(check.Items, item)
	}
	if d.configs != nil {
		if s, _ := d.configs.Current(); s != nil {
			for _, c := range s.PolicyClamps {
				warn("clamped", c)
			}
			for _, b := range p.Budgets {
				if _, ok := s.Scope(domain.ID(b.Scope)); b.Scope != "" && !ok {
					warn("budget_scope_missing", "the policy's "+b.Unit+" budget names scope "+b.Scope+", which the config doesn't define, so it limits nothing yet")
				}
			}
		}
	}
	if p.WritableByDaemon {
		warn("writable", "the daemon's own user can write this file, so it binds config authors but not code running as that user; make it read-only to that user")
	}
	return check
}

const (
	policyNamespace = "core.host.policy"
	policyKey       = "enforced"
	// noPolicy is how --accept-policy names running without one.
	noPolicy = "none"
)

// ErrPolicyChanged refuses to serve shared state under a policy other than
// the one it last ran under, until an operator accepts the change.
var ErrPolicyChanged = errors.New("the daemon's policy differs from the one this shared state last ran under")

// enforcedPolicy is the policy shared state last ran under. Digest is empty
// for none.
type enforcedPolicy struct {
	Digest string `json:"digest,omitempty"`
	Path   string `json:"path,omitempty"`
}

func (p enforcedPolicy) String() string {
	if p.Digest == "" {
		return "no policy"
	}
	return "policy sha256:" + p.Digest + " (" + p.Path + ")"
}

// heldPolicy is this daemon's policy as the record states one.
func heldPolicy(policy *config.Policy) enforcedPolicy {
	if policy == nil {
		return enforcedPolicy{}
	}
	return enforcedPolicy{Digest: policy.Digest, Path: policy.Path}
}

// policyChange is what admitting a policy to the state in tx would do.
type policyChange struct {
	before enforcedPolicy
	// write records the policy; audit is when that changes what the state
	// is held to, rather than noting that new state starts with none.
	write, audit bool
	// unknown is state that has run with no record, so before says nothing
	// about what it ran under.
	unknown bool
}

// previous names what the state was held to before this change.
func (c policyChange) previous() string {
	if c.unknown {
		return "unknown"
	}
	return policyName(c.before.Digest)
}

// decidePolicy holds shared state to the policy it last ran under. The file is
// on each host, so a successor started without it would otherwise run with no
// ceilings. Which of two policies is stricter can't be told from outside, so
// any difference needs accept to name this policy; only adding one to state
// recorded as having none needs nothing, because that can only narrow what is
// allowed.
//
// State that has run but has no record (a bundle from before the first one, or
// state that has just become shared) may have run under any policy, so any
// policy here could be looser than the last. Running it takes accept naming
// this policy, or none, once; the record then holds it. That one acceptance is
// the cost of state becoming shared, and it is paid by an operator who is
// there, rather than guessed at by a daemon during a failover.
func decidePolicy(tx storage.Tx, have enforcedPolicy, accept string) (policyChange, error) {
	var c policyChange
	raw, err := tx.Get(policyNamespace, policyKey)
	found := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return c, err
	}
	if found {
		if err = json.Unmarshal(raw, &c.before); err != nil {
			return c, fmt.Errorf("the recorded policy is unreadable: %w", err)
		}
		if c.before.Digest == have.Digest {
			return c, nil
		}
		if c.before.Digest != "" && !acceptsPolicy(accept, have.Digest) {
			return c, fmt.Errorf("%w: it last ran under %s and this daemon has %s; start it with that policy, or accept this one with --accept-policy %s", ErrPolicyChanged, c.before, have, policyName(have.Digest))
		}
		c.write, c.audit = true, true
		return c, nil
	}
	ran, err := management.HasStoredGeneration(tx)
	if err != nil {
		return c, err
	}
	if !ran {
		c.write, c.audit = true, have.Digest != ""
		return c, nil
	}
	if !acceptsPolicy(accept, have.Digest) {
		return c, fmt.Errorf("%w: it has run before with no record of its policy and this daemon has %s; start it with the policy it ran under and accept that with --accept-policy %s", ErrPolicyChanged, have, policyName(have.Digest))
	}
	c.unknown, c.write, c.audit = true, true, true
	return c, nil
}

// admitPolicy records the policy for the holder of shared state, or refuses
// it. Every change of what the state is held to is in the audit ledger.
func admitPolicy(ctx context.Context, store storage.Store, now func() time.Time, policy *config.Policy, accept, owner string, log *slog.Logger) error {
	have := heldPolicy(policy)
	var change policyChange
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		var err error
		if change, err = decidePolicy(tx, have, accept); err != nil || !change.write {
			return err
		}
		b, err := json.Marshal(have)
		if err != nil {
			return err
		}
		if err = tx.Put(policyNamespace, policyKey, b); err != nil {
			return err
		}
		if !change.audit {
			return nil
		}
		_, err = tx.AppendAudit(domain.AuditRecord{Actor: "system:serve", Action: "core.policy.changed", Outcome: domain.AuditOK, Detail: map[string]string{
			"previous": change.previous(), "current": policyName(have.Digest), "instance": owner,
		}})
		return err
	})
	if err != nil {
		return err
	}
	if change.audit {
		log.Warn("the policy this shared state runs under changed", "previous", change.previous(), "current", policyName(have.Digest), "owner", owner)
	}
	return nil
}

// prepareToOpen runs before each Open of the state. A daemon that wouldn't be
// admitted must not get as far as Open, which can raise the stored format or
// add to the schema and so lock out a correctly configured older standby. It
// also means a standby with the wrong policy is refused when it starts, not
// when it is needed.
func (d *daemon) prepareToOpen(ctx context.Context) error {
	if err := refuseUnadmittedPolicy(ctx, d.o.StateDir, d.policy, d.o.AcceptPolicy); err != nil {
		return err
	}
	if err := d.refuseUnservableConfig(ctx); err != nil {
		return err
	}
	return d.preserveBeforeUpgrade(ctx)
}

// refuseUnadmittedPolicy decides admission from a read of the state in place,
// which changes nothing and is cheap enough for a standby to repeat on each
// retry. admitPolicy decides again under the lease. A read that fails is
// retried as a transient failure, except state this binary can't open at all,
// which Open refuses in its own words.
func refuseUnadmittedPolicy(ctx context.Context, stateDir string, policy *config.Policy, accept string) error {
	have := heldPolicy(policy)
	var refused error
	err := viewSharedState(ctx, stateDir, func(tx storage.Tx) error {
		_, decided := decidePolicy(tx, have, accept)
		if errors.Is(decided, ErrPolicyChanged) {
			refused = decided
			return nil
		}
		return decided
	})
	if refused != nil {
		return refused
	}
	return err
}

// viewSharedState reads shared state in place, changing nothing. It does
// nothing for state that isn't shared or doesn't exist yet, or that this binary
// can't open at all, which Open refuses in its own words. A read that fails is
// transient, so a standby asks again.
func viewSharedState(ctx context.Context, stateDir string, read func(storage.Tx) error) error {
	c, err := coordinationOf(stateDir)
	if err != nil {
		return err
	}
	if !c.shared() {
		return nil
	}
	present, err := c.backend.Present(ctx, c.location)
	if err != nil || !present {
		return err
	}
	shared, ok := c.backend.(storage.SharedBackend)
	if !ok {
		return nil
	}
	err = shared.ViewInPlace(ctx, c.location, read)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist), errors.Is(err, storage.ErrSchemaNewer), errors.Is(err, storage.ErrRecordsNewer):
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return fmt.Errorf("reading the shared state: %v: %w", err, storage.ErrTransient)
	}
}

// policyName is a digest as --accept-policy takes it.
func policyName(digest string) string {
	if digest == "" {
		return noPolicy
	}
	return "sha256:" + digest
}

// acceptsPolicy reports whether accept names exactly this policy, so a flag
// left in a service file accepts no later change.
func acceptsPolicy(accept, digest string) bool {
	if digest == "" {
		return accept == noPolicy
	}
	return strings.TrimPrefix(accept, "sha256:") == digest
}
