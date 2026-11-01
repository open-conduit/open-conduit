package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// setupBinding is a setup session's agent: Claude Code (binary, keeping
// its state under root) with its built-in tools off and only the operations
// a session may have, offering a shell the operator may turn on for one
// session.
func setupBinding(root, binary string) map[string]any {
	return map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true},
		"operations": []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask"}, "secret_names": []any{},
		"config": map[string]any{"binary": binary, "playbook": "-", "builtin_tools": []string{}, "model": "opus", "models": []string{"opus", "sonnet"}, "cold_turns": "resume",
			"env": map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}},
		"grants": []any{map[string]any{"name": "shell", "label": "A shell", "config": map[string]any{"builtin_tools": []string{"Bash"}}}}}
}

// setupDaemonConfig is a scope with a `setup` entry and route bound to
// binding.
func setupDaemonConfig(binding map[string]any) []byte {
	ops := binding["operations"]
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "setup", "name": "Setup",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings":      []any{binding},
			"routes": []any{map[string]any{"name": "setup", "handler": "setup", "binding": "agent", "active_policy": "inject",
				"unsupported_fallback": "queue", "error_fallback": "queue", "config": map[string]any{}}},
			"extensions":  []any{map[string]any{"name": "setup", "type": "setup", "secret_names": []any{}, "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonDefaultConfiguration: with no config file and no stored
// generation the daemon starts on the default configuration, a setup scope,
// without writing the file; doctor says so and the web view can start a
// session. The first apply writes the file, and a restart keeps it.
func TestDaemonDefaultConfiguration(t *testing.T) {
	d := newDrill(t)
	if err := os.Remove(d.config); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	cfg, err := d.c.Config(ctx)
	if err != nil || cfg.Generation != "1" || len(cfg.Scopes) != 1 || cfg.Scopes[0].ID != "setup" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if _, err = os.Stat(d.config); !os.IsNotExist(err) {
		t.Fatalf("the default configuration wrote the file: %v", err)
	}
	code := func() string {
		doc, err := d.c.Doctor(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.Name == "config" {
				return c.Status + "/" + c.Code
			}
		}
		return ""
	}
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config: %s", got)
	}
	acts, err := d.c.Actions(ctx, client.ActionTarget{ScopeID: "setup"})
	if err != nil || len(acts) != 1 || acts[0].ID != "start" || strings.Join(acts[0].Choices, ",") != "opus,sonnet" {
		t.Fatalf("actions: %+v %v", acts, err)
	}

	d.kill()
	d.start("")
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config after a restart: %s", got)
	}
	// The session's agent never runs here.
	binary := filepath.Join(d.root, "claude")
	if err = os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	next := setupDaemonConfig(setupBinding(d.root, binary))
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(d.config); err != nil {
		t.Fatalf("apply wrote no file: %v", err)
	}
	if got := code(); got != "ok/current" {
		t.Fatalf("doctor config after apply: %s", got)
	}
}
