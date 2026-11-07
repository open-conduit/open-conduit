package integration_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// workReads counts the Work records the management API reads through the
// Tx reads whose cost can follow history: ListWorks, LiveWorks and
// QueryWorks. With scan set it is the negative control: it answers Work
// queries by reading every Work, as the API did before storage indexed
// them (#548).
type workReads struct {
	storage.Store
	read *int
	scan *bool
}
type workReadsTx struct {
	storage.Tx
	read *int
	scan bool
}

func (s workReads) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return s.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(workReadsTx{tx, s.read, *s.scan}) })
}
func (t workReadsTx) ListWorks() ([]domain.Work, error) {
	out, err := t.Tx.ListWorks()
	*t.read += len(out)
	return out, err
}
func (t workReadsTx) LiveWorks() ([]domain.Work, error) {
	out, err := t.Tx.LiveWorks()
	*t.read += len(out)
	return out, err
}
func (t workReadsTx) selected(q storage.WorkQuery) ([]domain.Work, storage.WorkFacets, error) {
	works, err := t.ListWorks()
	page, f := storage.SelectWorks(works, q)
	return page, f, err
}
func (t workReadsTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	if t.scan {
		page, _, err := t.selected(q)
		return page, err
	}
	out, err := t.Tx.QueryWorks(q)
	*t.read += len(out)
	return out, err
}
func (t workReadsTx) CountWorks(q storage.WorkQuery) (int, error) {
	if t.scan {
		_, f, err := t.selected(q)
		return f.Count, err
	}
	return t.Tx.CountWorks(q)
}
func (t workReadsTx) WorkFacets(q storage.WorkQuery) (storage.WorkFacets, error) {
	if t.scan {
		_, f, err := t.selected(q)
		return f, err
	}
	return t.Tx.WorkFacets(q)
}

// TestRunsPageReadsFollowTheirAnswer: the Runs page's reads of closed
// Work (its facets, a count by a facet, a page of 50 with runs, and the
// next page) read the Work they answer with through storage's indexes,
// not the 500 closed Work behind them. The same answers from a store
// that reads every Work fail the bound.
func TestRunsPageReadsFollowTheirAnswer(t *testing.T) {
	read, scan := 0, false
	h := newManagementHarnessServing(t, managementConfig, nil, func(s storage.Store) storage.Store { return workReads{s, &read, &scan} })
	const closed = 500
	seedClosedWork(t, h, closed)
	c := h.operatorClient()
	ctx := context.Background()
	closedStates := url.Values{"state": {"completed,failed,cancelled"}}
	with := func(v url.Values, kv ...string) url.Values {
		out := url.Values{}
		for k, vs := range v {
			out[k] = vs
		}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	type answer struct{ got any }
	reads := []struct {
		name string
		read func() (any, error)
		// bound is the Work records the read may take.
		bound int
	}{
		{"facets", func() (any, error) {
			f, err := c.WorkFacets(ctx, closedStates)
			return []any{f.Total, f.ScopeIDs, f.Sources, f.Executors}, err
		}, 0},
		{"a count by a source", func() (any, error) {
			p, err := c.Works(ctx, with(closedStates, "source", "github", "sort", "-updated", "limit", "1"))
			return []any{deref(p.Total), len(p.Items)}, err
		}, 2},
		{"a count by an executor", func() (any, error) {
			p, err := c.Works(ctx, with(closedStates, "executor", "fixture", "sort", "-updated", "limit", "1"))
			return []any{deref(p.Total), len(p.Items)}, err
		}, 2},
		{"two pages of 50 with runs", func() (any, error) {
			p, err := c.WorksWithRuns(ctx, with(closedStates, "sort", "-updated", "limit", "50"), false)
			if err != nil {
				return nil, err
			}
			ids := []string{}
			for _, w := range p.Items {
				ids = append(ids, w.ID)
			}
			next, err := c.WorksWithRuns(ctx, with(closedStates, "sort", "-updated", "limit", "50", "cursor", p.NextCursor), false)
			for _, w := range next.Items {
				ids = append(ids, w.ID)
			}
			return []any{deref(p.Total), ids}, err
		}, 2 * 51},
	}
	answers := map[string]answer{}
	for _, r := range reads {
		read = 0
		got, err := r.read()
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if read > r.bound {
			t.Errorf("%s read %d Work, want at most %d", r.name, read, r.bound)
		}
		answers[r.name] = answer{got}
	}
	if a := answers["facets"].got.([]any); a[0] != closed || len(a[3].([]string)) != 1 {
		t.Errorf("facets of closed Work: %v, want %d with the fixture executor", a, closed)
	}
	if a := answers["two pages of 50 with runs"].got.([]any); len(a[1].([]string)) != 100 {
		t.Errorf("two pages listed %d Work, want 100", len(a[1].([]string)))
	}
	scan = true
	for _, r := range reads {
		read = 0
		got, err := r.read()
		if err != nil {
			t.Fatalf("negative control %s: %v", r.name, err)
		}
		if !equalAnswers(got, answers[r.name].got) {
			t.Fatalf("negative control %s answered %v, want %v", r.name, got, answers[r.name].got)
		}
		if read <= r.bound {
			t.Fatalf("negative control %s: reading every Work read %d; the bound catches nothing", r.name, read)
		}
	}
}

func equalAnswers(a, b any) bool { return fmtAnswer(a) == fmtAnswer(b) }

func fmtAnswer(v any) string { return fmt.Sprintf("%v", v) }

func deref(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
