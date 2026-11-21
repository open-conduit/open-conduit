//go:build linux

package localfs

import (
	"os"
	"path/filepath"
	"syscall"
)

func detect(path string) (Info, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Info{}, err
	}
	// mountinfo only adds a name, and tells a local FUSE mount from a remote
	// one. Without it a FUSE mount stays refused.
	kind := ""
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		resolved := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			resolved = r
		}
		kind = mountType(string(data), resolved)
	}
	return classifyLinux(uint32(st.Type), kind), nil
}
