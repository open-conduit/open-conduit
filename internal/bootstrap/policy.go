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

// admitPolicy holds a holder of shared state to the policy that state last
// ran under. The file is on each host, so a successor started without it would
// otherwise run with no ceilings. Which of two policies is stricter can't be
// told from outside, so any difference needs accept to name this policy; only
// adding one where there was none needs nothing, because that can only narrow
// what is allowed. Every change is recorded in the audit ledger.
func admitPolicy(ctx context.Context, store storage.Store, now func() time.Time, policy *config.Policy, accept, owner string, log *slog.Logger) error {
	have := enforcedPolicy{}
	if policy != nil {
		have = enforcedPolicy{Digest: policy.Digest, Path: policy.Path}
	}
	var before enforcedPolicy
	changed := false
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		before, changed = enforcedPolicy{}, false
		raw, err := tx.Get(policyNamespace, policyKey)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(raw, &before); err != nil {
				return fmt.Errorf("the recorded policy is unreadable: %w", err)
			}
		}
		if before.Digest == have.Digest {
			return nil
		}
		if before.Digest != "" && !acceptsPolicy(accept, have.Digest) {
			return fmt.Errorf("%w: it last ran under %s and this daemon has %s; start it with that policy, or accept this one with --accept-policy %s", ErrPolicyChanged, before, have, policyName(have.Digest))
		}
		b, err := json.Marshal(have)
		if err != nil {
			return err
		}
		if err = tx.Put(policyNamespace, policyKey, b); err != nil {
			return err
		}
		changed = true
		_, err = tx.AppendAudit(domain.AuditRecord{Actor: "system:serve", Action: "core.policy.changed", Outcome: domain.AuditOK, Detail: map[string]string{
			"previous": policyName(before.Digest), "current": policyName(have.Digest), "instance": owner,
		}})
		return err
	})
	if err != nil {
		return err
	}
	if changed {
		log.Warn("the policy this shared state runs under changed", "previous", policyName(before.Digest), "current", policyName(have.Digest), "owner", owner)
	}
	return nil
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
