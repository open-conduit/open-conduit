package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
)

// SandboxBinding is the host's half of one attempt's sandbox (B09): what
// BindSandbox needs to merge an executor's needs with the resource's and the
// host's protected set, and to pin the plan.
type SandboxBinding struct {
	// Provider and Config are the binding's sandbox. A nil Provider binds
	// a broker that can only refuse new launches (Adopt still checks the
	// pinned plan), for a binding that no longer declares a sandbox.
	Provider SandboxProvider
	Owner    ResourceOwner
	Config   domain.Payload
	// Protected are the host's protected roots (the state directory, the
	// config file): denied, whatever the policy grants. Beneath them a
	// process may write only its own run directory and workspace, and read
	// only those and Shared.
	Protected []string
	// Shared are paths beneath Protected a process may read: the
	// execution's toolbelt directory and the published playbooks.
	Shared []string
	// RunRoot is the executor's runs directory: a launch's RunDir must be
	// a directory beneath it, reached without symbolic links.
	RunRoot string
	// Toolbelt is the execution's toolbelt socket path, if it has one.
	Toolbelt string
	// Records is the host-owned directory this execution's plans are kept
	// in, beneath Protected and outside every grant.
	Records string
	// Pin records a plan with the attempt's open segment, durably, before
	// the plan is returned. An error fails the launch.
	Pin func(SandboxPlan) error
	// Pinned is the digest of the plan a recovering segment was launched
	// under, "" when it ran unsandboxed.
	Pinned string
	// MaxScan bounds the hard-link check's walk of the workspace (default
	// 1,000,000 entries); a larger workspace can't be checked and is
	// refused.
	MaxScan int
}

// BoundSandbox is a SandboxBroker bound to one attempt. Its Resources wraps
// the attempt's resource broker, so the workspace the executor acquires is
// acquired for a sandbox and its needs reach the plan.
type BoundSandbox struct {
	b        SandboxBinding
	mu       sync.Mutex
	resource *Resource
}

// BindSandbox binds a sandbox provider to one attempt. Hosts call it;
// executors only see the broker.
func BindSandbox(b SandboxBinding) *BoundSandbox {
	if b.MaxScan <= 0 {
		b.MaxScan = 1_000_000
	}
	return &BoundSandbox{b: b}
}

// Resources wraps the attempt's resource broker: every acquisition is
// marked Sandboxed, and the resource acquired is kept for the plan.
func (s *BoundSandbox) Resources(r ResourceBroker) ResourceBroker {
	if r == nil {
		return nil
	}
	return sandboxedResources{r, s.keep}
}

// sandboxedResources marks every acquisition Sandboxed and hands what was
// acquired to keep, for a broker's plan.
type sandboxedResources struct {
	r    ResourceBroker
	keep func(*Resource)
}

func (w sandboxedResources) Acquire(ctx context.Context, req ResourceRequest) (Resource, error) {
	req.Sandboxed = true
	res, err := w.r.Acquire(ctx, req)
	if err == nil && res.Dir != "" {
		w.keep(&res)
	}
	return res, err
}
func (w sandboxedResources) Lookup(ctx context.Context, ref string) (Resource, error) {
	res, err := w.r.Lookup(ctx, ref)
	if err == nil && res.Dir != "" {
		w.keep(&res)
	}
	return res, err
}
func (s *BoundSandbox) keep(res *Resource) {
	s.mu.Lock()
	s.resource = res
	s.mu.Unlock()
}

// The root stub records the sandbox root's pid in the plan's record
// directory, then execs the provider's argv in its place.
const sandboxRoot = `printf '%s\n' "$$" > "$1/root.tmp" && mv -f "$1/root.tmp" "$1/root" && shift && exec "$@"`

// sandboxRecord is a plan as kept in its record directory: everything the
// digest covers, never the environment.
type sandboxRecord struct {
	Digest  string         `json:"digest"`
	Argv    []string       `json:"argv"`
	Signals string         `json:"signals"`
	Summary string         `json:"summary"`
	Data    domain.Payload `json:"data"`
}

