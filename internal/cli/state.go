package cli

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// The offline recovery commands. internal/cli reaches the state directory
// only through functions the open-conduit entrypoint injects (Env.State),
// so neither the os binary nor this package imports SQLite or the backup
// composition.

// BackupInfo describes a published or verified bundle.
type BackupInfo struct {
	Path string `json:"path"`
	// Backend names the storage backend the bundle's snapshot is for.
	Backend   string    `json:"backend,omitempty"`
	Schema    uint32    `json:"schema"`
	Build     string    `json:"build"`
	CreatedAt time.Time `json:"created_at"`
	Artifacts int       `json:"artifacts"`
	// OmittedArtifacts counts references whose content retention expired.
	OmittedArtifacts int      `json:"omitted_artifacts,omitempty"`
	Scopes           []string `json:"scopes,omitempty"`
	// TokenFile is the fresh operator credential a restore wrote.
	TokenFile string `json:"token_file,omitempty"`
	// ConfigGeneration is the committed config generation a restore
	// brought back (0 when the bundle holds none): the daemon starts from
	// it, and `config export` writes it out as a file.
	ConfigGeneration uint64 `json:"config_generation,omitempty"`
	// ConfigProblems are why the restored generation doesn't validate
	// against the packages installed here: a type no installed or bundled
	// package provides (unknown_registration), a package that couldn't
	// start, an entry a package rejects. With any, the daemon doesn't start
	// from the generation: it publishes the config file over it when the
	// file validates, and otherwise refuses to start.
	ConfigProblems []ConfigEntry `json:"config_problems,omitempty"`
}

// ConfigEntry is one config diagnostic a restore reports, by its path in
// the config document and its code (as config check reports them).
type ConfigEntry struct {
	Path    string `json:"path,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
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

// RestoreOptions are `backup restore`'s choices for the restored
// directory. Without ArtifactBackend its artifacts go to the file store in
// the directory; with one they are imported into that registered artifact
// backend at ArtifactLocation (which must be empty) and the directory's
// storage.json records it.
type RestoreOptions struct {
	ArtifactBackend  string
	ArtifactLocation string
	// ConfigPath is the config file the restored directory will run with:
	// its installed packages are the ones the restored generation is
	// checked against.
	ConfigPath string
}

// StateOps are the offline state-directory operations. Each takes the
// state directory's exclusive daemon lock, so it refuses to run while a
// daemon owns the directory.
type StateOps struct {
	Backup          func(ctx context.Context, stateDir, destination, build string) (BackupInfo, error)
	Verify          func(ctx context.Context, bundle string) (BackupInfo, error)
	Restore         func(ctx context.Context, bundle, destination string, o RestoreOptions) (BackupInfo, error)
	Check           func(ctx context.Context, stateDir string) (StateReport, error)
	RecoverOperator func(ctx context.Context, stateDir, tokenPath string) error
}

func offlineCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Minute)
}

// backup implements `backup create|verify|restore`.
func (o *operator) backup(args []string) int {
	if o.env.State == nil {
		return usageError(o.errOut, "offline state commands are not available in this binary")
	}
	if len(args) < 2 {
		return usageError(o.errOut, "expected backup create DEST | backup verify BUNDLE | backup restore BUNDLE --to DIR [--artifacts NAME --artifacts-location LOC] [--config FILE]")
	}
	f := newFlags("backup", "to", "artifacts", "artifacts-location", "config")
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
		return o.emit(o.env.State.Restore(cx, o.abs(pos[0]), o.abs(f.get("to")), RestoreOptions{ArtifactBackend: f.get("artifacts"), ArtifactLocation: f.get("artifacts-location"), ConfigPath: o.configPath(f.get("config"))}))
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
