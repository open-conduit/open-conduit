package localfs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyLinux(t *testing.T) {
	for _, c := range []struct {
		magic   uint32
		mount   string
		name    string
		network bool
	}{
		{0xef53, "ext4", "ext4", false},
		{0x58465342, "xfs", "xfs", false},
		{0x794c7630, "overlay", "overlay", false},
		{0x01021994, "tmpfs", "tmpfs", false},
		{0xef53, "", "0xef53", false},
		{0x6969, "nfs4", "nfs", true},
		{0xff534d42, "cifs", "cifs", true},
		{0xfe534d42, "smb3", "smb2", true},
		{0x65735546, "fuse.s3fs", "fuse.s3fs", true},
		{0x65735546, "fuse.gcsfuse", "fuse.gcsfuse", true},
		{0x65735546, "", "fuse", true},
		{0x65735546, "fuse.fuse-overlayfs", "fuse.fuse-overlayfs", false},
	} {
		got := classifyLinux(c.magic, c.mount)
		if got.Name != c.name || got.Network != c.network {
			t.Errorf("classifyLinux(%#x, %q) = %+v, want %s network=%v", c.magic, c.mount, got, c.name, c.network)
		}
	}
}

func TestClassifyDarwin(t *testing.T) {
	for name, network := range map[string]bool{"apfs": false, "hfs": false, "nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "macfuse": true, "osxfuse": true} {
		if got := classifyDarwin(name); got.Network != network || got.Name != name {
			t.Errorf("classifyDarwin(%q) = %+v, want network=%v", name, got, network)
		}
	}
}

func TestMountTypeTakesTheDeepestLatestMount(t *testing.T) {
	const info = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 22 0:40 / /mnt/state rw - nfs4 server:/export rw
31 22 0:41 / /mnt/state rw - fuse.s3fs s3fs rw
32 22 0:42 / /mnt/with\040space rw shared:2 master:3 - cifs //server/share rw
33 22 0:43 / /mnt/stat rw - tmpfs tmpfs rw
`
	for path, want := range map[string]string{
		"/home/conduit":        "ext4",
		"/mnt/state":           "fuse.s3fs",
		"/mnt/state/db":        "fuse.s3fs",
		"/mnt/with space/data": "cifs",
		"/mnt/stat/x":          "tmpfs",
		"/mnt/statement":       "ext4",
	} {
		if got := mountType(info, path); got != want {
			t.Errorf("mountType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRequireRefusesANetworkFilesystemByName(t *testing.T) {
	defer SwapProbe(func(string) (Info, error) { return Info{Name: "nfs", Network: true}, nil })()
	err := Require(t.TempDir(), "state directory")
	if !errors.Is(err, ErrNetwork) || !errors.Is(err, ErrNotLocal) || !strings.Contains(err.Error(), "nfs") || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("Require = %v", err)
	}
}

func TestRequireFailsClosedWhenItCannotTell(t *testing.T) {
	boom := errors.New("statfs failed")
	defer SwapProbe(func(string) (Info, error) { return Info{}, boom })()
	if err := Require(t.TempDir(), "state directory"); !errors.Is(err, boom) || !errors.Is(err, ErrNotLocal) || errors.Is(err, ErrNetwork) {
		t.Fatalf("Require = %v, want the probe's error as a refusal", err)
	}
}

func TestDetectJudgesAMissingPathByItsAncestor(t *testing.T) {
	dir := t.TempDir()
	var seen string
	defer SwapProbe(func(p string) (Info, error) { seen = p; return Info{Name: "ext4"}, nil })()
	if _, err := Detect(filepath.Join(dir, "not", "yet")); err != nil {
		t.Fatal(err)
	}
	if seen != dir {
		t.Fatalf("probed %q, want %q", seen, dir)
	}
}

func TestDetectReadsTheRealFilesystem(t *testing.T) {
	info, err := Detect(t.TempDir())
	if err != nil || info.Name == "" {
		t.Fatalf("Detect = %+v, %v", info, err)
	}
}
