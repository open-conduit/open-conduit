//go:build loadtest

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The load harness (#703) drives the composed daemon on real SQLite with
// fakes only. It sits behind the loadtest build tag and `make load-*`,
// outside make check, because what it produces is a measurement of the host
// it runs on rather than a verdict. Each scenario appends its tables to
// OC_LOAD_REPORT, headed with the host, Go version and commit.

// loadEnv is an integer list from the environment, or def.
func loadEnv(t *testing.T, name string, def ...int) []int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			t.Fatalf("%s: %q", name, f)
		}
		out = append(out, n)
	}
	return out
}

// loadDuration is a duration from the environment, or def.
func loadDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// loadReport writes a scenario's results to the log and OC_LOAD_REPORT.
type loadReport struct {
	t *testing.T
	b strings.Builder
}

func newLoadReport(t *testing.T, title string) *loadReport {
	r := &loadReport{t: t}
	commit, _ := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	cpu, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	load, _ := exec.Command("uptime").Output()
	at := strings.LastIndex(string(load), "load average")
	fmt.Fprintf(&r.b, "\n### %s\n\n%s/%s (%s, %d CPUs), %s, commit %s, without -race", title, runtime.GOOS, runtime.GOARCH, strings.TrimSpace(string(cpu)), runtime.NumCPU(), runtime.Version(), strings.TrimSpace(string(commit)))
	if at >= 0 {
		fmt.Fprintf(&r.b, ", %s", strings.TrimSpace(string(load)[at:]))
	}
	r.b.WriteString("\n\n")
	t.Cleanup(r.flush)
	return r
}

func (r *loadReport) table(head []string, rows [][]string) {
	fmt.Fprintf(&r.b, "| %s |\n|%s\n", strings.Join(head, " | "), strings.Repeat(" --- |", len(head)))
	for _, row := range rows {
		fmt.Fprintf(&r.b, "| %s |\n", strings.Join(row, " | "))
	}
	r.b.WriteString("\n")
}

func (r *loadReport) note(format string, args ...any) { fmt.Fprintf(&r.b, format+"\n\n", args...) }

func (r *loadReport) flush() {
	r.t.Log(r.b.String())
	path := os.Getenv("OC_LOAD_REPORT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Error(err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(r.b.String())
}

// latencies collects durations from any goroutine.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

// spreadOf is the count, p50, p95 and p99 of ds (nearest rank).
func spreadOf(ds []time.Duration) []string {
	if len(ds) == 0 {
		return []string{"0", "-", "-", "-"}
	}
	s := slices.Sorted(slices.Values(ds))
	at := func(q float64) string {
		i := int(q*float64(len(s))+0.999999) - 1
		return roundLatency(s[max(0, min(i, len(s)-1))])
	}
	return []string{strconv.Itoa(len(s)), at(.50), at(.95), at(.99)}
}

func roundLatency(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	}
	return d.Round(time.Microsecond).String()
}

// atRate calls fn(i) for i in [0,n) at a fixed rate, open loop: a slow call
// doesn't delay the next one's start, so queueing in the daemon shows as
// latency rather than a lower offered rate. It returns once all have
// returned, with the time the whole took.
func atRate(n, perSecond int, fn func(i int)) time.Duration {
	start := time.Now()
	interval := time.Second / time.Duration(perSecond)
	var wg sync.WaitGroup
	timer := time.NewTimer(0)
	defer timer.Stop()
	for i := range n {
		<-timer.C
		wg.Add(1)
		go func() { defer wg.Done(); fn(i) }()
		timer.Reset(time.Until(start.Add(time.Duration(i+1) * interval)))
	}
	wg.Wait()
	return time.Since(start)
}

// lifecycleLog is the daemon's lifecycle records, rotated files included,
// oldest first.
func lifecycleLog(t *testing.T, state string) []lifecycle {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(state, "logs", "lifecycle.jsonl*"))
	slices.SortFunc(files, func(a, b string) int {
		// The live file is the newest; rotated ones sort by their suffix.
		switch {
		case strings.HasSuffix(a, ".jsonl"):
			return 1
		case strings.HasSuffix(b, ".jsonl"):
			return -1
		}
		return strings.Compare(a, b)
	})
	var out []lifecycle
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1<<20), 1<<24)
		for s.Scan() {
			var r lifecycle
			if json.Unmarshal(s.Bytes(), &r) == nil {
				out = append(out, r)
			}
		}
		f.Close()
	}
	return out
}

// procSample is one reading of the daemon and the processes under it.
type procSample struct {
	at                            time.Time
	rssKB, treeRSSKB              int64
	procs, goroutines, fds        int
	cpu                           time.Duration
	dbBytes, walBytes, artifactsB int64
}

// sampleDaemon reads the daemon's process, its descendants and its state's
// sizes. Goroutines come from the faultinject build's stats file.
func sampleDaemon(d *drill, stats string) procSample {
	pid := d.cmd.Process.Pid
	s := procSample{at: time.Now()}
	if out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,time=").Output(); err == nil {
		rss, kids, cpu := map[int]int64{}, map[int][]int{}, map[int]time.Duration{}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 4 {
				continue
			}
			p, _ := strconv.Atoi(f[0])
			pp, _ := strconv.Atoi(f[1])
			r, _ := strconv.ParseInt(f[2], 10, 64)
			rss[p], cpu[p] = r, parseCPUTime(f[3])
			kids[pp] = append(kids[pp], p)
		}
		s.rssKB, s.cpu = rss[pid], cpu[pid]
		for todo := []int{pid}; len(todo) > 0; {
			p := todo[0]
			todo = append(todo[1:], kids[p]...)
			s.treeRSSKB += rss[p]
			s.procs++
		}
	}
	if out, err := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(pid), "-F", "f").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "f") {
				s.fds++
			}
		}
	}
	if b, err := os.ReadFile(stats); err == nil {
		s.goroutines, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	s.dbBytes, s.walBytes = fileSize(filepath.Join(d.state, "conduit.db")), fileSize(filepath.Join(d.state, "conduit.db-wal"))
	s.artifactsB = treeBytes(filepath.Join(d.state, "artifacts"))
	return s
}

// parseCPUTime reads ps's [[dd-]hh:]mm:ss.cc.
func parseCPUTime(v string) time.Duration {
	var days int
	if head, tail, ok := strings.Cut(v, "-"); ok {
		days, _ = strconv.Atoi(head)
		v = tail
	}
	var total float64
	for _, part := range strings.Split(v, ":") {
		n, _ := strconv.ParseFloat(strings.ReplaceAll(part, ",", "."), 64)
		total = total*60 + n
	}
	return time.Duration((total + float64(days)*86400) * float64(time.Second))
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func treeBytes(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// slopePerHour is the least-squares slope of ys over the samples' times.
func slopePerHour(at []time.Time, ys []float64) float64 {
	if len(at) < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range at {
		x := at[i].Sub(at[0]).Hours()
		sx, sy, sxx, sxy = sx+x, sy+ys[i], sxx+x*x, sxy+x*ys[i]
	}
	n := float64(len(at))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}
