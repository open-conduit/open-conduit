// Package localfs tells whether a path is on a filesystem that can hold live
// state. The daemon's exclusion and durability rest on SQLite's WAL (shared
// memory and POSIX locks), flock, atomic rename and directory sync, none of
// which a network or FUSE filesystem reliably provides across clients, so
// live state on one is refused rather than risked.
package localfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotLocal is every refusal of a path for live state: one on a network
// filesystem (ErrNetwork) and one whose filesystem can't be read.
var ErrNotLocal = errors.New("not a local filesystem")

// ErrNetwork is the refusal of a path on a network or FUSE filesystem.
var ErrNetwork = errors.New("network filesystem")

// refusal is an error that also matches ErrNotLocal.
type refusal struct{ error }

func (r refusal) Unwrap() error      { return r.error }
func (refusal) Is(target error) bool { return target == ErrNotLocal }

// Info is the filesystem a path is on.
type Info struct {
	// Name is the filesystem's type, such as ext4, apfs, nfs or fuse.s3fs.
	Name string
	// Network is true for a filesystem that can't hold live state.
	Network bool
}

// probe reads the filesystem of an existing path; tests replace it.
var probe = detect

// SwapProbe replaces the detection and returns what restores it. It is for
// tests, which can't mount a network filesystem.
func SwapProbe(fn func(path string) (Info, error)) (restore func()) {
	old := probe
	probe = fn
	return func() { probe = old }
}

// Detect reports the filesystem path is on, or would be on once created: a
// path that doesn't exist yet is judged by its nearest existing ancestor.
func Detect(path string) (Info, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Info{}, err
	}
	for {
		_, err := os.Stat(abs)
		if err == nil {
			return probe(abs)
		}
		parent := filepath.Dir(abs)
		if !errors.Is(err, os.ErrNotExist) || parent == abs {
			return Info{}, err
		}
		abs = parent
	}
}

// Require refuses a path that can't hold live state, and one whose
// filesystem can't be read: it fails closed. what names the state in the
// error, such as "state directory".
func Require(path, what string) error {
	_, err := Check(path, what)
	return err
}

// Check is Require that also returns the filesystem it judged.
func Check(path, what string) (Info, error) {
	info, err := Detect(path)
	if err != nil {
		return info, refusal{fmt.Errorf("%s %s: cannot determine its filesystem: %w", what, path, err)}
	}
	if info.Network {
		return info, refusal{fmt.Errorf("%s %s is on %s, a %w: its locks, fsync and rename can't protect a live database, so it could be silently corrupted. Move it to a local or block disk (such as EBS, Persistent Disk or Azure Disk)", what, path, info.Name, ErrNetwork)}
	}
	return info, nil
}

// linuxNetworkMagic names the statfs f_type of each Linux filesystem whose
// locking or durability is shared across clients or delegated to a server.
var linuxNetworkMagic = map[uint32]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x65735546: "fuse",
	0x00c36400: "ceph",
	0x5346414f: "afs",
	0x6b414653: "afs",
	0x73757245: "coda",
	0x564c:     "ncp",
	0x01021997: "9p",
	0x7461636f: "ocfs2",
	0x01161970: "gfs2",
	0x0bd00bd0: "lustre",
	0x47504653: "gpfs",
	0x20030528: "pvfs2",
	0x786f4256: "vboxsf",
}

// localFUSE are the FUSE filesystems that are a single host's own disk:
// fuse-overlayfs is the root filesystem of a rootless container.
var localFUSE = map[string]bool{"fuse.fuse-overlayfs": true}

// classifyLinux judges a statfs f_type. mountType is the path's type in
// mountinfo, which is what names a FUSE filesystem; "" when unknown.
func classifyLinux(magic uint32, mountType string) Info {
	name, network := linuxNetworkMagic[magic]
	if !network {
		if mountType == "" {
			mountType = fmt.Sprintf("0x%x", magic)
		}
		return Info{Name: mountType}
	}
	if name == "fuse" && mountType != "" {
		return Info{Name: mountType, Network: !localFUSE[mountType]}
	}
	return Info{Name: name, Network: true}
}

// classifyDarwin judges a statfs f_fstypename.
func classifyDarwin(name string) Info {
	switch {
	case name == "nfs", name == "smbfs", name == "afpfs", name == "webdav", name == "cifs", name == "vboxsf":
		return Info{Name: name, Network: true}
	case strings.Contains(name, "fuse"):
		return Info{Name: name, Network: true}
	}
	return Info{Name: name}
}

// mountType finds path's filesystem type in the text of
// /proc/self/mountinfo: the entry with the longest mount point containing
// it, the last one winning a tie because a later mount covers an earlier.
func mountType(mountinfo, path string) string {
	best, kind := -1, ""
	for _, line := range strings.Split(mountinfo, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		fields, tail := strings.Fields(left), strings.Fields(right)
		if !ok || len(fields) < 5 || len(tail) == 0 {
			continue
		}
		point := unescapeMount(fields[4])
		if !contains(point, path) || len(point) < best {
			continue
		}
		best, kind = len(point), tail[0]
	}
	return kind
}

func contains(point, path string) bool {
	return point == "/" || path == point || strings.HasPrefix(path, point+"/")
}

// unescapeMount undoes mountinfo's octal escapes of a path's separators.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
