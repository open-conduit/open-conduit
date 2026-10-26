package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// 06-02: explicit Open Supervisor conversion and the offline recovery
// commands. internal/cli reaches the converter and the state directory only
// through functions the open-conduit entrypoint injects (Env.Migrate,
// Env.State), so neither the os binary nor this package imports an
// extension, SQLite or the backup composition.

// MigrateRequest is one conversion of an Open Supervisor v1 config.
type MigrateRequest struct {
	Source []byte
	// RepoFiles reads each project checkout's .open-supervisor.yaml and
	// playbook additions.
	RepoFiles bool
	Home      string
	// TimeZone is the zone v1's local daily-run times were read in;
	// "" lets the converter detect the machine's.
	TimeZone               string
	ShellJobs              bool
	DetachedTimeoutSeconds int
}

// MigrateItem is one report line (see internal/migrate.Item).
type MigrateItem struct {
	Level   string `json:"level"`
	Path    string `json:"path"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

// MigrateSecret is one secret-reference mapping; it never carries a value or
// v1 reference text.
type MigrateSecret struct {
	From   string `json:"from"`
	Scheme string `json:"scheme"`
	To     string `json:"to"`
	Action string `json:"action,omitempty"`
}

// MigrateResult is a converted config and its report.
type MigrateResult struct {
	Config  json.RawMessage `json:"config"`
	Scopes  []string        `json:"scopes"`
	Items   []MigrateItem   `json:"items"`
	Secrets []MigrateSecret `json:"secrets"`
}

// BackupInfo describes a published or verified bundle.
type BackupInfo struct {
	Path      string    `json:"path"`
	Schema    uint32    `json:"schema"`
	Build     string    `json:"build"`
	CreatedAt time.Time `json:"created_at"`
	Artifacts int       `json:"artifacts"`
	Scopes    []string  `json:"scopes,omitempty"`
	// TokenFile is the fresh operator credential a restore wrote.
	TokenFile string `json:"token_file,omitempty"`
}

// StateCheck is one offline diagnosis line.
type StateCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ScopeState is a scope's nonterminal coordination state.
type ScopeState struct {
	ID               string         `json:"id"`
	Works            map[string]int `json:"works"`
	LiveExecutions   int            `json:"live_executions"`
	ActiveWaits      int            `json:"active_waits"`
	Drained          bool           `json:"drained"`
	DrainBlockedWork []string       `json:"drain_blocked_work,omitempty"`
}

// StateReport is `state check`'s offline diagnosis of a stopped instance.
type StateReport struct {
	StateDir string       `json:"state_dir"`
	OK       bool         `json:"ok"`
	Checks   []StateCheck `json:"checks"`
	Scopes   []ScopeState `json:"scopes,omitempty"`
}

// StateOps are the offline state-directory operations. Each takes the
// state directory's exclusive daemon lock, so it refuses to run while a
// daemon owns the directory.
type StateOps struct {
	Backup          func(ctx context.Context, stateDir, destination, build string) (BackupInfo, error)
	Verify          func(ctx context.Context, bundle string) (BackupInfo, error)
	Restore         func(ctx context.Context, bundle, destination string) (BackupInfo, error)
	Check           func(ctx context.Context, stateDir string) (StateReport, error)
	RecoverOperator func(ctx context.Context, stateDir, tokenPath string) error
}

func offlineCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Minute)
}

// migrate implements `migrate config`.
func (o *operator) migrate(args []string) int {
	if len(args) == 0 || args[0] != "config" {
		return usageError(o.errOut, "expected migrate config --from FILE")
	}
	fs := flag.NewFlagSet("migrate config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "")
	out := fs.String("out", "", "")
	write := fs.Bool("write", false, "")
	force := fs.Bool("force", false, "")
	noRepo := fs.Bool("no-repo-files", false, "")
	zone := fs.String("time-zone", "", "")
	shellJobs := fs.Bool("shell-jobs", false, "")
	detached := fs.Int("detached-timeout", 0, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *from == "" {
		return usageError(o.errOut, "expected migrate config --from FILE [--out FILE | --write] [--force] [--no-repo-files] [--time-zone ZONE] [--shell-jobs] [--detached-timeout SECONDS] [--json]")
	}
	if o.env.Migrate == nil {
		return usageError(o.errOut, "conversion is not available in this binary")
	}
	source := o.abs(*from)
	data, err := os.ReadFile(source)
	if err != nil {
		return o.emit(nil, fmt.Errorf("cannot read %s", source))
	}
	target := ""
	switch {
	case *out != "" && *write:
		return usageError(o.errOut, "give --out FILE or --write, not both")
	case *out != "":
		target = o.abs(*out)
	case *write:
		target = o.configPath("")
	}
	if target != "" {
		if err := refuseTarget(source, target); err != nil {
			return o.emit(nil, err)
		}
		if _, err := os.Lstat(target); err == nil && !*force {
			return o.emit(nil, fmt.Errorf("%s exists; conversion never replaces a config without --force (merge by hand, or convert to another file)", target))
		}
	}
	res, err := o.env.Migrate(MigrateRequest{Source: data, RepoFiles: !*noRepo, Home: o.env.Getenv("HOME"), TimeZone: *zone, ShellJobs: *shellJobs, DetachedTimeoutSeconds: *detached})
	if err != nil {
		return o.emit(nil, err)
	}
	var details []client.Detail
	if o.env.ValidateConfig != nil || o.env.ValidateConfigAt != nil {
		vpath := target
		if vpath == "" {
			vpath = o.configPath("")
		}
		if details, err = o.validateConfig(vpath, res.Config); err != nil {
			return o.emit(nil, fmt.Errorf("config could not be validated: %w", err))
		}
	}
	valid := len(details) == 0
	written := ""
	if target != "" && valid {
		if err := writePrivate(target, res.Config); err != nil {
			return o.emit(nil, fmt.Errorf("cannot write %s", target))
		}
		written = target
	}
	// Views show command reference text redacted, as config show does; the
	// written file keeps the references themselves.
	res.Config = redactCommands(res.Config)
	for i := range res.Secrets {
		if strings.HasPrefix(res.Secrets[i].To, "cmd:") {
			res.Secrets[i].To = "cmd:[redacted]"
		}
	}
	if *asJSON {
		if details == nil {
			details = []client.Detail{}
		}
		code := o.emit(map[string]any{"source": source, "valid": valid, "diagnostics": details, "written": written,
			"scopes": res.Scopes, "items": res.Items, "secrets": res.Secrets, "config": res.Config}, nil)
		if !valid || code != 0 {
			return 1
		}
		return 0
	}
	o.printMigration(source, res, details, written, target)
	if !valid {
		return 1
	}
	return 0
}

// redactCommands replaces cmd: secret reference text in a config document.
func redactCommands(doc json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return doc
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, child := range t {
				if refs, ok := child.(map[string]any); ok && k == "secret_refs" {
					for name, ref := range refs {
						if s, ok := ref.(string); ok && strings.HasPrefix(s, "cmd:") {
							refs[name] = "cmd:[redacted]"
						}
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return doc
	}
	return append(out, '\n')
}

// refuseTarget keeps conversion non-destructive: never the source itself and
// never Open Supervisor's own config directory.
func refuseTarget(source, target string) error {
	src, _ := filepath.Abs(source)
	dst, _ := filepath.Abs(target)
	if src == dst {
		return fmt.Errorf("refusing to write over the Open Supervisor config")
	}
	if filepath.Dir(src) == filepath.Dir(dst) || strings.Contains(dst, string(filepath.Separator)+"open-supervisor"+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write into Open Supervisor's config directory; Open Conduit keeps its own (open-conduit config path)")
	}
	return nil
}

func (o *operator) printMigration(source string, res MigrateResult, details []client.Detail, written, target string) {
	w := o.out
	fmt.Fprintf(w, "Open Supervisor config: %s (read only; nothing of v1 is modified)\n", source)
	fmt.Fprintf(w, "Scopes: %s\n\n", strings.Join(res.Scopes, ", "))
	for _, level := range []string{"action", "unsupported", "warn", "changed", "info"} {
		var lines []string
		for _, it := range res.Items {
			if it.Level == level {
				line := "  " + it.Path + ": " + it.Message
				if it.Target != "" {
					line += " [" + it.Target + "]"
				}
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(w, "%s:\n%s\n\n", strings.ToUpper(level[:1])+level[1:], strings.Join(lines, "\n"))
		}
	}
	if len(res.Secrets) > 0 {
		fmt.Fprintln(w, "Secret references (values are never read or copied):")
		for _, s := range res.Secrets {
			fmt.Fprintf(w, "  %s (%s) -> %s\n", s.From, s.Scheme, s.To)
			if s.Action != "" {
				fmt.Fprintf(w, "      %s\n", s.Action)
			}
		}
		fmt.Fprintln(w)
	}
	if len(details) > 0 {
		fmt.Fprintln(w, "Validation FAILED (offline, against the daemon's registrations):")
		for _, d := range details {
			fmt.Fprintf(w, "  %s  %s  %s\n", d.Path, d.Code, d.Message)
		}
		if target != "" {
			fmt.Fprintf(w, "Not written: %s\n", target)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "Validation: valid (offline, against the daemon's registrations)")
	}
	if written != "" {
		fmt.Fprintf(w, "Written: %s (0600). Next: complete the actions above, then open-conduit config check --file %s\n", written, written)
		return
	}
	fmt.Fprintln(w, "Preview (nothing written; add --out FILE or --write to save):")
	_, _ = w.Write(res.Config)
}

// backup implements `backup create|verify|restore`.
func (o *operator) backup(args []string) int {
	if o.env.State == nil {
		return usageError(o.errOut, "offline state commands are not available in this binary")
	}
	if len(args) < 2 {
		return usageError(o.errOut, "expected backup create DEST | backup verify BUNDLE | backup restore BUNDLE --to DIR")
	}
	f := newFlags("backup", "to")
	pos, err := f.parse(args[1:])
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected one path")
	}
	cx, cancel := offlineCtx()
	defer cancel()
	switch args[0] {
	case "create":
		return o.emit(o.env.State.Backup(cx, o.stateDir(), o.abs(pos[0]), o.build))
	case "verify":
		return o.emit(o.env.State.Verify(cx, o.abs(pos[0])))
	case "restore":
		if f.get("to") == "" {
			return usageError(o.errOut, "backup restore needs --to DIR (an absent directory; restore never replaces a state directory)")
		}
		return o.emit(o.env.State.Restore(cx, o.abs(pos[0]), o.abs(f.get("to"))))
	}
	return usageError(o.errOut, "expected backup create|verify|restore")
}

// state implements `state check`.
func (o *operator) state(args []string) int {
	if o.env.State == nil {
		return usageError(o.errOut, "offline state commands are not available in this binary")
	}
	if len(args) == 0 || args[0] != "check" {
		return usageError(o.errOut, "expected state check [--json]")
	}
	asJSON := len(args) == 2 && args[1] == "--json"
	if len(args) > 2 || len(args) == 2 && !asJSON {
		return usageError(o.errOut, "expected state check [--json]")
	}
	cx, cancel := offlineCtx()
	defer cancel()
	r, err := o.env.State.Check(cx, o.stateDir())
	if err != nil {
		return o.emit(nil, err)
	}
	if asJSON {
		o.emit(r, nil)
	} else {
		fmt.Fprintf(o.out, "State directory %s: %s\n", r.StateDir, map[bool]string{true: "ok", false: "PROBLEMS FOUND"}[r.OK])
		for _, c := range r.Checks {
			fmt.Fprintf(o.out, "  %-5s %-12s %-22s %s\n", c.Status, c.Name, c.Code, c.Message)
		}
		for _, s := range r.Scopes {
			fmt.Fprintf(o.out, "  scope %s: works %v, live executions %d, active waits %d, drained %s\n", s.ID, s.Works, s.LiveExecutions, s.ActiveWaits, strconv.FormatBool(s.Drained))
		}
	}
	if !r.OK {
		return 1
	}
	return 0
}

// recoverOperator implements `auth recover`: the locked local credential
// reset for a lost or ambiguous operator token (B05).
func (o *operator) recoverOperator() int {
	if o.env.State == nil {
		return usageError(o.errOut, "offline state commands are not available in this binary")
	}
	cx, cancel := offlineCtx()
	defer cancel()
	path := o.tokenPath()
	if err := o.env.State.RecoverOperator(cx, o.stateDir(), path); err != nil {
		return o.emit(nil, err)
	}
	return o.emit(map[string]string{"token_file": path, "note": "every earlier operator credential is revoked"}, nil)
}
