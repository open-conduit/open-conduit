//go:build !unix

package extension

import "io/fs"

func fileIdentity(fs.FileInfo) (dev, ino, nlink uint64, ok bool) { return 0, 0, 0, false }
