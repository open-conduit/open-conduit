package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var credentialURL = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// redact strips URL userinfo (tokens embedded in remote URLs) from git output.
func redact(s string) string { return credentialURL.ReplaceAllString(s, "${1}[redacted]@") }

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// gitEnv is the environment of the provider's own git commands: the daemon's
// environment without toolbelt variables, never prompting for credentials and
// with stable messages for parsing.
func (p *Provider) gitEnv() []string {
	var env []string
	for _, kv := range p.o.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "OS_") || name == "LC_ALL" || name == "GIT_TERMINAL_PROMPT" || name == "GIT_DIR" || name == "GIT_WORK_TREE" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// git runs one git command in dir. Errors carry the redacted first line of
// output, never a configured credential.
func (p *Provider) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.o.Git, append([]string{"-C", dir}, args...)...)
	cmd.Env = p.gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := redact(strings.TrimSpace(string(out)))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *Provider) refExists(ctx context.Context, dir, ref string) bool {
	_, err := p.git(ctx, dir, "show-ref", "--verify", "-q", ref)
	return err == nil
}

func (p *Provider) hasRemote(ctx context.Context, dir, remote string) bool {
	_, err := p.git(ctx, dir, "remote", "get-url", remote)
	return err == nil
}

// toplevel is the canonical work-tree root of dir, or an error when dir is not
// the root of a git work tree.
func (p *Provider) toplevel(ctx context.Context, dir string) (string, error) {
	top, err := p.git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if got, err := filepath.EvalSymlinks(top); err != nil || got != want {
		return "", errors.New("not the root of a git work tree")
	}
	return want, nil
}

// validWorktree reports whether path is a work tree of the checkout's
// repository (not some other repository that happens to live there).
func (p *Provider) validWorktree(ctx context.Context, checkout, path string) bool {
	if _, err := p.toplevel(ctx, path); err != nil {
		return false
	}
	a, err1 := p.git(ctx, path, "rev-parse", "--git-common-dir")
	b, err2 := p.git(ctx, checkout, "rev-parse", "--git-common-dir")
	if err1 != nil || err2 != nil {
		return false
	}
	abs := func(dir, d string) string {
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return r
		}
		return filepath.Clean(d)
	}
	return abs(path, a) == abs(checkout, b)
}

// base is the configured base branch, else the remote's HEAD, else main.
func (p *Provider) base(ctx context.Context, cfg Config, checkout string) string {
	if cfg.BaseBranch != "" {
		return cfg.BaseBranch
	}
	if ref, err := p.git(ctx, checkout, "symbolic-ref", "-q", "--short", "refs/remotes/"+cfg.Remote+"/HEAD"); err == nil {
		if b := strings.TrimPrefix(ref, cfg.Remote+"/"); b != ref && validRef(b) {
			return b
		}
	}
	if p.hasRemote(ctx, checkout, cfg.Remote) {
		if out, err := p.git(ctx, checkout, "ls-remote", "--symref", cfg.Remote, "HEAD"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
					if b, _, _ := strings.Cut(rest, "\t"); validRef(b) {
						return b
					}
				}
			}
		}
	}
	return "main"
}

// Sync is how a shared checkout stood against the base branch's remote head
// when a run started in it.
type Sync struct {
	// Updated is how many commits it was fast-forwarded by.
	Updated int `json:"updated,omitempty"`
	// Behind is how many commits of the remote head it still lacks, and Stale
	// why ("on branch x", "uncommitted changes ...", "couldn't fetch ...").
	Behind int    `json:"behind,omitempty"`
	Stale  string `json:"stale,omitempty"`
}

// behind fetches the base branch and counts the remote head's commits the
// checkout lacks, changing nothing else. Without the remote there is nothing
// to compare with.
func (p *Provider) behind(ctx context.Context, checkout, remote, base string) Sync {
	var got Sync
	if !p.hasRemote(ctx, checkout, remote) {
		return got
	}
	if _, err := p.git(ctx, checkout, "fetch", "-q", remote, base); err != nil {
		got.Stale = "couldn't fetch: " + firstLine(err.Error())
	}
	head := remote + "/" + base
	if !p.refExists(ctx, checkout, "refs/remotes/"+head) {
		return got
	}
	if count, err := p.git(ctx, checkout, "rev-list", "--count", "HEAD.."+head); err == nil {
		got.Behind, _ = strconv.Atoi(count)
	}
	return got
}

// catchUp fetches the base branch and fast-forwards the checkout when it is on
// that branch. It never resets, stashes, merges or rebases: uncommitted edits
// stay, and git refuses (reported as stale) when the update would touch one.
func (p *Provider) catchUp(ctx context.Context, checkout, remote, base string) Sync {
	got := p.behind(ctx, checkout, remote, base)
	if got.Behind == 0 || got.Stale != "" {
		return got
	}
	head := remote + "/" + base
	branch, _ := p.git(ctx, checkout, "symbolic-ref", "-q", "--short", "HEAD")
	if branch != base {
		if branch == "" {
			branch = "a detached HEAD"
		}
		got.Stale = "on " + branch + ", not " + base
		return got
	}
	if _, err := p.git(ctx, checkout, "merge", "-q", "--ff-only", head); err != nil {
		switch msg := err.Error(); {
		case strings.Contains(msg, "would be overwritten"):
			got.Stale = "uncommitted changes to files the update touches"
		case strings.Contains(msg, "fast-forward") || strings.Contains(msg, "diverg"):
			got.Stale = base + " has commits " + head + " doesn't"
		default:
			got.Stale = "couldn't fast-forward: " + firstLine(msg)
		}
		return got
	}
	got.Updated, got.Behind = got.Behind, 0
	return got
}

// dirty counts uncommitted paths, so a reused workspace explains local edits.
// Files the provider copies in (such as .env) are not the run's edits.
func (p *Provider) dirty(ctx context.Context, dir string, copied []string) int {
	args := []string{"status", "--porcelain", "--", "."}
	for _, name := range copied {
		args = append(args, ":(exclude,literal)"+name)
	}
	out, err := p.git(ctx, dir, args...)
	if err != nil || out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func (p *Provider) head(ctx context.Context, dir string) string {
	h, _ := p.git(ctx, dir, "rev-parse", "--short", "HEAD")
	return h
}

// gitDir is a work tree's own git directory, where the setup stamp lives out
// of the work tree.
func (p *Provider) gitDir(ctx context.Context, path string) string {
	if d, err := p.git(ctx, path, "rev-parse", "--absolute-git-dir"); err == nil {
		return d
	}
	return filepath.Join(path, ".git")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
