package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// executionFile, in an execution's run directory, names the execution it
// belongs to, so SweepState can map the directory back to it.
const executionFile = "execution"

// attachFile marks a Work's attach directory (ContainerAttach), which
// SweepState leaves alone: it is the Work's, made afresh on each attach.
const attachFile = "attach"

// recordExecution makes an execution's run directory and names the
// execution in it, before any segment of it is made.
func recordExecution(base string, id domain.ID) error {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(base, executionFile)); err == nil && string(b) == string(id)+"\n" {
		return nil
	}
	return writeFile(filepath.Join(base, executionFile), []byte(string(id)+"\n"))
}

// runExecution is the execution a run directory belongs to: the one it
// names, or for a directory made before it named one, the one a segment's
// turn record names. It is false when neither names one that hashes to the
// directory's key.
func runExecution(base string) (domain.ID, bool) {
	key := filepath.Base(base)
	if b, err := os.ReadFile(filepath.Join(base, executionFile)); err == nil {
		id := domain.ID(strings.TrimSpace(string(b)))
		return id, id != "" && hashKey(string(id)) == key
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !hexName.MatchString(entry.Name()) {
			continue
		}
		var turn turnRecord
		if readJSON(filepath.Join(base, entry.Name(), "turn.json"), &turn) == nil && turn.ExecutionID != "" && hashKey(turn.ExecutionID) == key {
			return domain.ID(turn.ExecutionID), true
		}
	}
	return "", false
}

// SweepState releases the run directories of executions known leaves out,
// as Forget does (extension.StateSweeper). A Work's attach directory is
// not an execution's and is left alone; another directory naming no
// execution is kept as unmapped, and one whose segment still runs as
// in_use. Work continuity is Maintain's, as with Forget.
func (e *Executor) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	var report extension.StateSweepReport
	root := filepath.Join(e.o.StateDir, "runs")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	} else if err != nil {
		return report, err
	}
	dirs := map[domain.ID]string{}
	var ids []domain.ID
	for _, entry := range entries {
		if !entry.IsDir() || !hexName.MatchString(entry.Name()) {
			continue
		}
		base := filepath.Join(root, entry.Name())
		if _, err := os.Lstat(filepath.Join(base, attachFile)); err == nil {
			continue
		}
		id, ok := runExecution(base)
		if !ok {
			report.Kept = append(report.Kept, extension.StateKept{Ref: base, Kept: "unmapped"})
			continue
		}
		dirs[id] = base
		ids = append(ids, id)
	}
	slices.Sort(ids)
	got, err := extension.KnownExecutions(ctx, known, ids)
	if err != nil {
		return report, err
	}
	for _, id := range ids {
		if got[id] {
			continue
		}
		switch err := e.Forget(ctx, id); {
		case err == nil:
			report.Removed = append(report.Removed, id)
		case errors.Is(err, ErrLiveSegment):
			report.Kept = append(report.Kept, extension.StateKept{Execution: id, Ref: dirs[id], Kept: "in_use"})
		case ctx.Err() != nil:
			return report, ctx.Err()
		default:
			report.Kept = append(report.Kept, extension.StateKept{Execution: id, Ref: dirs[id], Kept: "remove_failed"})
		}
	}
	return report, nil
}

var _ extension.StateSweeper = (*Executor)(nil)
