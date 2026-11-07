package sqlite

import (
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A Work query (storage.WorkQuery) reads through expression indexes over
// the records, like works_live: each filter, order and facet has one, so
// what a query reads follows the Work it selects, not the history
// retained. Queries must repeat these exact expressions for SQLite to use
// the indexes.

// nanos is the Unix time in nanoseconds of the RFC 3339 time at path in a
// record, as Go writes it: whole seconds through unixepoch, which takes the
// offset into account, plus the fraction's digits, padded to nine. It
// orders times exactly as time.Time does, whatever their zones.
func nanos(path string) string {
	t := "json_extract(CAST(record AS TEXT),'" + path + "')"
	return "(unixepoch(" + t + ")*1000000000+CAST(substr(CASE WHEN substr(" + t + ",20,1)='.' THEN substr(" + t + ",21,length(" + t + ")-20-CASE WHEN substr(" + t + ",-1)='Z' THEN 1 ELSE 6 END) ELSE '' END||'000000000',1,9) AS INTEGER))"
}

var (
	workCreated = nanos("$.CreatedAt")
	workUpdated = nanos("$.UpdatedAt")
)

const (
	workState = "json_extract(CAST(record AS TEXT),'$.State')"
	workKind  = "json_extract(CAST(record AS TEXT),'$.Kind')"
	// workSource is storage.WorkSource: the first correlation key's
	// source, else the kind.
	workSource = "coalesce(json_extract(CAST(record AS TEXT),'$.CorrelationKeys[0].Source')," + workKind + ")"
	// attemptExecutor is an attempt's recorded executor, "" before one is.
	attemptExecutor = "coalesce(json_extract(CAST(record AS TEXT),'$.Executor'),'')"
)

// workIndexes serve storage.WorkQuery. works_created and works_updated
// list Work in either order; works_state_created and works_state_updated
// list the Work in one state in either order, so a page of Work in some
// states reads a page of each, and count Work by state; works_scope and
// works_source find Work by scope and by source, and step through their
// distinct values; works_parent finds a Work's children;
// executions_executor steps through the distinct executors, and
// executions_latest reads a Work's latest attempt's executor from the
// index.
var workIndexes = []string{
	`CREATE INDEX IF NOT EXISTS works_created ON works(` + workCreated + `, id)`,
	`CREATE INDEX IF NOT EXISTS works_updated ON works(` + workUpdated + `, id)`,
	`CREATE INDEX IF NOT EXISTS works_state_created ON works(` + workState + `, ` + workCreated + `, id)`,
	`CREATE INDEX IF NOT EXISTS works_state_updated ON works(` + workState + `, ` + workUpdated + `, id)`,
	`CREATE INDEX IF NOT EXISTS works_scope ON works(scope_id, ` + workState + `)`,
	`CREATE INDEX IF NOT EXISTS works_source ON works(` + workSource + `, ` + workState + `)`,
	`CREATE INDEX IF NOT EXISTS works_parent ON works(parent_id)`,
	`CREATE INDEX IF NOT EXISTS executions_executor ON executions(` + attemptExecutor + `)`,
	`CREATE INDEX IF NOT EXISTS executions_latest ON executions(work_id, attempt, ` + attemptExecutor + `)`,
}

// latestIs is the condition that the latest attempt of the Work works.id
// recorded the executor value (SQL), read from executions_latest.
func latestIs(value string) string {
	return "(SELECT " + attemptExecutor + " FROM executions WHERE executions.work_id=works.id ORDER BY executions.attempt DESC LIMIT 1)=" + value
}

// workWhere is q's filters as a condition on works, with its arguments.
func workWhere(q storage.WorkQuery) (string, []any) {
	conds, args := []string{"1"}, []any{}
	in := func(expr string, values []string) {
		marks := make([]string, len(values))
		for i, v := range values {
			marks[i] = "?"
			args = append(args, v)
		}
		conds = append(conds, expr+" IN ("+strings.Join(marks, ",")+")")
	}
	if len(q.States) > 0 {
		states := make([]string, len(q.States))
		for i, s := range q.States {
			states[i] = string(s)
		}
		in(workState, states)
	}
	if q.ScopeID != "" {
		conds, args = append(conds, "scope_id=?"), append(args, string(q.ScopeID))
	}
	if q.Kind != "" {
		conds, args = append(conds, workKind+"=?"), append(args, q.Kind)
	}
	if q.ParentID != "" {
		conds, args = append(conds, "parent_id=?"), append(args, string(q.ParentID))
	}
	if len(q.Sources) > 0 {
		in(workSource, q.Sources)
	}
	if q.Executor != "" {
		conds, args = append(conds, latestIs("?")), append(args, q.Executor)
	}
	if !q.UpdatedSince.IsZero() {
		conds, args = append(conds, workUpdated+">=?"), append(args, q.UpdatedSince.UnixNano())
	}
	return strings.Join(conds, " AND "), args
}

func (t *transaction) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	if !q.Order.Valid() {
		return nil, storage.ErrInvalid
	}
	query, args := queryWorks(q)
	return list[domain.Work](t, query, args...)
}

