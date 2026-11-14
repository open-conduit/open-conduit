package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Ambient resolves the daemon's own AWS identity when a connection has no
// key pair: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (with
// AWS_SESSION_TOKEN) in its environment, or else whatever the AWS CLI's
// credential chain finds (an SSO or named profile, an instance profile,
// a container role), through `aws configure export-credentials`. Temporary
// credentials are kept until five minutes before they expire.
type Ambient struct {
	// Profile is the AWS CLI profile to export, "" for the default chain.
	Profile string
	// Getenv and Export replace the environment and the AWS CLI (tests).
	Getenv func(string) string
	Export func(ctx context.Context, profile string) ([]byte, error)
	Now    func() time.Time

	mu      sync.Mutex
	cached  Keys
	expires time.Time
}

// Keys resolves the ambient identity.
func (a *Ambient) Keys(ctx context.Context) (Keys, error) {
	getenv := a.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if id, secret := getenv("AWS_ACCESS_KEY_ID"), getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" && a.Profile == "" {
		return Keys{AccessKeyID: id, SecretAccessKey: secret, SessionToken: getenv("AWS_SESSION_TOKEN")}, nil
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached.AccessKeyID != "" && (a.expires.IsZero() || now().Before(a.expires.Add(-5*time.Minute))) {
		return a.cached, nil
	}
	export := a.Export
	if export == nil {
		export = exportCLI
	}
	raw, err := export(ctx, a.Profile)
	if err != nil {
		return Keys{}, ErrNoBaseIdentity
	}
	var p Process
	if json.Unmarshal(raw, &p) != nil || p.AccessKeyID == "" || p.SecretAccessKey == "" {
		return Keys{}, ErrNoBaseIdentity
	}
	a.cached = Keys{AccessKeyID: p.AccessKeyID, SecretAccessKey: p.SecretAccessKey, SessionToken: p.SessionToken}
	a.expires = time.Time{}
	if p.Expiration != "" {
		if t, err := time.Parse(time.RFC3339, p.Expiration); err == nil {
			a.expires = t
		}
	}
	return a.cached, nil
}

func exportCLI(ctx context.Context, profile string) ([]byte, error) {
	path, err := exec.LookPath("aws")
	if err != nil {
		return nil, errors.New("no aws CLI")
	}
	args := []string{"configure", "export-credentials", "--format", "process"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, args...).Output()
}

// Process is the JSON a credential_process prints, which the AWS CLI and
// every AWS SDK read.
type Process struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken,omitempty"`
	Expiration      string `json:"Expiration,omitempty"`
}
