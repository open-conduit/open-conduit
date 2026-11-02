package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// checkedProvider answers config check's workspace checks; only those
// methods are called.
type checkedProvider struct{ extension.ResourceProvider }

func (checkedProvider) ModeDefault(raw json.RawMessage) string {
	var c struct{ Mode string }
	_ = json.Unmarshal(raw, &c)
	return c.Mode
}

func (checkedProvider) SandboxWorkspace(_ json.RawMessage, mode string) error {
	if mode == "shared" {
		return errors.New("a shared directory is outside the sandbox; use mode work")
	}
	return nil
}

// modedExecutor says which mode its config requests; only that is called.
type modedExecutor struct{ extension.Executor }

func (modedExecutor) ResourceMode(raw json.RawMessage) string {
	var c struct {
		WorkspaceMode string `json:"workspace_mode"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.WorkspaceMode
}

// TestWorkspaceChecks: validate's resource_mode, resource_sandbox and
// executor_mode kinds are answered from the provider's and executor's
// optional interfaces, with the sandbox refusal's reason kept for the
// operator, and never reach Validate. A provider or executor without the
// interfaces names no mode and passes (the negative control).
func TestWorkspaceChecks(t *testing.T) {
	validated := false
	ext := &sdk.Extension{Manifest: manifest(),
		Providers: map[string]extension.ResourceProvider{"checked": checkedProvider{}, "plain": struct{ extension.ResourceProvider }{}},
		Executors: map[string]extension.Executor{"moded": modedExecutor{}, "plain": struct{ extension.Executor }{}},
		Validate: func(context.Context, protocol.ValidateParams) error {
			validated = true
			return errors.New("not for these kinds")
		},
	}
	host, _, _ := serve(t, ext)
	ctx := context.Background()
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1}}, nil); err != nil {
		t.Fatal(err)
	}
	mode := func(kind, typ, config string) string {
		t.Helper()
		var out protocol.ValidateResult
		if err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: kind, Type: typ, Config: json.RawMessage(config)}, &out); err != nil {
			t.Fatalf("%s %s: %v", kind, typ, err)
		}
		return out.Mode
	}
	if got := mode(protocol.ValidateResourceMode, "checked", `{"mode":"fresh"}`); got != "fresh" {
		t.Fatalf("resource mode %q", got)
	}
	if got := mode(protocol.ValidateExecutorMode, "moded", `{"workspace_mode":"work"}`); got != "work" {
		t.Fatalf("executor mode %q", got)
	}
	err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSandbox, Type: "checked", Config: json.RawMessage(`{}`), Mode: "shared"}, nil)
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeInvalid || pe.Message != "a shared directory is outside the sandbox; use mode work" {
		t.Fatalf("sandbox refusal: %v", err)
	}
	if err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSandbox, Type: "checked", Config: json.RawMessage(`{}`), Mode: "work"}, nil); err != nil {
		t.Fatalf("sandbox pass: %v", err)
	}
	// Without the interfaces: no mode, and the sandbox check passes.
	if got := mode(protocol.ValidateResourceMode, "plain", `{"mode":"fresh"}`); got != "" {
		t.Fatalf("plain provider named mode %q", got)
	}
	if got := mode(protocol.ValidateExecutorMode, "plain", `{"workspace_mode":"work"}`); got != "" {
		t.Fatalf("plain executor requested mode %q", got)
	}
	if err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSandbox, Type: "plain", Mode: "shared"}, nil); err != nil {
		t.Fatalf("plain provider refused: %v", err)
	}
	if validated {
		t.Fatal("a workspace check reached Validate")
	}
}

// warningExecutor warns about every config; only that is called.
type warningExecutor struct{ extension.Executor }

func (warningExecutor) ConfigWarnings(json.RawMessage) []string {
	return []string{"config.old is deprecated"}
}

// TestExecutorConfigWarnings: validate with kind executor answers an
// executor's ConfigWarner warnings once Validate passes; a refused config
// and an executor without the interface give none (the negative control).
func TestExecutorConfigWarnings(t *testing.T) {
	ext := &sdk.Extension{Manifest: manifest(),
		Executors: map[string]extension.Executor{"warned": warningExecutor{}, "plain": struct{ extension.Executor }{}},
		Validate: func(_ context.Context, p protocol.ValidateParams) error {
			if string(p.Config) == `{"bad":true}` {
				return errors.New("bad")
			}
			return nil
		},
	}
	host, _, _ := serve(t, ext)
	ctx := context.Background()
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1}}, nil); err != nil {
		t.Fatal(err)
	}
	warnings := func(typ, config string) ([]string, error) {
		var out protocol.ValidateResult
		err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExecutor, Type: typ, Config: json.RawMessage(config)}, &out)
		return out.Warnings, err
	}
	if got, err := warnings("warned", `{}`); err != nil || len(got) != 1 || got[0] != "config.old is deprecated" {
		t.Fatalf("warnings %q %v", got, err)
	}
	if got, err := warnings("warned", `{"bad":true}`); err == nil || len(got) != 0 {
		t.Fatalf("a refused config answered %q %v", got, err)
	}
	if got, err := warnings("plain", `{}`); err != nil || len(got) != 0 {
		t.Fatalf("an executor without ConfigWarner warned %q %v", got, err)
	}
}
