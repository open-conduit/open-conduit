package management

import (
	"context"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/config"
)

func TestSecretFailureNamesMissingCommand(t *testing.T) {
	ctx := context.Background()
	r := &config.Resolver{}
	ref := "cmd:no-such-command-oc-test auth --password hunter2"
	_, err := r.Resolve(ctx, ref)
	got := secretFailure(ref, err)
	for _, want := range []string{"command_not_found", "no-such-command-oc-test not found", config.CommandPath, "absolute path"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "auth") {
		t.Fatalf("remedy leaks arguments: %q", got)
	}
	ref = "cmd:exit 3"
	_, err = r.Resolve(ctx, ref)
	if got = secretFailure(ref, err); got != "command_failed" {
		t.Fatalf("other failures keep their bare code: %q", got)
	}
}
