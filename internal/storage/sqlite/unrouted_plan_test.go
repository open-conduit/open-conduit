package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// TestUnroutedReadsSearchIndexes: the reads routing makes for every Event
// (UnroutedInput, and MarkRouted's check for earlier input) and the
// scheduler's read of its own timer Events (UnroutedOfType) search the
// unrouted backlog's indexes and never scan events, the backlog included,
// so draining a backlog costs each route its Work's own input (#705).
// UnroutedEvents, which reads the whole backlog, is the control.
func TestUnroutedReadsSearchIndexes(t *testing.T) {
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
	// backlog is whether a plan reads events other than through the
	// targeted indexes or the primary key: a scan, or a range of the whole
	// backlog's index.
	backlog := func(p string) bool {
		for _, step := range strings.Split(p, " | ") {
			if strings.HasPrefix(step, "SCAN correlations") || strings.HasPrefix(step, "SCAN executions") {
				return true
			}
			if !strings.Contains(step, " events ") && !strings.HasSuffix(step, " events") {
				continue
			}
			if strings.HasPrefix(step, "SCAN") || !strings.Contains(step, "events_unrouted_") && !strings.Contains(step, "INTEGER PRIMARY KEY") {
				return true
			}
		}
		return false
	}
	check := func(label, query string, args []any) {
		t.Helper()
		if p := plan(query, args); backlog(p) {
			t.Errorf("%s reads the backlog: %s", label, p)
		}
	}
	// The usage feed's read searches its type's index from the cursor, in
	// sequence order, so a page never sorts or walks the retained log.
	for _, scopes := range [][]domain.ID{nil, {"a", ""}} {
		q, args := typeEventsQuery(domain.TypeExecutionUsage, scopes, 10, 200)
		if p := plan(q, args); !strings.HasPrefix(p, "SEARCH events USING INDEX events_type (") || !strings.Contains(p, "sequence>?") && !strings.Contains(p, "rowid>?") || strings.Contains(p, "TEMP B-TREE") {
			t.Errorf("the type read with scopes %v: %s", scopes, p)
		}
	}
	keys := []domain.CorrelationKey{{Source: "github", ExternalID: "1"}, {Source: "slack", ExternalID: "2"}}
	for _, target := range []storage.InputTarget{{Work: "w", Keys: keys}, {Work: "w"}, {Keys: keys[:1]}} {
		for _, bounds := range [][2]uint64{{0, 0}, {10, 500}} {
			query, args := unroutedInput(target, []string{"x1", "x2"}, bounds[0], bounds[1])
			check(fmt.Sprintf("UnroutedInput %+v %v", target, bounds), query, args)
		}
	}
	query, args := unroutedOfType([]string{"core.retry.due", domain.TypeExecutionTimeout})
	check("UnroutedOfType", query, args)
	query, args = earlierInput("w", keys, 10)
	check("MarkRouted's earlier input", query, args)
	if p := plan("SELECT record FROM events WHERE sequence>? AND "+unrouted+" ORDER BY sequence LIMIT ?", []any{0, -1}); !backlog(p) {
		t.Errorf("control: UnroutedEvents does not read the backlog: %s", p)
	}
}
