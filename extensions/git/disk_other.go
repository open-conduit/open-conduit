//go:build !linux && !darwin

package git

import "errors"

// diskSpace is unsupported here; a disk the provider can't read doesn't stop
// a workspace (B06).
func diskSpace(string) (total, free uint64, err error) {
	return 0, 0, errors.New("disk space is not readable on this platform")
}

// sameDevice is unknown here, so a clone is sized as a full copy.
func sameDevice(string, string) bool { return false }
