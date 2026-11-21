package claudecode

import (
	"context"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// claude files a session under a key made from its working directory, so a
// contained Work whose workspace moved with the state directory finds its
// carried transcript under the old path's key. adoptMovedSession refiles it
// under the new one, and the turn resumes.
//
// Only a contained Work does this: its home is its own, whereas a host-side
// claude's config is the operator's and doesn't travel with the state. And
// only when the old path is gone: a path that still exists is another
// workspace, whose session this turn must not take.
func (e *Executor) adoptMovedSession(ctx context.Context, r extension.RunRequest, configDir, workdir string) {
	var rec record
	err := e.o.Store.Transact(ctx, e.guard(r.Execution.ID, r.Execution.Lease), func(tx storage.Tx) error {
		var err error
		rec, err = e.loadRecord(tx, r.Work)
		return err
	})
	if err != nil || rec.Session == "" || rec.Attach == nil || !rec.Attach.Container {
		return
	}
	moved, err := refileSession(configDir, []string{rec.Attach.RealDir, rec.Attach.Dir}, workdir, rec.Session)
	attrs := map[string]any{"work": string(r.Work.ID), "execution": string(r.Execution.ID), "session": rec.Session}
	switch {
	case err != nil:
		// The turn goes on from the handoff note, as for any missing session.
		attrs["error"] = err.Error()
		e.report("session_move_failed", attrs)
	case moved:
		e.report("session_moved", attrs)
	}
}
