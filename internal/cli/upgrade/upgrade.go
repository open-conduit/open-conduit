// Package upgrade finds the latest Open Conduit release and installs it in
// place of the running binaries: `open-conduit upgrade` and the web view's
// update page share it. It reads the release from GitHub, verifies the
// archive against the release's checksums.txt, as install.sh does, and
// swaps open-conduit and os beside the running executable with a rename
// each, so a failure part way leaves the old binaries working. A Homebrew
// install is left to brew, which owns its files.
package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "open-conduit/open-conduit"

// BrewCommand upgrades a Homebrew install.
const BrewCommand = "brew upgrade --cask open-conduit"

// Release is a published release.
type Release struct {
	// Version is the tag without its leading v ("1.9.0"), the form the
	// binaries report.
	Version string
	// URL is the release's page.
	URL string
}

// ErrHomebrew reports that the running binary belongs to a Homebrew cask,
// which brew must upgrade.
var ErrHomebrew = errors.New("this open-conduit was installed with Homebrew: run " + BrewCommand)

// Updater checks for and installs releases.
type Updater struct {
	// Executable is the running open-conduit, links resolved.
	Executable string
	// API and Download are the GitHub API and release download bases
	// (https://api.github.com and https://github.com when empty).
	API, Download string
	// GOOS and GOARCH pick the archive (this build's when empty).
	GOOS, GOARCH string
	// HTTP makes the requests (a client with a timeout when nil).
	HTTP *http.Client
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// TTL is how long a check is reused (six hours when zero).
	TTL time.Duration

	mu       sync.Mutex
	latest   Release
	checked  time.Time
	checking bool
}

// New is an Updater for the running executable.
func New() *Updater {
	self, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
	}
	return &Updater{Executable: self}
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (u *Updater) api() string {
	if u.API != "" {
		return strings.TrimSuffix(u.API, "/")
	}
	return "https://api.github.com"
}

func (u *Updater) download() string {
	if u.Download != "" {
		return strings.TrimSuffix(u.Download, "/")
	}
	return "https://github.com"
}

// Homebrew says whether the running binary is a Homebrew cask's.
func (u *Updater) Homebrew() bool {
	return strings.Contains(filepath.ToSlash(u.Executable), "/Caskroom/")
}

// Latest asks GitHub for the latest release, and remembers it for Cached.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.api()+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := u.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking for a release: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("checking for a release: GitHub answered %s", res.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("checking for a release: %w", err)
	}
	if _, ok := parse(body.Tag); !ok {
		return Release{}, fmt.Errorf("checking for a release: unexpected tag %q", body.Tag)
	}
	r := Release{Version: strings.TrimPrefix(body.Tag, "v"), URL: body.URL}
	u.mu.Lock()
	u.latest, u.checked = r, u.now()
	u.mu.Unlock()
	return r, nil
}

// Cached is the last release Latest found, without waiting on the network.
// When that check is older than the TTL (or there was none) it starts
// another in the background, so a page that asks never waits on GitHub.
func (u *Updater) Cached() (Release, bool) {
	ttl := u.TTL
	if ttl == 0 {
		ttl = 6 * time.Hour
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.checking && (u.checked.IsZero() || u.now().Sub(u.checked) >= ttl) {
		u.checking = true
		// A failed check waits a TTL too, rather than asking on every page.
		u.checked = u.now()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = u.Latest(ctx)
			u.mu.Lock()
			u.checking = false
			u.mu.Unlock()
		}()
	}
	return u.latest, u.latest.Version != ""
}

// Newer says whether latest is a later release than current. A current
// version that isn't a release number (a dev build) is never behind.
func Newer(current, latest string) bool {
	c, ok := parse(current)
	if !ok {
		return false
	}
	l, ok := parse(latest)
	if !ok {
		return false
	}
	for i := range 3 {
		if l.core[i] != c.core[i] {
			return l.core[i] > c.core[i]
		}
	}
	// 1.9.0 is later than 1.9.0-rc.1; a snapshot of 1.9.1 is not behind 1.9.0.
	return c.pre != "" && l.pre == ""
}

type semver struct {
	core [3]int
	pre  string
}

func parse(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var s semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		s.core[i] = n
	}
	s.pre = pre
	return s, true
}

// binaries are what an archive must hold, installed side by side.
var binaries = []string{"open-conduit", "os"}

// Install downloads version's archive for this platform, verifies it
// against the release's checksums and puts its open-conduit and os in
// place of the running ones. It never touches a Homebrew install.
func (u *Updater) Install(ctx context.Context, version string) error {
	if u.Homebrew() {
		return ErrHomebrew
	}
	if u.Executable == "" {
		return errors.New("can't tell where the running open-conduit is installed")
	}
	if _, ok := parse(version); !ok {
		return fmt.Errorf("%q is not a release version", version)
	}
	version = strings.TrimPrefix(version, "v")
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = goruntime.GOOS
	}
	if goarch == "" {
		goarch = goruntime.GOARCH
	}
	archive := fmt.Sprintf("open-conduit_%s_%s_%s.tar.gz", version, goos, goarch)
	base := u.download() + "/" + Repo + "/releases/download/v" + version
	dir := filepath.Dir(u.Executable)

	sums, err := u.fetch(ctx, base+"/checksums.txt", 1<<20)
	if err != nil {
		return err
	}
	want := checksum(sums, archive)
	if want == "" {
		return fmt.Errorf("the v%s release lists no checksum for %s", version, archive)
	}
	data, err := u.fetch(ctx, base+"/"+archive, 512<<20)
	if err != nil {
		return err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s: nothing was installed", archive)
	}
	// Stage both beside the executable, so each swap is a rename on one
	// filesystem, and swap only once both are staged.
	staged, err := extract(data, dir)
	defer func() {
		for _, p := range staged {
			_ = os.Remove(p)
		}
	}()
	if err != nil {
		return err
	}
	for _, name := range binaries {
		if err := os.Rename(staged[name], filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("installing %s: %w", name, err)
		}
		delete(staged, name)
	}
	return nil
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", filepath.Base(url), err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", filepath.Base(url), res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", filepath.Base(url), err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("downloading %s: larger than expected", filepath.Base(url))
	}
	return data, nil
}

// checksum is archive's SHA-256 in a checksums.txt, or "".
func checksum(sums []byte, archive string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == archive {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// extract writes the archive's binaries into dir under temporary names and
// returns them by name. It returns what it wrote even on error, for the
// caller to remove.
func extract(data []byte, dir string) (map[string]string, error) {
	staged := map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return staged, fmt.Errorf("reading the archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return staged, fmt.Errorf("reading the archive: %w", err)
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || strings.Contains(name, "/") || !wanted(name) {
			continue
		}
		f, err := os.CreateTemp(dir, "."+name+".upgrade-*")
		if err != nil {
			return staged, fmt.Errorf("staging %s: %w", name, err)
		}
		staged[name] = f.Name()
		_, err = io.Copy(f, io.LimitReader(tr, 512<<20))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.Name(), 0o755)
		}
		if err != nil {
			return staged, fmt.Errorf("staging %s: %w", name, err)
		}
	}
	for _, name := range binaries {
		if staged[name] == "" {
			return staged, fmt.Errorf("the archive has no %s", name)
		}
	}
	return staged, nil
}

func wanted(name string) bool {
	for _, b := range binaries {
		if name == b {
			return true
		}
	}
	return false
}
