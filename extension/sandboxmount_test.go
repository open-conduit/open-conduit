package extension

import (
	"slices"
	"testing"
)

// A bind mount of the state directory, or of a directory holding it, is
// another path to the same files: it is denied too.
func TestMountAliases(t *testing.T) {
	info := `22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw
23 22 0:5 / /proc rw - proc proc rw
40 22 8:1 /home/op/.local/state/oc /mnt/alias rw - ext4 /dev/sda1 rw
41 22 8:1 /home/op /srv/home\040dir rw - ext4 /dev/sda1 rw
42 22 8:1 /var /mnt/var rw - ext4 /dev/sda1 rw
43 22 8:2 / /data rw - ext4 /dev/sdb1 rw`
	got := parseMountAliases(info, "/home/op/.local/state/oc")
	want := []string{"/mnt/alias", "/srv/home dir/.local/state/oc"}
	if !slices.Equal(got, want) {
		t.Fatalf("aliases %q, want %q", got, want)
	}
	// On a bind-mounted filesystem the path's place is the mount's root.
	if got := parseMountAliases(info, "/mnt/alias/private"); !slices.Equal(got, []string{"/home/op/.local/state/oc/private", "/srv/home dir/.local/state/oc/private"}) {
		t.Fatalf("aliases of a path on a bind mount: %q", got)
	}
	if got := parseMountAliases(info, "/data/x"); len(got) != 0 {
		t.Fatalf("a filesystem mounted once has no aliases: %q", got)
	}
}
