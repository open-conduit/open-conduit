package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// Authenticity is what checking the bundle's signature found: verified
	// (a trusted key signed it), unverified (signed, with no trust key to
	// check it against) or unsigned. SigningKey is the key's ID.
	Authenticity string `json:"authenticity,omitempty"`
	SigningKey   string `json:"signing_key,omitempty"`
	// InPlace is what an in-place restore did to the state directory.
	InPlace *InPlaceReport `json:"in_place,omitempty"`
}

// InPlaceReport is what `backup restore --in-place` (or `serve --restore`)
// did to the daemon's own state directory.
type InPlaceReport struct {
	StateDir string `json:"state_dir"`
	// AlreadyRestored is set when serve --restore found the directory
	// already restored from this bundle and changed nothing.
	AlreadyRestored bool `json:"already_restored,omitempty"`
	// MovedAside is the before-restore-* directory the replaced state went
	// to, and MovedItems what went there (paths relative to the state
	// directory). Delete it once the restored instance has run for a while.
	MovedAside string   `json:"moved_aside,omitempty"`
	MovedItems []string `json:"moved_items,omitempty"`
	// RemovedRuns are the executions whose toolbelt run directories and
	// sockets were removed, since the restored database doesn't know them.
	RemovedRuns []string `json:"removed_runs,omitempty"`
	// RemovedStages are stages earlier restores left when they stopped
	// before their swap began.
	RemovedStages []string `json:"removed_stages,omitempty"`
	// LaterTimeline is what stays in the directory from after the bundle,
	// with what happens to each: reported, never deleted.
	LaterTimeline []LaterEntry `json:"later_timeline,omitempty"`
	// Check is the state check of the restored directory.
	Check *StateReport `json:"check,omitempty"`
}

// LaterEntry is one part of the state directory an in-place restore left
// in place, and what becomes of it.
type LaterEntry struct {
	Path string `json:"path"`
	Note string `json:"note"`
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
	// Once makes an in-place restore a no-op when the directory was
	// already restored from this bundle (serve --restore, which runs on
	// every start).
	Once bool
	// TrustKeys are public keys to accept besides the config file's
	// settings.backup.trust_keys, for a host that has lost its config.
	TrustKeys []string
	// AcceptUnsigned lets a bundle with no signature through when trust
	// keys are configured. A bundle with a wrong signature is still refused.
	AcceptUnsigned bool
}

// BackupOptions are `backup create`'s choices.
type BackupOptions struct {
	// ConfigPath is the config file whose settings.backup.signing_key signs
	// the bundle.
	ConfigPath string
	// Unsigned publishes without signing even when a key is configured.
	Unsigned bool
}

// KeyPair is a new signing key: the private half went to Path.
type KeyPair struct {
	Path      string `json:"path"`
	PublicKey string `json:"public_key"`
}

// StateOps are the offline state-directory operations. Each takes the
// state directory's exclusive daemon lock, so it refuses to run while a
// daemon owns the directory.
type StateOps struct {
	// Keygen makes a signing key's private and public text forms.
	Keygen  func() (private, public string, err error)
	Backup  func(ctx context.Context, stateDir, destination, build string, o BackupOptions) (BackupInfo, error)
	Verify  func(ctx context.Context, bundle string, o RestoreOptions) (BackupInfo, error)
	Restore func(ctx context.Context, bundle, destination string, o RestoreOptions) (BackupInfo, error)
	// RestoreInPlace restores a bundle into stateDir itself, moving the
	// state it replaces aside.
	RestoreInPlace  func(ctx context.Context, bundle, stateDir string, o RestoreOptions) (BackupInfo, error)
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
		return usageError(o.errOut, "expected backup keygen FILE | backup create DEST [--unsigned] | backup verify BUNDLE | backup restore BUNDLE (--to DIR | --in-place) [--artifacts NAME --artifacts-location LOC] [--trust-key KEYS] [--accept-unsigned] [--config FILE]")
	}
	f := newFlags("backup", "to", "artifacts", "artifacts-location", "config", "trust-key")
	inPlace := f.fs.Bool("in-place", false, "")
	unsigned := f.fs.Bool("unsigned", false, "")
	acceptUnsigned := f.fs.Bool("accept-unsigned", false, "")
	pos, err := f.parse(args[1:])
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected one path")
	}
	cx, cancel := offlineCtx()
	defer cancel()
	trust := splitKeys(f.get("trust-key"))
	switch args[0] {
	case "keygen":
		return o.emit(o.backupKeygen(o.abs(pos[0])))
	case "create":
		return o.emit(o.env.State.Backup(cx, o.stateDir(), o.abs(pos[0]), o.build, BackupOptions{ConfigPath: o.configPath(f.get("config")), Unsigned: *unsigned}))
	case "verify":
		return o.emit(o.env.State.Verify(cx, o.abs(pos[0]), RestoreOptions{ConfigPath: o.configPath(f.get("config")), TrustKeys: trust, AcceptUnsigned: *acceptUnsigned}))
	case "restore":
		ro := RestoreOptions{ArtifactBackend: f.get("artifacts"), ArtifactLocation: f.get("artifacts-location"), ConfigPath: o.configPath(f.get("config")), TrustKeys: trust, AcceptUnsigned: *acceptUnsigned}
		switch {
		case *inPlace && f.get("to") != "":
			return usageError(o.errOut, "backup restore takes --to DIR (a new directory) or --in-place (the daemon's own), not both")
		case *inPlace:
			return o.restoredInPlace(o.env.State.RestoreInPlace(cx, o.abs(pos[0]), o.stateDir(), ro))
		case f.get("to") == "":
			return usageError(o.errOut, "backup restore needs --to DIR (an absent directory) or --in-place (the daemon's own state directory, with the daemon stopped)")
		}
		return o.emit(o.env.State.Restore(cx, o.abs(pos[0]), o.abs(f.get("to")), ro))
	}
	return usageError(o.errOut, "expected backup keygen|create|verify|restore")
}

// splitKeys reads a comma-separated --trust-key.
func splitKeys(text string) []string {
	var out []string
	for _, k := range strings.Split(text, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// backupKeygen writes a new signing key to an absent owner-only file and
// returns its public half. The private key is never printed.
func (o *operator) backupKeygen(path string) (KeyPair, error) {
	private, public, err := o.env.State.Keygen()
	if err != nil {
		return KeyPair{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return KeyPair{}, err
	}
	_, err = f.WriteString(private + "\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return KeyPair{}, err
	}
	return KeyPair{Path: path, PublicKey: public}, nil
}

// restoredInPlace prints an in-place restore's report, and fails when the
// restored directory's state check found problems.
func (o *operator) restoredInPlace(info BackupInfo, err error) int {
	if code := o.emit(info, err); code != 0 {
		return code
	}
	if info.InPlace != nil && info.InPlace.Check != nil && !info.InPlace.Check.OK {
		fmt.Fprintln(o.errOut, "open-conduit: the restored state directory has problems (in_place.check)")
		return 1
	}
	return 0
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
