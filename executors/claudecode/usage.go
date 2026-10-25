package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// Subscription usage windows (v1 Usage): the 5-hour and 7-day percentages
// /usage shows, read from the OAuth usage endpoint Claude Code itself uses
// with the machine's Claude login. The endpoint is undocumented, so callers
// treat failure as "usage unknown" rather than a fault. Reading it is opt-in
// for the host (it reads the login token), and nothing here is retained.

// UsageURL is the OAuth usage endpoint.
const UsageURL = "https://api.anthropic.com/api/oauth/usage"

// UsageWindow is one rate-limit window.
type UsageWindow struct {
	Name     string    `json:"name"`
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
}

// UsageOptions configure a usage read; zero values use the login token, the
// public endpoint and a 10-second client.
type UsageOptions struct {
	URL   string
	Token func(context.Context) (string, error)
	HTTP  *http.Client
	// ConfigDir is Claude Code's config directory (default ~/.claude).
	ConfigDir string
}

// LoginToken reads the Claude login's OAuth access token: the macOS Keychain
// item Claude Code keeps, else <configDir>/.credentials.json.
func LoginToken(ctx context.Context, configDir string) (string, error) {
	var raw []byte
	if goruntime.GOOS == "darwin" {
		if out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output(); err == nil {
			raw = out
		}
	}
	if raw == nil {
		if configDir == "" {
			home, _ := os.UserHomeDir()
			configDir = filepath.Join(home, ".claude")
		}
		data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
		if err != nil {
			return "", errors.New("no Claude login found (sign in with `claude` first)")
		}
		raw = data
	}
	var creds struct {
		ClaudeAIOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &creds); err != nil || creds.ClaudeAIOauth.AccessToken == "" {
		return "", errors.New("no Claude login token (an API-key login has no subscription windows)")
	}
	return creds.ClaudeAIOauth.AccessToken, nil
}

// Usage reads the subscription's windows.
func Usage(ctx context.Context, o UsageOptions) ([]UsageWindow, error) {
	if o.URL == "" {
		o.URL = UsageURL
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Token == nil {
		o.Token = func(ctx context.Context) (string, error) { return LoginToken(ctx, o.ConfigDir) }
	}
	token, err := o.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "open-conduit")
	res, err := o.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("usage endpoint unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint answered %d", res.StatusCode)
	}
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var body struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, errors.New("usage endpoint answered an unknown shape")
	}
	var out []UsageWindow
	add := func(name string, w *window) {
		if w == nil || w.Utilization == nil || w.ResetsAt == nil {
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, *w.ResetsAt)
		out = append(out, UsageWindow{Name: name, Used: *w.Utilization, ResetsAt: at})
	}
	add("5h", body.FiveHour)
	add("7d", body.SevenDay)
	return out, nil
}

// Version runs `<binary> --version` (the doctor check that Claude Code is
// installed). It starts no session.
func Version(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version failed: install Claude Code or set the binding's binary", filepath.Base(binary))
	}
	v := strings.TrimSpace(string(out))
	if len(v) > 120 {
		v = v[:120]
	}
	return v, nil
}
