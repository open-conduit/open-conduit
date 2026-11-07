package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// TestWorkQueriesUseIndexes: every Work query reads works and executions
// through an index, never a scan of the table, and a page of Work in
// several states sorts only the merged pages (one temporary b-tree), not
// every Work in them. A query by an executor searches that executor's
// index, so it reads that executor's Work, not every Work its other
// filters select. A query by a field with no index is the control.
func TestWorkQueriesUseIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := func(query string, args []any) string {
		rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, " | ")
	}
	scans := func(p string) bool {
		for _, step := range strings.Split(p, " | ") {
			if (strings.HasPrefix(step, "SCAN works") || strings.HasPrefix(step, "SCAN executions")) && !strings.Contains(step, "INDEX") {
				return true
			}
		}
		return false
	}
	closed := []domain.WorkState{domain.WorkCompleted, domain.WorkFailed, domain.WorkCancelled}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, q := range []storage.WorkQuery{
		{States: closed, Order: storage.WorkUpdatedDesc, Limit: 51},
		{States: closed, Order: storage.WorkCreated, After: &storage.WorkKey{At: at, ID: "x"}, Limit: 51},
		{Order: storage.WorkCreated, Limit: 51},
		{Order: storage.WorkUpdatedDesc, UpdatedSince: at, Limit: 51},
		{States: []domain.WorkState{domain.WorkReady}, Order: storage.WorkUpdated, Limit: 51},
		{States: closed, Sources: []string{"github.issue"}, Order: storage.WorkUpdatedDesc, Limit: 51},
		{States: closed, Executor: "claude", Order: storage.WorkUpdatedDesc, Limit: 51},
		{Executor: "claude", Order: storage.WorkCreated, Limit: 51},
		{States: []domain.WorkState{domain.WorkCompleted}, ScopeID: "ops", Executor: "claude", Order: storage.WorkCreatedDesc, Limit: 51},
		{States: closed, UpdatedSince: at, Order: storage.WorkUpdatedDesc, Limit: 51},
		{States: closed, ScopeID: "ops", Kind: "x.y", ParentID: "p", Order: storage.WorkCreatedDesc, Limit: 51},
	} {
		query, args := queryWorks(q)
		p := plan(query, args)
		if scans(p) {
			t.Errorf("QueryWorks %+v scans: %s", q, p)
		}
		if len(q.States) > 1 && strings.Count(p, "TEMP B-TREE") != 1 {
			t.Errorf("QueryWorks %+v sorts more than the merged pages: %s", q, p)
		}
		byExecutor := func(what, p string) {
			// With a scope, the scope's index is as narrow a range.
			if q.Executor != "" && (!strings.Contains(p, "works_executor_") && q.ScopeID == "" || strings.Contains(p, "CORRELATED")) {
				t.Errorf("%s %+v doesn't search the executor's index: %s", what, q, p)
			}
		}
		byExecutor("QueryWorks", p)
		query, args = countWorks(q)
		if p = plan(query, args); scans(p) {
			t.Errorf("CountWorks %+v scans: %s", q, p)
		}
		byExecutor("CountWorks", p)
		scopes, sources, executors, args := facetQueries(q)
		for _, f := range []string{scopes, sources, executors} {
			if p = plan(f, args); scans(p) {
				t.Errorf("WorkFacets %+v scans: %s", q, p)
			}
		}
		if p = plan(executors, args); !strings.Contains(p, "works_executor_") {
			t.Errorf("WorkFacets %+v doesn't step through the executors' index: %s", q, p)
		}
	}
	if p := plan("SELECT record FROM works WHERE json_extract(CAST(record AS TEXT),'$.Context.Schema')=?", []any{"x"}); !scans(p) {
		t.Errorf("control: a query by an unindexed field reads as no scan: %s", p)
	}
}
