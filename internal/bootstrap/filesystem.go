package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// filesystemCheck is doctor's report of the disks a running daemon uses.
func (d *daemon) filesystemCheck(context.Context) runtime.Check {
	backupDir := d.o.BackupDir
	if d.o.BackupInterval < 0 {
		backupDir = ""
	}
	return FilesystemCheck(d.o.StateDir, backupDir)
}

// filesystemCode is the check code of a localfs refusal.
func filesystemCode(err error) string {
	if errors.Is(err, localfs.ErrNetwork) {
		return "network_filesystem"
	}
	return "filesystem_unreadable"
}

// FilesystemCheck reports the state directory's filesystem and fails on
// one that can't hold live state. A backup directory (optional) on a
// network filesystem is a warning: a bundle has one writer and digests that
// catch a bad copy, but it still relies on that mount's renames and syncs.
func FilesystemCheck(stateDir, backupDir string) runtime.Check {
	check := runtime.Check{Name: "filesystem", Status: runtime.CheckOK, Code: "local"}
	state, err := localfs.Check(stateDir, "state directory")
	if err != nil {
		// The same refusal that stops the daemon starting.
		check.Status, check.Code, check.Detail = runtime.CheckFail, filesystemCode(err), err.Error()
		return check
	}
	check.Detail = "state on " + state.Name
	if backupDir == "" {
		return check
	}
	switch backups, err := localfs.Detect(backupDir); {
	case err != nil:
		check.Status, check.Code = runtime.CheckWarn, "backup_unknown"
		check.Items = append(check.Items, fmt.Sprintf("backup directory %s: %v", backupDir, err))
	case backups.Network:
		check.Status, check.Code = runtime.CheckWarn, "backup_network_filesystem"
		check.Items = append(check.Items, fmt.Sprintf("backup directory %s is on %s: run backup verify on its bundles, since a bundle relies on that mount's renames and syncs", backupDir, backups.Name))
	default:
		check.Detail += ", backups on " + backups.Name
	}
	return check
}
