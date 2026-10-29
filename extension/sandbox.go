package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/domain"
)

// SPIKE (#161): a process-scoped sandbox as a launch wrapper. Evidence for
// B09, not a contract.

// SandboxNeeds is what an executor's process asks for. A need is a request,
// never a grant: the broker intersects it with the operator's policy and
// refuses the launch (ErrSandboxDenied) when a need falls outside it.
type SandboxNeeds struct {
	Read    []string // paths the process must read (beyond what the policy leaves readable)
	Write   []string // paths the process must write
	Hosts   []string // outbound hosts, optionally host:port
	Sockets []string // unix sockets the process must connect to (the toolbelt)
}

// SandboxRequest is one launch: the argv the executor would run, its working
// directory and environment, and its needs. RunDir is the segment's private
// directory; the broker may write its policy file there.
type SandboxRequest struct {
	Argv   []string
	Dir    string
	Env    []string
	RunDir string
	Needs  SandboxNeeds
}

// SandboxLaunch is the wrapped launch. Digest pins the effective policy: the
// executor records it with the attempt's recovery metadata, and Recover adopts
// only a process launched under the same digest.
type SandboxLaunch struct {
	Argv    []string
	Env     []string
	Digest  string
	Summary string
	Policy  domain.Payload
}

// SandboxBroker wraps launches for one attempt. It is nil in ExecutionContext
// when the binding declares no sandbox. A broker never returns an unwrapped
// launch: anything it cannot enforce is an error and nothing runs.
type SandboxBroker interface {
	Wrap(context.Context, SandboxRequest) (SandboxLaunch, error)
}

var (
	// ErrSandboxDenied: a need falls outside the operator's policy.
	ErrSandboxDenied = errors.New("sandbox_denied")
	// ErrSandboxUnavailable: the sandbox can't be enforced here (binary
	// missing, platform unsupported, policy not applicable).
	ErrSandboxUnavailable = errors.New("sandbox_unavailable")
)

// WrapLaunch is the one call an executor makes before it starts its process.
// With no broker it returns the launch unchanged. Otherwise it fails closed:
// a broker error, or a launch without an argv, means nothing runs. Digest is
// computed here over the wrapped argv and the provider's policy, so every
// provider pins the same way and Recover can check it without the provider.
func WrapLaunch(ctx context.Context, b SandboxBroker, r SandboxRequest) (SandboxLaunch, error) {
	if b == nil {
		return SandboxLaunch{Argv: r.Argv, Env: r.Env}, nil
	}
	l, err := b.Wrap(ctx, r)
	if err != nil {
		return SandboxLaunch{}, err
	}
	if len(l.Argv) == 0 || len(l.Policy.Data) == 0 {
		return SandboxLaunch{}, ErrSandboxUnavailable
	}
	l.Digest = SandboxDigest(l.Argv, l.Policy)
	return l, nil
}

// SandboxDigest pins a wrapped launch: its argv and its effective policy.
func SandboxDigest(argv []string, policy domain.Payload) string {
	b, _ := json.Marshal(struct {
		Argv   []string       `json:"argv"`
		Policy domain.Payload `json:"policy"`
	}{argv, policy})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SandboxReason is the structured failure reason for a WrapLaunch error.
func SandboxReason(err error) string {
	if errors.Is(err, ErrSandboxDenied) {
		return "sandbox_denied"
	}
	return "sandbox_unavailable"
}

// sandboxRecord is kept in the segment's run dir, which no grant makes
// writable from inside the sandbox.
type sandboxRecord struct {
	Digest string         `json:"digest"`
	Argv   []string       `json:"argv"`
	Policy domain.Payload `json:"policy"`
}

// WriteSandboxRecord keeps a wrapped launch with its segment, before launch.
func WriteSandboxRecord(runDir string, l SandboxLaunch) error {
	b, err := json.Marshal(sandboxRecord{Digest: l.Digest, Argv: l.Argv, Policy: l.Policy})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, "sandbox.json"), b, 0o600)
}

// VerifySandboxRecord proves, at Recover, that the segment was launched
// under the pinned digest. An empty digest means no sandbox was declared at
// launch; the segment must then have no record either.
func VerifySandboxRecord(runDir, digest string) error {
	b, err := os.ReadFile(filepath.Join(runDir, "sandbox.json"))
	if digest == "" {
		if err == nil {
			return ErrSandboxUnavailable
		}
		return nil
	}
	var rec sandboxRecord
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Digest != digest || SandboxDigest(rec.Argv, rec.Policy) != digest {
		return ErrSandboxUnavailable
	}
	return nil
}
