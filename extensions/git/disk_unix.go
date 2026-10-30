//go:build linux || darwin

package git

import "golang.org/x/sys/unix"

// diskSpace is the size of the filesystem path is on and what an
// unprivileged process can still write to it.
func diskSpace(path string) (total, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), nil
}

// sameDevice reports whether a and b are on one filesystem, so a file of
// one can be hardlinked into the other.
func sameDevice(a, b string) bool {
	var sa, sb unix.Stat_t
	return unix.Stat(a, &sa) == nil && unix.Stat(b, &sb) == nil && sa.Dev == sb.Dev
}