// queryWorks is QueryWorks's statement for q, with its arguments. With
// states it reads a page of each state from its ordered index and merges
// them, so a page costs a page a state, not every Work in them.
func queryWorks(q storage.WorkQuery) (string, []any) {
	key := workCreated
	if q.Order == storage.WorkUpdated || q.Order == storage.WorkUpdatedDesc {
		key = workUpdated
	}
	dir, cmp := "ASC", ">"
	if q.Order.Descending() {
		dir, cmp = "DESC", "<"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	branch := func(q storage.WorkQuery) (string, []any) {
		where, args := workWhere(q)
		if q.After != nil {
			where += " AND (" + key + ",id)" + cmp + "(?,?)"
			args = append(args, q.After.At.UnixNano(), string(q.After.ID))
		}
		return "SELECT record, " + key + " AS k, id FROM works WHERE " + where + " ORDER BY k " + dir + ", id " + dir + " LIMIT ?", append(args, limit)
	}
	if len(q.States) <= 1 {
		query, args := branch(q)
		return "SELECT record FROM (" + query + ")", args
	}
	var parts []string
	var args []any
	for _, state := range slices.Compact(slices.Sorted(slices.Values(q.States))) {
		one := q
		one.States = []domain.WorkState{state}
		query, a := branch(one)
		parts, args = append(parts, "SELECT * FROM ("+query+")"), append(args, a...)
	}
	return "SELECT record FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY k " + dir + ", id " + dir + " LIMIT ?", append(args, limit)
}

func (t *transaction) CountWorks(q storage.WorkQuery) (int, error) {
	query, args := countWorks(q)
	var n int
	err := t.queryRow(query, args...).Scan(&n)
	return n, mapError(err)
}

func countWorks(q storage.WorkQuery) (string, []any) {
	where, args := workWhere(q)
	return "SELECT count(*) FROM works WHERE " + where, args
}

// WorkFacets counts the Work q selects, then finds each distinct scope,
// source and executor by stepping through its index from one value to the
// next (each step a search, not a scan) and keeps those a selected Work
// has, each found by a search that stops at the first.
func (t *transaction) WorkFacets(q storage.WorkQuery) (storage.WorkFacets, error) {
	f := storage.WorkFacets{ScopeIDs: []domain.ID{}, Sources: []string{}, Executors: []string{}}
	var err error
	if f.Count, err = t.CountWorks(q); err != nil || f.Count == 0 {
		return f, err
	}
	scopes, sources, executors, args := facetQueries(q)
	ids, err := t.strings(scopes, args...)
	if err != nil {
		return f, err
	}
	for _, s := range ids {
		f.ScopeIDs = append(f.ScopeIDs, domain.ID(s))
	}
	if f.Sources, err = t.strings(sources, args...); err != nil {
		return f, err
	}
	f.Executors, err = t.strings(executors, args...)
	return f, err
}

// facetQueries are WorkFacets's statements for q's distinct scopes,
// sources and executors, which take the same arguments.
func facetQueries(q storage.WorkQuery) (scopes, sources, executors string, args []any) {
	where, args := workWhere(q)
	scopes = "SELECT s.id FROM scopes s WHERE EXISTS(SELECT 1 FROM works WHERE works.scope_id=s.id AND " + where + ") ORDER BY s.id"
	sources = distinct("works", workSource, "", "EXISTS(SELECT 1 FROM works WHERE "+workSource+"=v AND "+where+")")
	executors = distinct("executions", attemptExecutor, "''", "EXISTS(SELECT 1 FROM works WHERE "+latestIs("v")+" AND "+where+")")
	return scopes, sources, executors, args
}

// distinct is a query for the distinct values of expr in table above
// after (SQL; "" for none) that keep: a recursive step from each value to
// the least one above it, which reads one index entry a value.
func distinct(table, expr, after, keep string) string {
	first := "SELECT min(" + expr + ") FROM " + table
	if after != "" {
		first += " WHERE " + expr + ">" + after
	}
	return "WITH RECURSIVE d(v) AS (" + first + " UNION ALL SELECT (SELECT min(" + expr + ") FROM " + table + " WHERE " + expr + ">d.v) FROM d WHERE d.v IS NOT NULL) " +
		"SELECT v FROM d WHERE v IS NOT NULL AND " + keep + " ORDER BY v"
}

// strings reads one text column.
func (t *transaction) strings(query string, args ...any) ([]string, error) {
	rows, err := t.query(query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (t *transaction) SetExecutionExecutor(id domain.ID, executor string) error {
	x, err := t.Execution(id)
	if err != nil {
		return err
	}
	if executor == "" {
		return storage.ErrInvalid
	}
	if x.Executor != "" {
		if x.Executor == executor {
			return nil
		}
		return storage.ErrConflict
	}
	x.Executor = executor
	return t.writeExecution(x, true)
}
