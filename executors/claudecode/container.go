package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// credentialEnv are the variables a container launch passes as secrets
// whatever layer set them: claude's sign-in and a GitHub token.
var credentialEnv = map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "GH_TOKEN": true, "GITHUB_TOKEN": true}

// onboarded is the minimal .claude.json a new home gets, so claude starts
// without its first-run onboarding. It holds no credentials.
const onboarded = `{"hasCompletedOnboarding": true}` + "\n"

// containerPath is a container's PATH unless the binding's env sets one:
// the daemon's own names host directories.
const containerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// containerHome points a container launch's claude at the Work's home
// (HOME and CLAUDE_CONFIG_DIR) and a Linux PATH, seeds the home on first use, turns off
// claude's nonessential traffic (the gateway would refuse its telemetry)
// and moves every credential out of env into the secrets the provider
// keeps in a file: claude's sign-in, a GitHub token (which git uses for
// github.com through a credential helper) and each selected secret.
func (e *Executor) containerHome(cfg Config, home string, env []string, selected map[string]string) ([]string, []string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, nil, fmt.Errorf("%w: the host gave the container no home for the Work", extension.ErrSandboxUnavailable)
	}
	if err := seedHome(home, cfg.HomeSeed); err != nil {
		return nil, nil, fmt.Errorf("%w: could not seed the Work's home: %v", extension.ErrSandboxUnavailable, err)
	}
	secret := map[string]bool{}
	for name := range selected {
		secret[cfg.secretVariable(name)] = true
	}
	var out, secrets []string
	github := false
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME" || k == "CLAUDE_CONFIG_DIR" || k == "TMPDIR" || k == "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" || k == "PATH" && cfg.Env["PATH"] == "":
		case secret[k] || credentialEnv[k]:
			secrets = append(secrets, kv)
			github = github || k == "GH_TOKEN" || k == "GITHUB_TOKEN"
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "HOME="+home, "CLAUDE_CONFIG_DIR="+home, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	if cfg.Env["PATH"] == "" {
		out = append(out, "PATH="+containerPath)
	}
	if github && !gitCredentialHelper(out) {
		out = gitConfig(out, [2]string{"credential.https://github.com.helper",
			`!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=${GH_TOKEN:-$GITHUB_TOKEN}"; }; f`})
	}
	sort.Strings(out)
	sort.Strings(secrets)
	return out, secrets, nil
}

// toolbeltGitHelper makes the toolbelt git's credential helper in a
// container launch that has one and no other: `os git-credential` asks the
// host for a token for the Work's repository, which a GitHub App
// connection mints for that repository alone and revokes when the segment
// ends. It goes after the GIT_CONFIG entries env already sets (the
// workspace's signing policy); a credential helper already set there (a
// static GitHub token's, from containerHome, a binding's own or a git "bot"
// binding's) is left as it is.
func toolbeltGitHelper(env []string) []string {
	if gitCredentialHelper(env) {
		return env
	}
	out := gitConfig(env, [2]string{"credential.helper", "!os git-credential"})
	sort.Strings(out)
	return out
}

// seedHome fills an empty home with a copy of the seed directory (its
// .credentials.json left out) and the onboarding .claude.json, unless the
// seed brought one. A home with anything in it is left as it is: it is the
// agent's.
func seedHome(home, seed string) error {
	entries, err := os.ReadDir(home)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	if seed != "" {
		if err := copyTree(seed, home); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(home, ".claude.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = f.WriteString(onboarded)
	return errors.Join(err, f.Close())
}

// copyTree copies the regular files, directories and symlinks under src
// into dst, owner-only.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		if rel == ".credentials.json" {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.Mkdir(target, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0o700|0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// ContainerAttach implements extension.ContainerAttachExecutor: an
// interactive `claude --resume` on the Work's session, launched in a
// container with the Work's home and workspace, with the binding's sign-in
// and no toolbelt. Like Attach, it clears the handoff note.
func (e *Executor) ContainerAttach(ctx context.Context, req extension.ContainerAttachRequest) (extension.ContainerAttachSpec, error) {
	cfg, err := ParseConfig(req.Config.Data)
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	var a attachInfo
	var session string
	err = e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		var rec record
		found, err := getJSON(tx, e.o.Namespace, workKey(req.Work.ID), &rec)
		if err != nil {
			return err
		}
		if !found || rec.Session == "" || rec.Attach == nil || !rec.Attach.Container {
			return ErrNoSession
		}
		a, session = *rec.Attach, rec.Session
		rec.Handoff = ""
		return putJSON(tx, e.o.Namespace, workKey(req.Work.ID), rec)
	})
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	env, err := e.environment(cfg, extension.RunRequest{Work: req.Work, Context: extension.ExecutionContext{Secrets: req.Secrets}}, nil)
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	env, secrets, err := e.containerHome(cfg, req.Home, env, req.Secrets)
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	if !hasEnv(env, "TERM") {
		env = append(env, "TERM=xterm-256color")
	}
	// The attach's run directory is the executor's, one per Work, made
	// afresh each time.
	runs := filepath.Join(e.o.StateDir, "runs", hashKey("attach/"+string(req.Work.ID)))
	if err := os.RemoveAll(runs); err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	nonce, err := newNonce()
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	dir := filepath.Join(runs, nonce)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	argv := append([]string{a.Binary, "--resume", session, "--model", a.Model, "--permission-mode", a.PermissionMode}, a.ExtraArgs...)
	needs := extension.SandboxNeeds{Write: []string{a.Dir, req.Home}, Hosts: []string{"api.anthropic.com"}}
	if a.PluginDir != "" {
		argv = append(argv, "--plugin-dir", a.PluginDir)
		needs.Read = append(needs.Read, a.PluginDir)
	}
	return extension.ContainerAttachSpec{Needs: needs, Launch: extension.SandboxLaunch{Argv: argv, Dir: a.Dir, Env: env, RunDir: dir, Secrets: secrets, TTY: true}}, nil
}

var _ extension.ContainerAttachExecutor = (*Executor)(nil)
