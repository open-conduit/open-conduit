package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

func TestFSListProbe(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{"code/web/.git", "code/tools", "code/.hidden", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://x-access-token:ghs_abcdefghijklmnopqrstuvwxyz@github.com/octo/web.git\n[remote \"fork\"]\n\turl = git@github.com:me/web.git\n"
	if err := os.WriteFile(filepath.Join(home, "code/web/.git/config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "out")); err != nil {
		t.Fatal(err)
	}
	p := Probes{Home: home}
	list := p.SetupProbes()[0]
	r, err := list.Run(context.Background(), map[string]string{"dir": "~/code", "depth": "2"})
	b, _ := json.Marshal(r.Output)
	got := string(b)
	if err != nil || !strings.Contains(got, `{"path":"web","kind":"git","remotes":["fork github.com:me/web.git","origin https://github.com/octo/web.git"]}`) ||
		!strings.Contains(got, `{"path":"tools","kind":"dir"}`) || !strings.Contains(got, `"hidden":1`) || strings.Contains(got, "ghs_") || strings.Contains(got, "access-token") {
		t.Fatalf("list: %s %v", got, err)
	}
	for _, args := range []map[string]string{{"dir": "/etc"}, {"dir": "~/out"}, {"dir": "~/code", "depth": "3"}, {"dir": "code"}, {"dir": "~/missing"}} {
		if _, err := list.Run(context.Background(), args); !errors.Is(err, extension.ErrProbeInput) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestToolsProbe(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Probes{LookPath: func(name string) (string, error) {
		if name == "gh" {
			return script, nil
		}
		return "", errors.New("not found")
	}}
	r, err := p.SetupProbes()[1].Run(context.Background(), nil)
	b, _ := json.Marshal(r.Output)
	if err != nil || !strings.Contains(string(b), `{"name":"gh","found":true,"path":"`+script+`","script":true}`) || !strings.Contains(string(b), `{"name":"git","found":false}`) {
		t.Fatalf("tools: %s %v", b, err)
	}
}

func TestSecretScan(t *testing.T) {
	for _, s := range []string{"ghp_" + strings.Repeat("a", 36), "github_pat_" + strings.Repeat("b", 30), "xoxb-1234567890-abc", "lin_api_" + strings.Repeat("c", 30), "sk-ant-api03-" + strings.Repeat("d", 30), "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		if scanText("here: "+s) == nil || !looksLikeSecret(map[string]any{"x": s}) {
			t.Errorf("missed %s", s)
		}
	}
	for _, s := range []string{"keyring:github-token", "use the work account", "ghp_short"} {
		if scanText(s) != nil {
			t.Errorf("refused %q", s)
		}
	}
}
