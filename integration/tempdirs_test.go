package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// What a test binary keeps for its whole run lives in the temp directory in
// two kinds of directory, each holding a flock on itself for as long as a
// live process uses it. The kernel drops the lock however the process ends,
// including a test timeout's panic and SIGKILL, so a directory whose lock is
// free belongs to nobody, and every run reclaims those at start and at exit
// (#445, #841).
//
//   - A run root (runRootPrefix) is one process's, locked exclusively.
//   - A shared build (sharedBuildPrefix) is the output of one build, named by
//     what went into it. Its builder locks it exclusively, and every process
//     using it holds a shared lock, so parallel runs of the same code keep
//     one copy, which goes when the last of them does.
//
// The lock is on the directory itself, so nothing can sit between making a
// directory and owning it except the check lockDir makes: that the path
// still names the directory it locked.
//
// Run roots are not named oc-test-run-*: a checkout from before these locks
// makes those and holds no lock on them, so a free lock there wouldn't mean
// the run is dead.
const (
	runRootPrefix     = "oc-test-root-"
	sharedBuildPrefix = "oc-test-build-"
	sharedReadyFile   = ".ready"
)

var (
	// tempBase is fixed at start, so a test's t.Setenv("TMPDIR", ...) can't
	// move what outlives that test into a directory that test removes.
	tempBase = os.TempDir()

	runDirMu    sync.Mutex
	runDir      string
	runLock     *os.File
	sharedLocks []*os.File
)

var (
	errDirGone    = errors.New("directory was reclaimed")
	errDirNotOurs = errors.New("directory is not this user's alone")
)

// lockDir takes how (a syscall.LOCK_* mode) on the directory at path, and
// returns the open file holding it. errDirGone means path no longer names the
// directory that was locked: another process reclaimed it first.
//
// The temp directory can be shared and a shared build's name is predictable,
// so another user can put something at path. errDirNotOurs refuses it, before
// the lock so that they can't stall this run by holding one.
func lockDir(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, errDirGone
		case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR), errors.Is(err, fs.ErrPermission):
			return nil, errDirNotOurs
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !dirIsPrivate(info, os.Getuid()) {
		_ = f.Close()
		return nil, errDirNotOurs
	}
	if err = flock(f, how); err != nil {
		_ = f.Close()
		return nil, err
	}
	if !sameDir(f, path) {
		_ = f.Close()
		return nil, errDirGone
	}
	return f, nil
}

