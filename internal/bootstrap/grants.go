package bootstrap

import (
	"context"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// grantsCheck is doctor's check of the operation grants
// (config.GrantWarnings): a binding whose playbook relies on operations it
// or its scope doesn't grant, and an operations list that names an earlier
// version of a toolset's operations instead of the toolset.
func grantsCheck(s *config.Snapshot, reg config.Registry) runtime.Check {
	warnings := config.GrantWarnings(s, reg)
	if len(warnings) == 0 {
		return runtime.Check{Name: "grants", Status: runtime.CheckOK, Code: "granted", Detail: "every binding is granted the operations its playbook uses"}
	}
	return runtime.Check{Name: "grants", Status: runtime.CheckWarn, Code: "drift", Detail: "operation grants that leave an agent short; each says how to fix it", Items: warnings}
}

// grantCheck is grantsCheck for the active generation.
func (d *daemon) grantCheck(context.Context) runtime.Check {
	s, _ := d.configs.Current()
	return grantsCheck(s, d.configs.Registry())
}