// SandboxDigest pins a plan: its argv and its effective policy.
func SandboxDigest(argv []string, data domain.Payload) string {
	b, _ := json.Marshal(struct {
		Argv []string       `json:"argv"`
		Data domain.Payload `json:"data"`
	}{argv, data})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *BoundSandbox) Plan(ctx context.Context, needs SandboxNeeds, l SandboxLaunch) (SandboxPlan, error) {
	b := s.b
	if b.Provider == nil {
		return SandboxPlan{}, fmt.Errorf("%w: the binding no longer declares a sandbox", ErrSandboxUnavailable)
	}
	if len(l.Argv) == 0 || !filepath.IsAbs(l.Dir) || !filepath.IsAbs(l.RunDir) || b.Records == "" || b.Pin == nil {
		return SandboxPlan{}, fmt.Errorf("%w: incomplete launch", ErrSandboxUnavailable)
	}
	s.mu.Lock()
	res := s.resource
	s.mu.Unlock()
	g, err := GuardLaunch(LaunchGuard{Protected: b.Protected, Shared: b.Shared, RunRoot: b.RunRoot, Toolbelt: b.Toolbelt, Records: b.Records,
		MaxScan: b.MaxScan, Dir: l.Dir, RunDir: l.RunDir, Resource: res, Needs: needs})
	if err != nil {
		return SandboxPlan{}, err
	}
	workspace, run := g.Workspace, g.Run
	merged := needs.Merge(g.Resource)
	merged.Read = append(merged.Read, g.Shared...)
	merged.Deny = append(merged.Deny, g.Protected...)
	merged = canonicalNeeds(merged)
	sum := sha256.Sum256([]byte(run))
	dir := filepath.Join(b.Records, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	paths := SandboxPaths{Workspace: workspace, Run: run, Toolbelt: b.Toolbelt, Records: dir}
	l.Dir, l.RunDir = workspace, run
	p, err := b.Provider.Plan(ctx, b.Owner, SandboxPolicy{Config: b.Config, Paths: paths}, merged, l)
	if err != nil {
		return SandboxPlan{}, err
	}
	if len(p.Argv) == 0 || len(p.Data.Data) == 0 || (p.Signals != SandboxTerm && p.Signals != SandboxKill) {
		return SandboxPlan{}, fmt.Errorf("%w: the provider returned an incomplete plan", ErrSandboxUnavailable)
	}
	p.Argv = append([]string{"/bin/sh", "-c", sandboxRoot, "open-conduit-sandbox", dir}, p.Argv...)
	p.Digest = SandboxDigest(p.Argv, p.Data)
	rec, _ := json.Marshal(sandboxRecord{Digest: p.Digest, Argv: p.Argv, Signals: p.Signals, Summary: p.Summary, Data: p.Data})
	_ = os.Remove(filepath.Join(dir, "root"))
	if err := writeOnce(filepath.Join(dir, "plan.json"), rec); err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: could not record the plan: %v", ErrSandboxUnavailable, err)
	}
	if err := b.Pin(p); err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: could not pin the plan: %v", ErrSandboxUnavailable, err)
	}
	return p, nil
}

func (s *BoundSandbox) Adopt(ctx context.Context, parent int) error {
	b := s.b
	if b.Pinned == "" {
		return errors.New("the segment was launched without a sandbox")
	}
	if b.Records == "" {
		return errors.New("no sandbox records")
	}
	entries, err := os.ReadDir(b.Records)
	if err != nil {
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(b.Records, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err != nil {
			continue
		}
		var rec sandboxRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Digest != b.Pinned {
			continue
		}
		// The record must still be the plan the segment was pinned to.
		if SandboxDigest(rec.Argv, rec.Data) != b.Pinned {
			return errors.New("the sandbox record does not match the pinned plan")
		}
		if b.Provider == nil {
			return errors.New("the sandbox provider is not available to prove the boundary")
		}
		root := 0
		if raw, err := os.ReadFile(filepath.Join(dir, "root")); err == nil {
			if root, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil || root <= 1 {
				return errors.New("unreadable sandbox root")
			}
		}
		plan := SandboxPlan{Argv: rec.Argv[5:], Digest: rec.Digest, Signals: rec.Signals, Summary: rec.Summary, Data: rec.Data}
		return b.Provider.Adopt(ctx, plan, parent, root)
	}
	return errors.New("no sandbox record for the pinned plan")
}

// writeOnce writes a read-only file in place of any earlier one.
func writeOnce(path string, data []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o400); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
