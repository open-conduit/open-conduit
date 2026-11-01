package integration_test

// git as the GitHub bot App (the GitHub connection's git "bot") in an
// unsandboxed and a process-sandboxed claude-code run, through the GitHub
// extension's run environment layer: the agent's plain `git commit` and
// `git push` go out as the App's bot user, with a token the App minted for
// the Work's one repository, though the operator's own git config names a
// credential helper and an identity of its own. Without the option, git
// stays the operator's. GitHub is githubtest over TLS on loopback, its git
// served by git's own smart HTTP; no real service is called.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/extensions/srt"
	"github.com/open-conduit/open-conduit/internal/management"
)

// gitBotClaude stands in for claude: it commits a file and pushes it to
// $LAB_REMOTE as an agent would, with nothing but git's configuration, and
// writes git's output and the push's status to ./git-out. The push leaves
// out NO_PROXY: this GitHub is on loopback, which srt keeps out of its
// proxy and its sandbox refuses directly, so the push goes through the
// proxy as one to github.com would.
const gitBotClaude = `#!/bin/sh
read -r line
echo '{"type":"system","subtype":"init"}'
export GIT_TERMINAL_PROMPT=0 GIT_SSL_NO_VERIFY=1
{
  echo "$LAB_FILE" > "$LAB_FILE" && git add "$LAB_FILE" && git commit -qm "$LAB_FILE" && env -u NO_PROXY -u no_proxy git push "$LAB_REMOTE" "HEAD:refs/heads/$LAB_FILE"
  echo "push=$?"
} > ./git-out 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"review","summary":"pushed"}}'
`

// gitBotLab is GitHub (with a bot App, 43) over TLS, the GitHub tools on a
// token connection with that bot App and git "bot" (and on the same
// connection without it, as the owner's), each execution's toolbelt
// serving github.git.credential, and an operator whose git config would
// push with a token GitHub refuses and commit as themselves.
type gitBotLab struct {
	t                  *testing.T
	root, bare, bin    string
	gitconfig, remote  string
	fake               *githubtest.Server
	tools, ownerTools  *gh.Tools
	endpoints          *management.Endpoints
	work               domain.Work
	botLogin, botEmail string
	// reviewerLogin and reviewerEmail are the bot user of App 44, the
	// github-reviewer connection a binding may name, with git "bot".
	reviewerLogin, reviewerEmail string
	connections                  sync.Map // execution → the connection its binding names
	ownerName, ownerMail         string
}