// dirIsPrivate reports whether info is a directory uid owns and nobody else
// can write to.
func dirIsPrivate(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && int(st.Uid) == uid && info.Mode().Perm()&0o022 == 0
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func sameDir(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Lstat(path)
	return err == nil && os.SameFile(held, now)
}

// keptDir is a new directory, named from pattern as os.MkdirTemp names it,
// that outlives any one test; TestMain removes it after the run.
func keptDir(pattern string) (string, error) {
	runDirMu.Lock()
	defer runDirMu.Unlock()
	if runDir == "" {
		dir, lock, err := newRunRoot(tempBase)
		if err != nil {
			return "", err
		}
		runDir, runLock = dir, lock
	}
	return os.MkdirTemp(runDir, pattern)
}

// newRunRoot makes a run root under base and locks it. Another run's reclaim
// can take a root in the moment before its lock, so that case makes another.
func newRunRoot(base string) (string, *os.File, error) {
	for range 16 {
		dir, err := os.MkdirTemp(base, runRootPrefix)
		if err != nil {
			return "", nil, err
		}
		lock, err := lockDir(dir, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return dir, lock, nil
		}
		if !errors.Is(err, errDirGone) && !errors.Is(err, syscall.EWOULDBLOCK) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("could not lock a new run root")
}

// removeKeptDirs removes this run's root, lets go of the shared builds it
// used, and reclaims whatever that leaves unowned.
func removeKeptDirs() {
	runDirMu.Lock()
	defer runDirMu.Unlock()
	if runDir != "" {
		_ = os.RemoveAll(runDir)
		_ = runLock.Close()
		runDir, runLock = "", nil
	}
	for _, f := range sharedLocks {
		_ = f.Close()
	}
	sharedLocks = nil
	reclaimDirs(tempBase)
}

// reclaimDirs removes every run root and shared build under base that no live
// process holds. A directory it can't lock is in use, or another user's.
func reclaimDirs(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !(strings.HasPrefix(e.Name(), runRootPrefix) || strings.HasPrefix(e.Name(), sharedBuildPrefix)) {
			continue
		}
		path := filepath.Join(base, e.Name())
		lock, err := lockDir(path, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			continue
		}
		// The marker goes first, so a removal that stops part way can't
		// leave a build that still claims to be complete.
		_ = os.Remove(filepath.Join(path, sharedReadyFile))
		_ = os.RemoveAll(path)
		_ = lock.Close()
	}
}

// sharedBuild is the directory holding build's output for this source tree
// and the given options (the tags and flags that change the result). The
// first process to ask runs build; the rest wait for it and reuse the output,
// which they must treat as read-only. Without a key for the tree (no git, say)
// it builds into a directory of this run's own.
func sharedBuild(name string, options []string, build func(dir string) error) (string, error) {
	key, err := buildKey(name, options)
	if err != nil {
		return privateBuild(name, build)
	}
	return sharedBuildAt(tempBase, name, key, func() (string, error) { return buildKey(name, options) }, build)
}

func privateBuild(name string, build func(dir string) error) (string, error) {
	dir, err := keptDir("oc-" + name + "-")
	if err != nil {
		return "", err
	}
	if err = build(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// sharedBuildAt is sharedBuild under base. rekey is the key as it stands
// after the build: if the tree changed while it ran, the output doesn't
// match its key, so it is not published and the run builds its own.
func sharedBuildAt(base, name, key string, rekey func() (string, error), build func(dir string) error) (string, error) {
	path := filepath.Join(base, sharedBuildPrefix+name+"-"+key)
	for {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		// Shared first: it waits out a builder, and never waits on the live
		// runs using a finished build, as an exclusive request would until
		// they all exit.
		lock, err := lockDir(path, syscall.LOCK_SH)
		if errors.Is(err, errDirGone) {
			continue
		}
		if errors.Is(err, errDirNotOurs) {
			return privateBuild(name, build)
		}
		if err != nil {
			return "", err
		}
		if buildReady(path) {
			holdSharedBuild(lock)
			return path, nil
		}
		_ = lock.Close()
		// Unfinished, so nobody is using it. Losing this race means another
		// process is building, which the shared lock above then waits for.
		lock, err = lockDir(path, syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, errDirGone) || errors.Is(err, syscall.EWOULDBLOCK) {
			continue
		}
		if errors.Is(err, errDirNotOurs) {
			return privateBuild(name, build)
		}
		if err != nil {
			return "", err
		}
		if !buildReady(path) {
			// Empty, or what a killed builder left.
			if err = clearDir(path); err == nil {
				err = build(path)
			}
			if err != nil {
				_ = clearDir(path)
				_ = lock.Close()
				return "", err
			}
			if now, err := rekey(); err != nil || now != key {
				_ = clearDir(path)
				_ = lock.Close()
				return privateBuild(name, build)
			}
			if err = publishBuild(path); err != nil {
				_ = clearDir(path)
				_ = lock.Close()
				return "", err
			}
		}
		// Converting a flock isn't atomic: a reclaim can win in between, so
		// the build is only ours once it's still there under the shared lock.
		if err = flock(lock, syscall.LOCK_SH); err != nil {
			_ = lock.Close()
			return "", err
		}
		if !sameDir(lock, path) || !buildReady(path) {
			_ = lock.Close()
			continue
		}
		holdSharedBuild(lock)
		return path, nil
	}
}

// holdSharedBuild keeps lock for the rest of the run.
func holdSharedBuild(lock *os.File) {
	runDirMu.Lock()
	defer runDirMu.Unlock()
	sharedLocks = append(sharedLocks, lock)
}

func buildReady(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, sharedReadyFile))
	return err == nil
}

// publishBuild marks dir complete, in one step.
func publishBuild(dir string) error {
	f, err := os.CreateTemp(dir, ".ready-*")
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, sharedReadyFile))
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// buildKey names one build's output: the checkout (paths are compiled in),
// the content of every file git would see, the toolchain and its environment,
// and the options.
func buildKey(name string, options []string) (string, error) {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(top))
	files := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	files.Dir = root
	listed, err := files.Output()
	if err != nil {
		return "", err
	}
	toolchain, err := exec.Command("go", "env", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOEXPERIMENT", "GOAMD64", "GOARM64", "CC", "CGO_CFLAGS", "CGO_LDFLAGS").Output()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%q\x00%s\x00%s\x00", name, root, options, runtime.Version(), toolchain)
	for _, rel := range bytes.Split(listed, []byte{0}) {
		if len(rel) == 0 {
			continue
		}
		path := filepath.Join(root, string(rel))
		fmt.Fprintf(h, "%s\x00", rel)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprint(h, "absent\x00")
		case err != nil:
			return "", err
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "link\x00%s\x00", target)
		case info.IsDir():
			// A submodule: its commit is in the index.
			fmt.Fprint(h, "dir\x00")
		default:
			raw, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%o\x00%d\x00", info.Mode().Perm(), len(raw))
			h.Write(raw)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}
