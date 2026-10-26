package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/prototypes/transport/goplugin"
	"github.com/alsiola/open-conduit/prototypes/transport/jsonrpc"
	"github.com/alsiola/open-conduit/prototypes/transport/scenario"
)

var binaries = map[string]string{}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "oc-transport-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"jsonrpc-ext", "goplugin-ext"} {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/"+name)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
		binaries[name] = out
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type transport struct {
	name, binary string
	start        scenario.Starter
}

func transports() []transport {
	return []transport{
		{"jsonrpc-stdio", binaries["jsonrpc-ext"], jsonrpc.Start},
		{"goplugin-grpc", binaries["goplugin-ext"], goplugin.Start},
	}
}

// TestTransportCompatibility runs the same semantic checks and measurements
// over both candidate transports and logs a comparison table (run with -v).
func TestTransportCompatibility(t *testing.T) {
	var rows [][]string
	for _, tr := range transports() {
		t.Run(tr.name, func(t *testing.T) {
			rows = append(rows, exercise(t, tr))
		})
	}
	header := []string{"transport", "cold start", "warm restart", "version mismatch", "echo p50", "echo p99", "host callback", "artifact 16 MiB/64 KiB", "cancel seen by ext", "crash detected", "binary", "deps"}
	var b strings.Builder
	fmt.Fprintf(&b, "\n| %s |\n|%s\n", strings.Join(header, " | "), strings.Repeat(" --- |", len(header)))
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s |\n", strings.Join(r, " | "))
	}
	t.Log(b.String())
}

func exercise(t *testing.T, tr transport) []string {
	ctx := context.Background()
	stats := &scenario.HostStats{}
	host := scenario.Host(stats)

	began := time.Now()
	p, err := tr.start(ctx, tr.binary, nil, []int{1}, host)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	startup := time.Since(began)
	var m scenario.Manifest
	if err := json.Unmarshal(p.Manifest(), &m); err != nil || m.Protocol != 1 || m.Name == "" {
		t.Fatalf("manifest %s: %v", p.Manifest(), err)
	}

	// Version mismatch fails deterministically at the handshake.
	began = time.Now()
	if q, err := tr.start(ctx, tr.binary, []string{"PROTO_VERSIONS=2"}, []int{1}, host); !errors.Is(err, scenario.ErrIncompatible) {
		if q != nil {
			_ = q.Close()
		}
		t.Fatalf("version mismatch: %v", err)
	}
	mismatch := time.Since(began)

	// Unary latency.
	lat := make([]time.Duration, 2000)
	for i := range lat {
		s := time.Now()
		raw, err := p.Call(ctx, "echo", map[string]any{"i": i, "pad": strings.Repeat("x", 200)})
		if err != nil || !bytes.Contains(raw, []byte(fmt.Sprintf(`"i":%d`, i))) {
			t.Fatalf("echo %d: %s %v", i, raw, err)
		}
		lat[i] = time.Since(s)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	// Structured errors cross verbatim; unknown methods have their own code.
	var se *scenario.Error
	if _, err := p.Call(ctx, "fail", nil); !errors.As(err, &se) || se.Code != "not_found" || se.Message == "" {
		t.Fatalf("structured error: %v", err)
	}
	if _, err := p.Call(ctx, "nope", nil); !errors.As(err, &se) || se.Code != "method_not_found" {
		t.Fatalf("unknown method: %v", err)
	}

	// Concurrent calls are matched to their responses.
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				want := fmt.Sprintf(`"g":%d,"i":%d`, g, i)
				raw, err := p.Call(ctx, "echo", json.RawMessage(fmt.Sprintf(`{%s}`, want)))
				if err != nil || !bytes.Contains(raw, []byte(want)) {
					errs <- fmt.Errorf("concurrent echo %s: %s %v", want, raw, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Extension-to-host callbacks while serving a host request.
	s := time.Now()
	raw, err := p.Call(ctx, "kv", map[string]int{"n": 1000})
	if err != nil || string(raw) != fmt.Sprint(999*1000) || stats.KVGets.Load() != 1000 {
		t.Fatalf("kv: %s %v (%d gets)", raw, err, stats.KVGets.Load())
	}
	callback := time.Since(s) / 1000

	// Chunked artifact upload acknowledged per chunk.
	const total = 16 << 20
	s = time.Now()
	if raw, err = p.Call(ctx, "artifact", map[string]int{"bytes": total, "chunk": 64 << 10}); err != nil || string(raw) != fmt.Sprint(total) || stats.ArtifactBytes.Load() != total {
		t.Fatalf("artifact: %s %v", raw, err)
	}
	mbps := float64(total) / (1 << 20) / time.Since(s).Seconds()

	// Stray stdout from extension code leaves the stream intact.
	if raw, err = p.Call(ctx, "print", nil); err != nil || string(raw) != `"printed"` {
		t.Fatalf("print: %s %v", raw, err)
	}
	if _, err = p.Call(ctx, "echo", 1); err != nil {
		t.Fatalf("echo after stray output: %v", err)
	}

	// Extension-initiated delivery answered by the host.
	if raw, err = p.Call(ctx, "push", nil); err != nil || !bytes.Contains(raw, []byte(`"accepted":true`)) {
		t.Fatalf("push: %s %v", raw, err)
	}

	// Cancellation returns promptly and reaches the extension handler.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := p.Call(cctx, "sleep", map[string]int{"ms": 10000}); done <- err }()
	time.Sleep(100 * time.Millisecond)
	cancelled := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	var seen time.Duration
	for deadline := time.Now().Add(2 * time.Second); ; {
		raw, err := p.Call(ctx, "stats", nil)
		var st struct {
			CancelledAt int64 `json:"cancelled_at"`
		}
		if err == nil && json.Unmarshal(raw, &st) == nil && st.CancelledAt >= cancelled.UnixNano() {
			seen = time.Unix(0, st.CancelledAt).Sub(cancelled)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extension never observed the cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A crash mid-call is a transport error, not a hang or an authored error;
	// later calls fail the same way and a restarted process serves again.
	s = time.Now()
	if _, err := p.Call(ctx, "crash", nil); !errors.Is(err, scenario.ErrTransportClosed) {
		t.Fatalf("crash: %v", err)
	}
	crash := time.Since(s)
	time.Sleep(50 * time.Millisecond)
	if _, err := p.Call(ctx, "echo", 1); !errors.Is(err, scenario.ErrTransportClosed) {
		t.Fatalf("after crash: %v", err)
	}
	_ = p.Close()
	s = time.Now()
	p, err = tr.start(ctx, tr.binary, nil, []int{1}, host)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	restart := time.Since(s)
	if _, err := p.Call(ctx, "echo", 1); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	_ = p.Close()

	info, _ := os.Stat(tr.binary)
	out, _ := exec.Command("go", "version", "-m", tr.binary).Output()
	deps := strings.Count(string(out), "\tdep\t")
	return []string{tr.name, round(startup), round(restart), round(mismatch), round(lat[len(lat)/2]), round(lat[len(lat)*99/100]), round(callback),
		fmt.Sprintf("%.0f MiB/s", mbps), round(seen), round(crash), fmt.Sprintf("%.1f MiB", float64(info.Size())/(1<<20)), fmt.Sprint(deps)}
}

func round(d time.Duration) string {
	switch {
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}