func newGitBotLab(t *testing.T, root string) *gitBotLab {
	t.Helper()
	l := &gitBotLab{t: t, root: root, bare: filepath.Join(root, "github"), bin: filepath.Join(root, "bin"), gitconfig: filepath.Join(root, "gitconfig"),
		ownerName: "Owner", ownerMail: "owner@example.com"}
	for _, d := range []string{l.bare, l.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	if out, err := exec.Command(gitPath, "init", "-q", "--bare", filepath.Join(l.bare, "octo", "widgets.git")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	self := builtToolbelt
	if self == "" {
		if self, err = os.Executable(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(l.bin, "os")); err != nil {
		t.Fatal(err)
	}
	// The operator's git: a helper with a token GitHub refuses, which git
	// would ask first unless the run clears it, and their own identity.
	if err := os.WriteFile(l.gitconfig, []byte(fmt.Sprintf("[credential]\n\thelper = \"!f() { echo username=owner; echo password=owner-token; }; f\"\n[user]\n\tname = %s\n\temail = %s\n", l.ownerName, l.ownerMail)), 0o644); err != nil {
		t.Fatal(err)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	l.fake = githubtest.New("octo/widgets", "pat")
	t.Cleanup(l.fake.Close)
	l.fake.AddApp("43", &key.PublicKey)
	l.fake.AddApp("44", &key.PublicKey)
	_, login, id := githubtest.AppBot("43")
	api, gitHTTP := l.fake.Handler(), l.fake.GitHandler(l.bare, gitPath)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v3"); ok {
			r = r.Clone(r.Context())
			r.URL.Path = rest
			api.ServeHTTP(w, r)
			return
		}
		gitHTTP.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	l.remote = srv.URL + "/octo/widgets.git"
	l.botLogin, l.botEmail = login, fmt.Sprintf("%d+%s@users.noreply.%s", id, login, host)
	_, rlogin, rid := githubtest.AppBot("44")
	l.reviewerLogin, l.reviewerEmail = rlogin, fmt.Sprintf("%d+%s@users.noreply.%s", rid, rlogin, host)

	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	const ns = "github/github.com/octo/widgets"
	settings, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"scope","repo":"octo/widgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	l.work = domain.Work{ID: "work", ScopeID: "scope", CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: gh.IssueKey(3, "")}}}
	secrets := func(_ context.Context, name string) (string, error) {
		if name == gh.SecretToken {
			return "pat", nil
		}
		return keyPEM, nil
	}
	// A binding may name github-reviewer, App 44 with git "bot".
	writers := gh.NewWriters(func(scope domain.ID, id string) (json.RawMessage, gh.SecretFunc, bool) {
		if scope != "scope" || id != "github-reviewer" {
			return nil, nil, false
		}
		return json.RawMessage(`{"api_url":"` + srv.URL + `/api/v3","auth":"app","app_id":"44","git":"bot"}`), secrets, true
	}, l.fake.Now)
	tools := func(git string) *gh.Tools {
		conn, err := gh.ParseConnection([]byte(`{"api_url":"` + srv.URL + `/api/v3","auth":"token","bot_app_id":"43","git":"` + git + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		client, err := gh.NewClient(conn, secrets, gh.ClientOptions{HTTP: srv.Client(), Now: l.fake.Now})
		if err != nil {
			t.Fatal(err)
		}
		out, err := gh.NewTools(gh.ToolsOptions{Store: gitStore{l.work}, Sources: func() []gh.Source { return []gh.Source{{Settings: settings, Connection: "github", Client: client}} }, Now: l.fake.Now, Writers: writers})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	l.tools, l.ownerTools = tools(gh.GitBot), tools(gh.GitOwner)
	ops := map[string]extension.Operation{}
	for _, op := range l.tools.Registration().Operations {
		if op.Name == gh.OpGitCredential {
			ops[op.Name] = op
		}
	}
	// Each execution's toolbelt, as the daemon serves it, with the git
	// operation a binding in a git "bot" scope is granted.
	l.endpoints, err = management.NewEndpoints(nil, filepath.Join(root, "state"), filepath.Join(root, "s"), func(exec domain.ID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The input is a payload, as the management handler decodes it.
			var call struct {
				Operation string
				Input     struct {
					Schema string          `json:"schema"`
					Data   json.RawMessage `json:"data"`
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&call)
			w.Header().Set("Content-Type", "application/json")
			op, ok := ops[call.Operation]
			if !ok || r.Header.Get("Authorization") != "Bearer belt-"+string(exec) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"execution authority rejected"}}`)
				return
			}
			conn, _ := l.connections.Load(exec)
			connection, _ := conn.(string)
			out, err := op.Handler(r.Context(), extension.Authority{ScopeID: "scope", WorkID: "work", ExecutionID: exec, Connection: connection}, domain.Payload{Schema: call.Input.Schema, Data: call.Input.Data})
			var refused *extension.OperationError
			if errors.As(err, &refused) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": refused.Code, "message": refused.Message}})
				return
			} else if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"error":{"code":"unavailable","message":"failed"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"output": map[string]any{"schema": out.Schema, "data": out.Data}}})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.endpoints.Shutdown(context.Background()) })
	return l
}

// workspace is a fresh clone-like repository with one commit.
func (l *gitBotLab) workspace(dir string) {
	l.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			l.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o700)
}

// pushed is the author and committer of the branch's head on GitHub, or ""
// when nothing reached it.
func (l *gitBotLab) pushed(branch string) string {
	out, err := exec.Command("git", "--git-dir", filepath.Join(l.bare, "octo", "widgets.git"), "log", "-1", "--format=%an <%ae> / %cn <%ce>", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// committed is the author and committer of the workspace's head.
func committed(ws string) string {
	out, _ := exec.Command("git", "-C", ws, "log", "-1", "--format=%an <%ae> / %cn <%ce>").Output()
	return strings.TrimSpace(string(out))
}

// run is one claude-code turn, in the sandbox when b is set, with the run
// environment layer the GitHub extension gives it, as the daemon gathers
// it: as the bot when bot is set, else on the owner's connection.
func (l *gitBotLab) run(t *testing.T, e *claudecode.Executor, exec domain.ID, bot bool, ws, branch string, env map[string]string, sandbox *extension.BoundSandbox) extension.Result {
	t.Helper()
	return l.runAs(t, e, exec, bot, "", ws, branch, env, sandbox)
}

// runAs is run for a binding naming connection (none when ""), which the
// lab's connections all set git "bot".
func (l *gitBotLab) runAs(t *testing.T, e *claudecode.Executor, exec domain.ID, bot bool, connection, ws, branch string, env map[string]string, sandbox *extension.BoundSandbox) extension.Result {
	t.Helper()
	l.connections.Store(exec, connection)
	token := "belt-" + string(exec)
	endpoint, tokenFile, err := l.endpoints.Serve(context.Background(), exec, token)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"LAB_REMOTE": l.remote, "LAB_FILE": branch, "GIT_CONFIG_GLOBAL": l.gitconfig, "GIT_CONFIG_NOSYSTEM": "1"}
	for k, v := range env {
		vars[k] = v
	}
	c := map[string]any{"binary": filepath.Join(l.bin, "claude"), "playbook": "-", "env": vars}
	cfg, _ := json.Marshal(c)
	tools := l.ownerTools
	if bot {
		tools = l.tools
	}
	layer, err := tools.RunEnvironment(context.Background(), extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: l.work.ScopeID, WorkID: l.work.ID, ExecutionID: exec}, Connection: connection})
	if err != nil {
		t.Fatal(err)
	}
	// A bot run gets the bot's identity; any other only the static token
	// helper for the web host, which says nothing without a token.
	if want := bot || connection != ""; want != (layer["GIT_AUTHOR_NAME"] != "") || !want && (layer["GIT_CONFIG_COUNT"] != "1" || layer["GIT_CONFIG_VALUE_0"] != gh.TokenHelper) {
		t.Fatalf("bot %v, connection %q, run environment %v", bot, connection, layer)
	}
	ctx := extension.ExecutionContext{Config: domain.Payload{Data: cfg}, ToolbeltEndpoint: endpoint, ToolbeltToken: token, ToolbeltTokenFile: tokenFile,
		Resources: plainWorkspace{ws}, Env: layer}
	if sandbox != nil {
		ctx.Sandbox, ctx.Resources = sandbox, sandbox.Resources(labWorkspace{ws, true})
	}
	h, err := e.Run(context.Background(), extension.RunRequest{Work: l.work, Execution: domain.Execution{ID: exec, WorkID: l.work.ID, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Trigger: domain.Event{ID: domain.ID("ev-" + exec), Type: "note"}, Context: ctx, PersistRecovery: func(domain.Payload) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, h)
	if err := l.tools.RevokeGitTokens(context.Background(), exec); err != nil {
		t.Fatalf("revoking %s's git tokens: %v", exec, err)
	}
	return res
}

func (l *gitBotLab) claude(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.bin, "claude"), []byte(gitBotClaude), 0o755); err != nil {
		t.Fatal(err)
	}
}

// plainWorkspace is an unsandboxed binding's resource: the directory.
type plainWorkspace struct{ dir string }

func (w plainWorkspace) Acquire(context.Context, extension.ResourceRequest) (extension.Resource, error) {
	return extension.Resource{Kind: "workspace", Ref: "lease", Dir: w.dir}, nil
}
func (w plainWorkspace) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return w.Acquire(ctx, extension.ResourceRequest{})
}

// TestGitBotUnsandboxed: an unsandboxed claude-code run of Work whose
// GitHub connection sets git "bot" commits as the bot App's bot user and pushes with a token the
// App minted for the Work's repository, though the operator's git config
// has a helper and an identity of its own; the token is revoked once the
// run ends. The same run on a connection without the option commits as the operator
// and pushes with the operator's helper, as before.
func TestGitBotUnsandboxed(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ocgb")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	l := newGitBotLab(t, root)
	l.claude(t)
	home := filepath.Join(root, "home")
	_ = os.MkdirAll(home, 0o700)
	e, err := claudecode.New(claudecode.Options{StateDir: filepath.Join(root, "executor"), Store: reference.New(), ToolbeltDir: l.bin,
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home} }, Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	bot := fmt.Sprintf("%s <%s> / %s <%s>", l.botLogin, l.botEmail, l.botLogin, l.botEmail)
	owner := fmt.Sprintf("%s <%s> / %s <%s>", l.ownerName, l.ownerMail, l.ownerName, l.ownerMail)

	ws := filepath.Join(root, "ws-bot")
	l.workspace(ws)
	res := l.run(t, e, "bot-1", true, ws, "as-bot", nil, nil)
	out, _ := os.ReadFile(filepath.Join(ws, "git-out"))
	if res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("outcome %q\n%s", res.Outcome.Summary, out)
	}
	if got := committed(ws); got != bot {
		t.Errorf("committed as %q, want %q\n%s", got, bot, out)
	}
	if got := l.pushed("as-bot"); got != bot {
		t.Errorf("GitHub has %q on as-bot, want %q\n%s", got, bot, out)
	}
	if issued := l.fake.InstallationTokensIssued(); issued < 1 {
		t.Errorf("the App minted %d tokens", issued)
	}

	// The default: the operator's helper (whose token GitHub refuses) and
	// identity, untouched.
	ws = filepath.Join(root, "ws-owner")
	l.workspace(ws)
	res = l.run(t, e, "owner-1", false, ws, "as-owner", nil, nil)
	out, _ = os.ReadFile(filepath.Join(ws, "git-out"))
	if res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("outcome %q\n%s", res.Outcome.Summary, out)
	}
	if got := committed(ws); got != owner {
		t.Errorf("without git bot, committed as %q, want %q", got, owner)
	}
	if got := l.pushed("as-owner"); got != "" || !strings.Contains(string(out), "push=1") {
		t.Errorf("without git bot, the push went out (%q) rather than with the operator's refused token:\n%s", got, out)
	}

	// In the same scope, a binding naming github-reviewer (App 44, git
	// "bot") commits and pushes as that App, though the entry's connection
	// leaves git the operator's.
	reviewer := fmt.Sprintf("%s <%s> / %s <%s>", l.reviewerLogin, l.reviewerEmail, l.reviewerLogin, l.reviewerEmail)
	ws = filepath.Join(root, "ws-reviewer")
	l.workspace(ws)
	res = l.runAs(t, e, "reviewer-1", false, "github-reviewer", ws, "as-reviewer", nil, nil)
	out, _ = os.ReadFile(filepath.Join(ws, "git-out"))
	if res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("outcome %q\n%s", res.Outcome.Summary, out)
	}
	if got := committed(ws); got != reviewer {
		t.Errorf("binding naming github-reviewer committed as %q, want %q\n%s", got, reviewer, out)
	}
	if got := l.pushed("as-reviewer"); got != reviewer {
		t.Errorf("GitHub has %q on as-reviewer, want %q\n%s", got, reviewer, out)
	}
}

// TestSandboxGitBot: the same in a process sandbox. git's helper is the
// toolbelt's os, reaching the run's own toolbelt socket through the
// sandbox.
func TestSandboxGitBot(t *testing.T) {
	sl := newSandboxLab(t, "claude-code")
	l := newGitBotLab(t, filepath.Join(sl.root, "gb"))
	l.claude(t)
	e, err := claudecode.New(claudecode.Options{StateDir: sl.execState, Store: reference.New(), ToolbeltDir: l.bin,
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + sl.home} }, Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const exec domain.ID = "bot-sb"
	endpoint, tokenFile, err := l.endpoints.Serve(context.Background(), exec, "belt-"+string(exec))
	if err != nil {
		t.Fatal(err)
	}
	socket := strings.TrimPrefix(endpoint, "unix://")
	policy := `{"write":["{workspace}","~/.claude-sandboxed"],"network":{"allow":["api.anthropic.com","127.0.0.1","localhost"]},"sockets":["{toolbelt}"],"unix_sockets":"all"}`
	b := extension.BindSandbox(extension.SandboxBinding{
		Provider: srt.New(srt.Options{Binary: sl.srt}), Owner: extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: exec},
		Config: domain.Payload{Schema: "core.config.sandbox.v1", Data: json.RawMessage(policy)}, Protected: []string{sl.state, sl.config},
		Shared: []string{filepath.Dir(tokenFile)}, RunRoot: filepath.Join(sl.execState, "runs"), Toolbelt: socket,
		Records: filepath.Join(l.root, "sandbox"), Pin: func(extension.SandboxPlan) error { return nil }})
	ws := sl.ws
	res := l.run(t, e, exec, true, ws, "as-bot", map[string]string{"CLAUDE_CONFIG_DIR": sl.claude}, b)
	out, _ := os.ReadFile(filepath.Join(ws, "git-out"))
	if res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("outcome %q\n%s", res.Outcome.Summary, out)
	}
	bot := fmt.Sprintf("%s <%s> / %s <%s>", l.botLogin, l.botEmail, l.botLogin, l.botEmail)
	if got := committed(ws); got != bot {
		t.Errorf("committed as %q, want %q\n%s", got, bot, out)
	}
	if got := l.pushed("as-bot"); got != bot {
		t.Errorf("GitHub has %q on as-bot, want %q\n%s", got, bot, out)
	}
}
