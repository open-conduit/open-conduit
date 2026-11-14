package management

import (
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// One execution holds at most maxExecutionGitSessions sessions, its oldest
// evicted for a new one, so it can't fill the host's table; another
// execution's are untouched. Reconcile drops an ended execution's sessions.
func TestGitSessionsPerExecution(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	g := NewGitTransport(nil, func() time.Time { return now })
	other, _, err := g.open(gitSession{execution: "exec/b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < maxExecutionGitSessions+3; i++ {
		now = now.Add(time.Second)
		id, _, err := g.open(gitSession{execution: "exec/a"}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	held := 0
	for _, id := range ids {
		if _, ok := g.session(id, "exec/a"); ok {
			held++
		}
	}
	if held != maxExecutionGitSessions {
		t.Fatalf("exec/a holds %d sessions, want %d", held, maxExecutionGitSessions)
	}
	if _, ok := g.session(ids[0], "exec/a"); ok {
		t.Fatal("the oldest session wasn't the one evicted")
	}
	if _, ok := g.session(ids[len(ids)-1], "exec/a"); !ok {
		t.Fatal("the newest session was evicted")
	}
	if _, ok := g.session(other, "exec/b"); !ok {
		t.Fatal("another execution's session was evicted")
	}
	// A session opened while the sweep asks who is running belongs to an
	// execution it didn't ask about, and stays.
	var late string
	if err := g.Reconcile(func([]domain.ID) (map[domain.ID]bool, error) {
		late, _, _ = g.open(gitSession{execution: "exec/c"}, "")
		return map[domain.ID]bool{"exec/b": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.session(ids[len(ids)-1], "exec/a"); ok {
		t.Fatal("an ended execution's session outlived reconcile")
	}
	if _, ok := g.session(other, "exec/b"); !ok {
		t.Fatal("reconcile dropped a running execution's session")
	}
	if _, ok := g.session(late, "exec/c"); !ok {
		t.Fatal("reconcile dropped a session opened while it asked")
	}
}
