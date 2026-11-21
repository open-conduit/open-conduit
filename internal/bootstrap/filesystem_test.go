package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// onNetwork makes every path under dir report nfs, and the rest ext4.
func onNetwork(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(localfs.SwapProbe(func(p string) (localfs.Info, error) {
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return localfs.Info{Name: "nfs", Network: true}, nil
		}
		return localfs.Info{Name: "ext4"}, nil
	}))
}

func TestLiveStateIsRefusedOnANetworkFilesystem(t *testing.T) {
	dir := t.TempDir()
	onNetwork(t, dir)
	if _, err := management.AcquireLock(filepath.Join(dir, "state")); !errors.Is(err, localfs.ErrNetwork) {
		t.Errorf("AcquireLock = %v", err)
	}
	if _, err := sqlite.Open(context.Background(), filepath.Join(dir, "conduit.db")); !errors.Is(err, localfs.ErrNetwork) {
		t.Errorf("sqlite.Open = %v", err)
	}
	if _, err := artifacts.Open(filepath.Join(dir, "artifacts")); !errors.Is(err, localfs.ErrNetwork) {
		t.Errorf("artifacts.Open = %v", err)
	}
	// The negative control: a bundle's own store may be written there.
	s, err := artifacts.OpenBundle(filepath.Join(dir, "bundle"))
	if err != nil {
		t.Fatalf("OpenBundle = %v", err)
	}
	s.Close()
}

func TestLiveStateOpensOnALocalFilesystem(t *testing.T) {
	dir := t.TempDir()
	onNetwork(t, filepath.Join(dir, "elsewhere"))
	lock, err := management.AcquireLock(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestStateCheckNamesANetworkFilesystem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := sqlite.Open(context.Background(), filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	onNetwork(t, dir)
	r, err := CheckState(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || len(r.Checks) != 1 || r.Checks[0].Name != "filesystem" || r.Checks[0].Code != "network_filesystem" || strings.Contains(r.Checks[0].Message, "stop the daemon") {
		t.Fatalf("report = %+v", r)
	}
}

func TestFilesystemCheck(t *testing.T) {
	root := t.TempDir()
	network, local := filepath.Join(root, "nfs"), filepath.Join(root, "disk")
	for _, d := range []string{network, local} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	onNetwork(t, network)
	for _, c := range []struct {
		name, state, backups, status, code string
	}{
		{"local", local, local, runtime.CheckOK, "local"},
		{"state on network", network, local, runtime.CheckFail, "network_filesystem"},
		{"backups on network", local, network, runtime.CheckWarn, "backup_network_filesystem"},
	} {
		if got := FilesystemCheck(c.state, c.backups); got.Status != c.status || got.Code != c.code {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// A bundle may go to a network mount and be verified from it; restoring
// live state onto one is refused and publishes nothing.
func TestBundleMayBeWrittenToANetworkMountButNotRestoredOntoOne(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, mount := filepath.Join(root, "state"), filepath.Join(root, "mount")
	if err := os.Mkdir(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	onNetwork(t, mount)
	bundle := filepath.Join(mount, "bundle")
	if _, err := BackupState(ctx, state, bundle, "test", cli.BackupOptions{}); err != nil {
		t.Fatalf("BackupState to a network mount: %v", err)
	}
	if _, err := VerifyBundle(ctx, bundle, cli.RestoreOptions{}); err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	restored := filepath.Join(mount, "restored")
	if _, err := RestoreState(ctx, bundle, restored, cli.RestoreOptions{}); !errors.Is(err, localfs.ErrNetwork) {
		t.Fatalf("RestoreState onto a network mount = %v", err)
	}
	if _, err := os.Lstat(restored); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused restore left %s: %v", restored, err)
	}
}

// A filesystem that can't be read stops the daemon, so every report says
// that, and none sends the operator after a daemon that isn't running.
func TestAnUnreadableFilesystemIsReportedAsARefusal(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := BackupState(ctx, dir, bundle, "test", cli.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(localfs.SwapProbe(func(string) (localfs.Info, error) {
		return localfs.Info{}, errors.New("statfs failed")
	}))
	if got := FilesystemCheck(dir, ""); got.Status != runtime.CheckFail || got.Code != "filesystem_unreadable" {
		t.Errorf("FilesystemCheck = %+v", got)
	}
	r, err := CheckState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "filesystem" || r.Checks[0].Code != "filesystem_unreadable" || strings.Contains(r.Checks[0].Message, "stop the daemon") {
		t.Errorf("CheckState = %+v", r)
	}
	if _, err := locked(dir); !errors.Is(err, localfs.ErrNotLocal) || strings.Contains(err.Error(), "stop the daemon") {
		t.Errorf("locked = %v", err)
	}
	if _, err := RestoreInPlace(ctx, bundle, dir, cli.RestoreOptions{}); !errors.Is(err, localfs.ErrNotLocal) || strings.Contains(err.Error(), "stop the daemon") {
		t.Errorf("RestoreInPlace = %v", err)
	}
}

// A bundle fetched from a destination may land on a network mount, as one
// written by a backup may.
func TestBundleMayBeFetchedOntoANetworkMount(t *testing.T) {
	m, public, name := shippedBundle(t)
	mount := t.TempDir()
	onNetwork(t, mount)
	fetched := filepath.Join(mount, "fetched")
	o := cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "absent.json"), TrustKeys: []string{public}}
	if _, err := fetchBundle(context.Background(), m, extension.BackupTarget{}, name, fetched, o); err != nil {
		t.Fatalf("fetchBundle onto a network mount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fetched, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
