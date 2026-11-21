package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

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

// refileSession moves the session's transcript, and the directory of tool
// results and subagents beside it, from the first of the old paths that holds
// it to the key claude will look under in workdir. It reports whether it moved
// one.
//
// The agent can write to this directory, so nothing here follows a link:
// every step works from a descriptor opened without following one, and only a
// regular file is moved. The transcript is linked into place, which never
// replaces a file, and goes last, so a move that stops part way is finished by
// the next turn.
func refileSession(configDir string, from []string, workdir, session string) (bool, error) {
	if transcriptExists(configDir, workdir, session) {
		return false, nil
	}
	to := workdir
	if real, err := filepath.EvalSymlinks(workdir); err == nil {
		to = real
	}
	toKey := nonAlnum.ReplaceAllString(to, "-")
	projects, err := openDirNoFollow(unix.AT_FDCWD, filepath.Join(configDir, "projects"))
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer unix.Close(projects)
	name := session + ".jsonl"
	for _, old := range from {
		if old == "" || old == workdir || old == to {
			continue
		}
		if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
			return false, nil // Still there, or unreadable: not known to have moved.
		}
		fromKey := nonAlnum.ReplaceAllString(old, "-")
		if fromKey == toKey {
			continue
		}
		src, err := openDirNoFollow(projects, fromKey)
		if errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return false, err
		}
		defer unix.Close(src)
		var st unix.Stat_t
		if err := unix.Fstatat(src, name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return false, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return false, errors.New("the session's transcript is not a regular file")
		}
		if err := unix.Mkdirat(projects, toKey, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return false, err
		}
		dst, err := openDirNoFollow(projects, toKey)
		if err != nil {
			return false, err
		}
		defer unix.Close(dst)
		var side unix.Stat_t
		if err := unix.Fstatat(src, session, &side, unix.AT_SYMLINK_NOFOLLOW); err == nil && side.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := unix.Renameat(src, session, dst, session); err != nil && !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.ENOTEMPTY) {
				return false, err
			}
		}
		if err := unix.Linkat(src, name, dst, name, 0); err != nil {
			return false, err
		}
		return true, unix.Unlinkat(src, name, 0)
	}
	return false, nil
}

// openDirNoFollow opens a directory, refusing a symbolic link at its last
// component.
func openDirNoFollow(at int, path string) (int, error) {
	for {
		fd, err := unix.Openat(at, path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}
