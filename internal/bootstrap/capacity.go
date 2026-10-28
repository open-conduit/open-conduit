package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// jobPool is the capacity pool of job executors (#13): settings.concurrency
// bounds agent turns (and any executor not listed here) and
// settings.job_concurrency bounds these, so neither can starve the other.
const jobPool = "jobs"

// jobExecutors are the executor registrations that count against
// settings.job_concurrency instead of settings.concurrency.
func jobExecutors() map[string]string {
	return map[string]string{"shell": jobPool, sc.LegacyExecutorName: jobPool, gh.HoldExecutor: jobPool}
}

// capacitySource is the Annotation source of a Work's capacity wait; WorkLabel
// never names Work by it.
const capacitySource = "runtime"

// capacityCode is the diagnostic code of a start held back by a pool, named
// after the setting that bounds it.
func capacityCode(err *runtime.CapacityError) string {
	switch err.Pool {
	case runtime.GlobalPool:
		return "capacity_concurrency"
	case jobPool:
		return "capacity_job_concurrency"
	case "scope":
		return "capacity_scope"
	}
	return "capacity"
}

// capacityLabel names a pool by its setting and what it bounds.
func capacityLabel(pool string) string {
	switch pool {
	case runtime.GlobalPool:
		return "agent turns (settings.concurrency)"
	case jobPool:
		return "jobs (settings.job_concurrency)"
	}
	return pool
}

func poolOfCode(code string) (string, bool) {
	switch code {
	case "capacity_concurrency":
		return runtime.GlobalPool, true
	case "capacity_job_concurrency":
		return jobPool, true
	}
	return "", strings.HasPrefix(code, "capacity")
}

func usageText(u runtime.PoolUsage) string {
	if u.Limit == 0 {
		return fmt.Sprintf("%s %d running, unbounded", capacityLabel(u.Pool), u.Running)
	}
	return fmt.Sprintf("%s %d/%d running", capacityLabel(u.Pool), u.Running, u.Limit)
}

// waitingOnCapacity lists ready Work whose last start was held back by a
// capacity limit, with the diagnostic code.
func (d *daemon) waitingOnCapacity(ctx context.Context) map[domain.ID]string {
	d.mu.Lock()
	waiting := map[domain.ID]string{}
	for key, code := range d.reported {
		kind, id, _ := strings.Cut(key, "\x00")
		if _, ok := poolOfCode(code); kind == "start" && ok {
			waiting[domain.ID(id)] = code
		}
	}
	d.mu.Unlock()
	if len(waiting) == 0 {
		return waiting
	}
	_ = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		for id := range waiting {
			if w, err := tx.Work(id); err != nil || w.State != domain.WorkReady {
				delete(waiting, id)
			}
		}
		return nil
	})
	return waiting
}

// capacityCheck reports each pool's running attempts against its limit and
// the Work waiting for one, with the limit holding it back.
func (d *daemon) capacityCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "capacity", Status: runtime.CheckOK, Code: "ok"}
	usage, err := d.current.Load().scheduler.Capacity(ctx)
	if err != nil {
		check.Status, check.Code, check.Detail = runtime.CheckWarn, "storage_unavailable", "running attempts could not be counted"
		return check
	}
	parts := make([]string, 0, len(usage))
	byPool := map[string]runtime.PoolUsage{}
	for _, u := range usage {
		parts = append(parts, usageText(u))
		byPool[u.Pool] = u
	}
	check.Detail = strings.Join(parts, "; ")
	waiting := d.waitingOnCapacity(ctx)
	ids := make([]domain.ID, 0, len(waiting))
	for id := range waiting {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		code := waiting[id]
		if check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckInfo, "waiting"
		}
		item := string(id) + ": waiting for a scope limit"
		if pool, _ := poolOfCode(code); pool != "" {
			item = string(id) + ": waiting for " + usageText(byPool[pool])
		}
		check.Items = append(check.Items, item)
	}
	if n := len(ids); n > 0 {
		check.Detail += fmt.Sprintf("; %d Work waiting for capacity", n)
	}
	return check
}

// capacityPools reports each pool for GET /v1/capacity from the scheduler's
// own accounting: the agent pool as "agents", the job pool as "jobs".
func (d *daemon) capacityPools(ctx context.Context) ([]management.Pool, error) {
	usage, err := d.current.Load().scheduler.Capacity(ctx)
	if err != nil {
		return nil, err
	}
	waiting := map[string]int{}
	for _, code := range d.waitingOnCapacity(ctx) {
		if pool, _ := poolOfCode(code); pool != "" {
			waiting[pool]++
		}
	}
	out := make([]management.Pool, 0, len(usage))
	for _, u := range usage {
		p := management.Pool{Name: u.Pool, Limit: u.Limit, InUse: u.Running, Waiting: waiting[u.Pool]}
		switch u.Pool {
		case runtime.GlobalPool:
			p.Name, p.Setting = "agents", "settings.concurrency"
		case jobPool:
			p.Setting = "settings.job_concurrency"
		}
		out = append(out, p)
	}
	return out, nil
}

// capacityAnnotation says which limit holds ready Work back, if one does.
func (d *daemon) capacityAnnotation(ctx context.Context, w domain.Work) []management.Annotation {
	if w.State != domain.WorkReady {
		return nil
	}
	pool, ok := poolOfCode(d.reportedCode("start", string(w.ID)))
	if !ok {
		return nil
	}
	value := "a scope concurrency limit is reached"
	if pool != "" {
		value = "limit reached: " + capacityLabel(pool)
		if usage, err := d.current.Load().scheduler.Capacity(ctx); err == nil {
			for _, u := range usage {
				if u.Pool == pool {
					value = "limit reached: " + usageText(u)
				}
			}
		}
	}
	return []management.Annotation{{Source: capacitySource, Name: "waiting", Value: value}}
}
