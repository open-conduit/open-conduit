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
