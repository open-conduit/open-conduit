package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Wait progress: a Work waiting on `pr:N:checks` shows how many of the
// waited head's checks have finished, from each github.checks.pending
// observation of that head. The wait plan records which head each Work's
// latest wait watches; a pending observation of it sets the progress line
// on the Work's active Wait through the host (ToolsOptions.WaitProgress),
// which refuses a Wait GitHub did not declare or that is no longer active.

// progressNamespace keys, by Work, the heads its latest wait watches.
const progressNamespace = "github.tools.progress"

type watchedChecks struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Head   string `json:"head"`
}

// watchChecks records the pr_checks dependencies of a Work's new wait plan,
// replacing what its earlier plan watched; a plan without one clears it,
// so progress never lands on a later wait for something else.
func (t *Tools) watchChecks(ctx context.Context, work domain.ID, plan extension.WaitPlan) error {
	if t.o.WaitProgress == nil {
		return nil
	}
	var watched []watchedChecks
	for _, d := range plan.Dependencies {
		var f dependencyFilter
		if d.Filter.Schema != SchemaFilter || json.Unmarshal(d.Filter.Data, &f) != nil || f.Kind != "pr_checks" {
			continue
		}
		watched = append(watched, watchedChecks{Repo: f.Repo, Number: f.Number, Head: f.Head})
	}
	return t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		if len(watched) == 0 {
			err := tx.Delete(progressNamespace, string(work))
			if errors.Is(err, storage.ErrNotFound) {
				err = nil
			}
			return err
		}
		data, err := json.Marshal(watched)
		if err != nil {
			return err
		}
		return tx.Put(progressNamespace, string(work), data)
	})
}

// ObserveChecks sets wait progress from observations about to be accepted:
// each github.checks.pending observation of a head a waiting Work watches.
// Progress is for operator views only, so a failure is never the
// observation's: it is skipped, and the next observation tries again.
func (t *Tools) ObserveChecks(ctx context.Context, obs []extension.Observation) {
	if t.o.WaitProgress == nil {
		return
	}
	var pending []ChecksPayload
	for _, o := range obs {
		var p ChecksPayload
		if o.Event.Type != TypeChecksPending || o.Event.Data.Schema != SchemaChecks || json.Unmarshal(o.Event.Data.Data, &p) != nil {
			continue
		}
		pending = append(pending, p)
	}
	if len(pending) == 0 {
		return
	}
	var rows []storage.Entry
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		rows, err = tx.List(progressNamespace, "")
		return err
	}); err != nil {
		return
	}
	for _, row := range rows {
		var watched []watchedChecks
		if json.Unmarshal(row.Value, &watched) != nil {
			continue
		}
		for _, p := range pending {
			if !watches(watched, p) {
				continue
			}
			err := t.o.WaitProgress(ctx, domain.ID(row.Key), ChecksProgress(p))
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, extension.ErrUnauthorized) {
				t.forgetFinished(ctx, domain.ID(row.Key))
			}
		}
	}
}

func watches(watched []watchedChecks, p ChecksPayload) bool {
	for _, w := range watched {
		if strings.EqualFold(w.Repo, p.Repo) && w.Number == p.Number && w.Head == p.SHA {
			return true
		}
	}
	return false
}

// forgetFinished drops a Work's record once the Work has ended; while it
// lives, a wait not yet registered or a later turn's may still need it.
func (t *Tools) forgetFinished(ctx context.Context, work domain.ID) {
	_ = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		w, err := tx.Work(work)
		if err == nil && !w.State.Terminal() {
			return nil
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		err = tx.Delete(progressNamespace, string(work))
		if errors.Is(err, storage.ErrNotFound) {
			err = nil
		}
		return err
	})
}

// ChecksProgress says how far a head's checks have got: "3 of 7 checks
// finished", with how many failed so far. Required checks that have not
// reported count as unfinished.
func ChecksProgress(p ChecksPayload) string {
	total, finished, failed := len(p.Runs)+len(p.Missing), 0, 0
	for _, r := range p.Runs {
		switch r.State {
		case "pending":
		case "failure":
			finished++
			failed++
		default:
			finished++
		}
	}
	noun := "checks"
	if total == 1 {
		noun = "check"
	}
	s := fmt.Sprintf("%d of %d %s finished", finished, total, noun)
	if failed > 0 {
		s += fmt.Sprintf(", %d failed", failed)
	}
	return s
}
