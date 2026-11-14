package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A toolbelt call refused with authority_recovering (the run's lease
// lapsed while the host slept, and recovery is adopting the process) is
// made again with the credential re-read from the token file, which
// recovery replaces, keeping its request ID; the caller sees only the
// answer. A call still refused once the budget is spent reports the
// refusal, and any other refusal is reported at once, never retried. git's
// credential helper retries its serving lookup the same way.
func TestToolbeltRetriesWhileAuthorityRecovers(t *testing.T) {
	dir, err := os.MkdirTemp("", "ocrc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket, tokenFile := filepath.Join(dir, "s"), filepath.Join(dir, "token")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string // "<bearer> <request_id>" per call
	accept, refuse := "Bearer fresh", `{"error":{"code":"authority_recovering","message":"recovering"}}`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call map[string]any
		_ = json.NewDecoder(r.Body).Decode(&call)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %v", r.Header.Get("Authorization"), call["request_id"]))
		ok, refusal := r.Header.Get("Authorization") == accept, refuse
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			status := http.StatusForbidden
			if strings.Contains(refusal, "authority_recovering") {
				status = http.StatusServiceUnavailable
			}
			w.WriteHeader(status)
			fmt.Fprint(w, refusal)
			return
		}
		if r.URL.Path == "/v1/toolbelt/git" {
			fmt.Fprint(w, `{"data":{"session":"gs_1","expires_at":"2026-10-31T10:00:00Z"}}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/toolbelt/git/") {
			fmt.Fprint(w, `{"data":{}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"output":{"schema":"fixture.v1","data":{"opened":true}}}}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN_FILE":
			return tokenFile
		}
		return ""
	}}
	var slept []time.Duration
	var onSleep func()
	prev := recoverySleep
	recoverySleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if onSleep != nil {
			onSleep()
		}
		return nil
	}
	t.Cleanup(func() { recoverySleep = prev })
	reset := func(token string) {
		t.Helper()
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		seen, slept, onSleep = nil, nil, nil
		mu.Unlock()
	}
	run := func() (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", []string{"call", "--request-id", "r1", "assist.spawn"}, env, nil, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	t.Run("recovered", func(t *testing.T) {
		reset("stale")
		calls := 0
		onSleep = func() {
			// Recovery replaces the token file on its second poll.
			if calls++; calls == 2 {
				_ = os.WriteFile(tokenFile, []byte("fresh\n"), 0o600)
			}
		}
		code, out, errOut := run()
		if code != 0 || !strings.Contains(out, `"opened": true`) {
			t.Fatalf("exit %d: %s %s", code, out, errOut)
		}
		if got := strings.Join(seen, ","); got != "Bearer stale r1,Bearer stale r1,Bearer fresh r1" {
			t.Fatalf("calls %s", got)
		}
		if len(slept) != 2 || slept[0] != 250*time.Millisecond || slept[1] != 500*time.Millisecond {
			t.Fatalf("slept %v", slept)
		}
	})

	t.Run("git remote", func(t *testing.T) {
		// git's first request through the transport, the session for the
		// repository, is the one a push makes during recovery.
		reset("stale")
		onSleep = func() { _ = os.WriteFile(tokenFile, []byte("fresh\n"), 0o600) }
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", []string{"git-remote", "origin", "https://github.com/octo/widgets.git"}, env, strings.NewReader(""), &out, &errOut)
		if code != 0 {
			t.Fatalf("exit %d: %q %s", code, out.String(), errOut.String())
		}
		// stale, then fresh once recovery replaced the token, then the
		// session's end.
		if len(seen) != 3 || !strings.HasPrefix(seen[0], "Bearer stale") || !strings.HasPrefix(seen[1], "Bearer fresh") || len(slept) != 1 {
			t.Fatalf("calls %v, slept %v", seen, slept)
		}
	})

	t.Run("never recovers", func(t *testing.T) {
		reset("stale")
		code, _, errOut := run()
		if code != 1 || !strings.Contains(errOut, "authority_recovering") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		var total time.Duration
		for _, d := range slept {
			total += d
		}
		if total > recoveryBudget || total < recoveryBudget/2 {
			t.Fatalf("waited %s in all, want most of %s and never more", total, recoveryBudget)
		}
	})

	t.Run("other refusals are not retried", func(t *testing.T) {
		reset("stale")
		mu.Lock()
		refuse = `{"error":{"code":"forbidden","message":"execution authority rejected"}}`
		mu.Unlock()
		code, _, errOut := run()
		if code != 1 || !strings.Contains(errOut, "forbidden") || len(seen) != 1 || len(slept) != 0 {
			t.Fatalf("exit %d, %d calls, slept %v: %s", code, len(seen), slept, errOut)
		}
	})
}
