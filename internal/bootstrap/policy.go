package bootstrap

import (
	"context"
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
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
