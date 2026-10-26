//go:build linux || darwin

package host

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// diskSpace is the size of the filesystem path is on, what an unprivileged process can still write to it and
// the filesystem's id, which paths on the same one share.
func diskSpace(path string) (total, free uint64, id string, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, "", err
	}
	var s unix.Stat_t
	if err := unix.Stat(path, &s); err != nil {
		return 0, 0, "", err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), strconv.FormatUint(uint64(s.Dev), 10), nil
}
