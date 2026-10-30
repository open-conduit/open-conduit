//go:build unix

package extension

import (
	"io/fs"
	"syscall"
)

func fileIdentity(info fs.FileInfo) (dev, ino, nlink uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), true
}
