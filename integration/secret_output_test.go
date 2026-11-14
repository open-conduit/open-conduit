package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	servedSecret = "vault-served-secret-0123456789"
	servedPublic = "vault-public-id"
)

// secretScript prints a secret-output operation's answer every way a run
// can publish it: its log, an output file, and a retried call with a
// request ID.
const secretScript = `set -e
os call vault.key --output data
os call vault.key --output data > "$OS_OUTPUT_DIR/key.json"
os call vault.key --request-id r1 --output data > "$OS_OUTPUT_DIR/retry.json"
os call vault.key --request-id r1 --output data >> "$OS_OUTPUT_DIR/retry.json"
if os call vault.odd --output data 2> "$OS_OUTPUT_DIR/odd.txt"; then echo "an unscrubbable secret was handed over" >&2; exit 9; fi
echo "plain line"
s=$(os call vault.key --output data | sed 's/.*"secret":"\([^"]*\)".*/\1/')
os emit --type fixture.reported.v1 --delivery-id leak-1 --schema fixture.v1 --data "{\"note\":\"$s\"}" > /dev/null
os outcome completed --summary "key $s" --schema fixture.v1 --data "{\"key\":\"$s\"}"
`

func secretShellConfig(t *testing.T, ops ...string) string {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{},
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"command": secretScript}}},
			"routes": []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(cfg)
}

// TestSecretOutputScrubbed runs a shell binding that prints a secret-output
// operation's answer. The run's published artifacts and outcome carry
// [REDACTED] in place of the secret, keep the answer's other fields and the
// script's own output, and the outcome names the scrubbed copies. The
// result is never replayed, so the request ID runs the handler again. The
// negative control is the same run with no SecretOutput declared: then the
// secret is published as printed.
func TestSecretOutputScrubbed(t *testing.T) {
	for _, declared := range []bool{true, false} {
		name := "declared"
		if !declared {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			runs := 0
			op := extension.Operation{Name: "vault.key", Summary: "A key.", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
				mu.Lock()
				runs++
				mu.Unlock()
				return domain.Payload{Schema: "vault.key.v1", Data: []byte(`{"id":"` + servedPublic + `","secret":"` + servedSecret + `"}`)}, nil
			}}
			// vault.odd's secret output isn't an object, so the host can't
			// read the values to scrub, and refuses to hand it over.
			odd := extension.Operation{Name: "vault.odd", Summary: "Not an object.", SecretOutput: []string{"secret"}, Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
				return domain.Payload{Schema: "vault.odd.v1", Data: []byte(`"` + servedSecret + `"`)}, nil
			}}
			if declared {
				op.SecretOutput = []string{"secret"}
			} else {
				odd.Handler = func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
					return domain.Payload{}, &extension.OperationError{Code: "secret_output_unscrubbable", Message: "control"}
				}
			}
			state, bin := shellState(t, secretShellConfig(t, "vault.key", "vault.odd", "emit", "declare_outcome"))
			h := newShellHostWith(t, state, bin, conformance.NewClock(), "daemon", false, shellHostOptions{
				operations: []string{"vault.key", "vault.odd"},
				register: func(storage.Store) extension.Registration {
					return extension.Registration{Version: extension.ContractVersion, Name: "vault", Operations: []extension.Operation{op, odd}}
				}})
			wait := extension.Operation{Name: "other.wait", SecretOutput: []string{"x"}, Wait: func(context.Context, extension.Authority, domain.Payload) (extension.WaitPlan, error) {
				return extension.WaitPlan{}, nil
			}}
			if err := h.toolbelt.Register(extension.Registration{Version: extension.ContractVersion, Name: "other", Operations: []extension.Operation{wait}}); !errors.Is(err, extension.ErrInvalid) {
				t.Fatalf("a wait with a secret output registered: %v", err)
			}
			ctx := context.Background()
			c := h.operator()
			root := h.submit("operator.requested", "secret-1", "secret")
			run, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
			if err != nil {
				t.Fatal(err)
			}
			h.observe(run)
			x, err := c.Execution(ctx, string(run.Lease.ExecutionID))
			if err != nil {
				t.Fatal(err)
			}
			if x.Outcome == nil || x.Outcome.Kind != "completed" {
				t.Fatalf("outcome: %+v", x.Outcome)
			}
			var all strings.Builder
			for _, a := range x.Artifacts {
				all.WriteString(h.artifact(a.ID))
			}
			published := all.String() + x.Outcome.Summary + string(x.Outcome.Data.Data)
			if !strings.Contains(published, servedPublic) || !strings.Contains(published, "plain line") {
				t.Fatalf("published output lost what isn't secret: %q", published)
			}
			// The declared outcome replaces the shell's, so the output
			// files are found by what they hold.
			key := all.String()
			if !strings.Contains(key, "secret_output_unscrubbable") {
				t.Fatalf("vault.odd wasn't refused: %q", key)
			}
			// What the run sent through its toolbelt: the Event it emitted
			// and the outcome it declared.
			var emitted strings.Builder
			h.coord.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
				events, err := tx.Events(0, 0)
				for _, e := range events {
					if e.Event.Type == "fixture.reported.v1" {
						emitted.Write(e.Event.Data.Data)
					}
				}
				return err
			})
			if !strings.Contains(emitted.String(), "note") {
				t.Fatalf("no emitted Event: %q", emitted.String())
			}
			if !declared {
				if !strings.Contains(key, servedSecret) || !strings.Contains(x.Outcome.Summary, servedSecret) || !strings.Contains(emitted.String(), servedSecret) {
					t.Fatalf("control: an undeclared secret was scrubbed: %q, %q, %q", key, x.Outcome.Summary, emitted.String())
				}
				return
			}
			if x.Outcome.Summary != "key "+runtime.Redacted || strings.Contains(string(x.Outcome.Data.Data), servedSecret) || strings.Contains(emitted.String(), servedSecret) {
				t.Fatalf("the declared outcome or emitted Event kept the secret: %+v, %q", x.Outcome, emitted.String())
			}
			if strings.Contains(published, servedSecret) {
				t.Fatalf("a served secret was published: %q", published)
			}
			if !strings.Contains(key, `{"id":"`+servedPublic+`","secret":"`+runtime.Redacted+`"}`) {
				t.Fatalf("no scrubbed answer published: %q", key)
			}
			mu.Lock()
			n := runs
			mu.Unlock()
			if n != 5 {
				t.Fatalf("vault.key ran %d times, want 5: a secret output was replayed", n)
			}
			if got := h.served.Values(run.Lease.ExecutionID); len(got) != 1 || got[0] != servedSecret {
				t.Fatalf("served = %q", got)
			}
			// Once the execution is over and its stop needs no confirming,
			// its values are forgotten.
			if err = h.served.Reconcile(func(ids []domain.ID) (map[domain.ID]bool, error) { return h.scheduler.Holding(ctx, ids) }); err != nil {
				t.Fatal(err)
			}
			if got := h.served.Values(run.Lease.ExecutionID); len(got) != 0 {
				t.Fatalf("a finished execution's secrets are kept: %q", got)
			}
		})
	}
}
