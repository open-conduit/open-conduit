package claudecode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// fakeClaude stands in for the Claude Code CLI. It records how it was invoked,
// echoes each stream-json user message back as a replay (as
// --replay-user-messages does), writes the session transcript where claude
// keeps it, and ends with a result whose totals are cumulative for the session
// (0.50 USD, 10 input and 100 cache-read tokens per turn so far). The mode
// file selects review, finished, limit, error or wait (block until released).
const fakeClaude = `#!/bin/sh
[ "$1" = --version ] && { echo "9.9.9 (Claude Code)"; exit 0; }
session=""; resume=0; mcp=""; model=""
while [ $# -gt 0 ]; do
  case "$1" in
    --model) model=$2; shift ;;
    --session-id) session=$2; shift ;;
    --resume) session=$2; resume=1; shift ;;
    --mcp-config) mcp=$2; shift ;;
  esac
  shift
done
f=${FAKE_DIR:?}
mode=$(cat "$f/mode" 2>/dev/null || echo review)
printf 'session=%s resume=%s mcp=%s\n' "$session" "$resume" "$mcp" >> "$f/invocations"
printf '%s\n' "$model" >> "$f/models"
env | grep -E '^(ANTHROPIC_API_KEY|CLAUDECODE|CLAUDE_CODE_ENTRYPOINT|OS_[A-Z_]+|UNRELATED)=' | sort > "$f/env"
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$t"
echo '{"type":"system","subtype":"init"}'
want=${FAKE_MESSAGES:-1}
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$f/stdin"
  content=$(printf '%s' "$line" | sed -E 's/^\{"message":\{"content":"(.*)","role":"user"\},"type":"user"\}$/\1/')
  printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
  [ "$n" -eq 1 ] && : > "$f/started"
  [ "$n" -ge "$want" ] && break
done
if [ "$mode" = wait ]; then
  echo $$ > "$f/pid"
  while [ ! -e "$f/release" ]; do sleep 0.05; done
  mode=review
fi
echo '{"type":"user","timestamp":"2026-10-25T10:00:00Z"}' >> "$t/$session.jsonl"
turns=$(wc -l < "$t/$session.jsonl" | tr -d ' ')
cost=$((turns * 50))
case "$mode" in
  error) echo "boom" >&2; exit 1 ;;
  limit)
    echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":'"${FAKE_RESET:-4102444800}"',"rateLimitType":"five_hour"}}'
    echo '{"type":"result","subtype":"success","is_error":true,"api_error":"usage_limit_reached","num_turns":1,"total_cost_usd":0,"session_id":"'"$session"'","result":"You have hit the limit"}'
    exit 1 ;;
  finished) state=finished ;;
  *) state=review ;;
esac
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}'
printf '{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"%s","num_turns":2,"total_cost_usd":%s.%02d,"structured_output":{"state":"%s","summary":"turn %s","prs":[7],"handoff":"note %s"},"modelUsage":{"claude-x":{"inputTokens":%s,"outputTokens":%s,"cacheReadInputTokens":%s,"cacheCreationInputTokens":%s}}}\n' "$session" $((cost/100)) $((cost%100)) "$state" "$turns" "$turns" $((turns*10)) $((turns*5)) $((turns*100)) $((turns*3))
`

// memStore is a small serializable public-contract store: namespaced KV plus
// Work lookups, which is all the executor's continuity uses.
type memStore struct {
	mu    sync.Mutex
	kv    map[string][]byte
	works map[domain.ID]domain.Work
}

func newMemStore() *memStore {
	return &memStore{kv: map[string][]byte{}, works: map[domain.ID]domain.Work{}}
}

type memTx struct {
	storage.Tx
	kv    map[string][]byte
	works map[domain.ID]domain.Work
}

func (s *memStore) Transact(ctx context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &memTx{kv: map[string][]byte{}, works: s.works}
	for k, v := range s.kv {
		tx.kv[k] = v
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.kv = tx.kv
	return nil
}

func (s *memStore) setWork(w domain.Work) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.works[w.ID] = w
}

func (s *memStore) record(t *testing.T, work string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{}
	if b, ok := s.kv["executor.claudecode\x00work/"+work]; ok {
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (s *memStore) edit(t *testing.T, work string, fn func(map[string]any)) {
	t.Helper()
	m := s.record(t, work)
	fn(m)
	b, _ := json.Marshal(m)
	s.mu.Lock()
	s.kv["executor.claudecode\x00work/"+work] = b
	s.mu.Unlock()
}

func (tx *memTx) Get(ns, key string) ([]byte, error) {
	if v, ok := tx.kv[ns+"\x00"+key]; ok {
		return append([]byte(nil), v...), nil
	}
	return nil, storage.ErrNotFound
}
func (tx *memTx) Put(ns, key string, v []byte) error {
	tx.kv[ns+"\x00"+key] = append([]byte(nil), v...)
	return nil
}
func (tx *memTx) Delete(ns, key string) error {
	delete(tx.kv, ns+"\x00"+key)
	return nil
}
func (tx *memTx) List(ns, prefix string) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range tx.kv {
		if strings.HasPrefix(k, ns+"\x00"+prefix) {
			out = append(out, storage.Entry{Key: strings.TrimPrefix(k, ns+"\x00"), Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (tx *memTx) Work(id domain.ID) (domain.Work, error) {
	if w, ok := tx.works[id]; ok {
		return w, nil
	}
	return domain.Work{}, storage.ErrNotFound
}

// memoryArtifacts is a minimal public-contract artifact store.
type memoryArtifacts struct {
	mu   sync.Mutex
	n    int
	data map[string][]byte
}
type memoryWriter struct {
	s     *memoryArtifacts
	media string
	buf   bytes.Buffer
}

func (w *memoryWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memoryWriter) Close() error                { return nil }
func (w *memoryWriter) Commit(context.Context) (storage.ArtifactRef, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.n++
	id := fmt.Sprintf("artifact-%d", w.s.n)
	w.s.data[id] = append([]byte(nil), w.buf.Bytes()...)
	return storage.ArtifactRef{ID: id, Size: int64(w.buf.Len()), MediaType: w.media}, nil
}
func (s *memoryArtifacts) Create(_ context.Context, media string) (storage.ArtifactWriter, error) {
	return &memoryWriter{s: s, media: media}, nil
}
func (s *memoryArtifacts) Open(_ context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[ref.ID]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s *memoryArtifacts) Delete(context.Context, storage.ArtifactRef) error { return nil }

// clock is a settable wall clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// reports collects turn telemetry.
type reports struct {
	mu  sync.Mutex
	all []report
}
type report struct {
	name  string
	attrs map[string]any
}

func (r *reports) add(name string, attrs map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all, report{name, attrs})
}
func (r *reports) named(name string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, x := range r.all {
		if x.name == name {
			out = append(out, x.attrs)
		}
	}
	return out
}

// harness is one test's executor environment.
type harness struct {
	t       *testing.T
	root    string
	fake    string
	binary  string
	config  string
	state   string
	store   *memStore
	art     *memoryArtifacts
	clock   *clock
	reports *reports
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root, err := os.MkdirTemp("", "claude-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	h := &harness{t: t, root: root, fake: filepath.Join(root, "fake"), binary: filepath.Join(root, "claude"), config: filepath.Join(root, "claude-config"), state: filepath.Join(root, "state"),
		store: newMemStore(), art: &memoryArtifacts{data: map[string][]byte{}}, clock: &clock{now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}, reports: &reports{}}
	for _, d := range []string{h.fake, h.config} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(h.binary, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) executor() *claudecode.Executor {
	h.t.Helper()
	bin := filepath.Join(h.root, "bin")
	_ = os.MkdirAll(bin, 0o700)
	e, err := claudecode.New(claudecode.Options{
		StateDir: h.state, Store: h.store, Artifacts: h.art, ToolbeltDir: bin,
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.root, "CLAUDE_CONFIG_DIR=" + h.config, "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "ANTHROPIC_API_KEY=ambient", "UNRELATED=x"}
		},
		Report: h.reports.add, Now: h.clock.Now,
		Grace: 300 * time.Millisecond, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *harness) mode(m string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.fake, "mode"), []byte(m), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) bindingConfig(extra map[string]any) json.RawMessage {
	cfg := map[string]any{"binary": h.binary, "env": map[string]string{"FAKE_DIR": h.fake}, "status_schema": "github.status.v1"}
	for k, v := range extra {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	return b
}

// request builds a run request for a Work and a fresh execution.
func (h *harness) request(work, execution string, epoch uint64, extra map[string]any, inputs ...domain.Event) (extension.RunRequest, *domain.Payload) {
	w := domain.Work{ID: domain.ID(work), ScopeID: "eng", Kind: "github.issue", State: domain.WorkRunning, CorrelationKeys: []domain.CorrelationKey{{Source: "github/x", ExternalID: "issue/12"}}}
	h.store.setWork(w)
	saved := &domain.Payload{}
	if len(inputs) == 0 {
		inputs = []domain.Event{{ID: domain.ID("ev-" + execution), Type: "github.comment.human", Data: domain.Payload{Schema: "github.comment.v1", Data: json.RawMessage(`{"author":"alice","body":"please fix","kind":"issue","number":12}`)}}}
	}
	r := extension.RunRequest{
		Work:      w,
		Execution: domain.Execution{ID: domain.ID(execution), WorkID: w.ID, Attempt: 1, Lease: domain.Lease{Owner: "test", Epoch: epoch}},
		Trigger:   inputs[len(inputs)-1],
		Inputs:    inputs,
		Context:   extension.ExecutionContext{Config: domain.Payload{Data: h.bindingConfig(extra)}},
	}
	r.PersistRecovery = func(p domain.Payload) error { *saved = p; return nil }
	return r, saved
}

func (h *harness) run(e *claudecode.Executor, r extension.RunRequest) extension.Result {
	h.t.Helper()
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.wait(handle)
}

func (h *harness) wait(handle extension.RunHandle) extension.Result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := handle.Wait(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := extension.ValidateResult(res); err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) invocations() []string {
	b, _ := os.ReadFile(filepath.Join(h.fake, "invocations"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// prompts returns the decoded user messages claude received.
func (h *harness) prompts() []string {
	b, _ := os.ReadFile(filepath.Join(h.fake, "stdin"))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m.Message.Content)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type outcomeData struct {
	Status     string             `json:"status"`
	Reason     string             `json:"reason"`
	PRs        []int              `json:"prs"`
	Kind       string             `json:"kind"`
	Models     []string           `json:"models"`
	ModelCalls int                `json:"model_calls"`
	ToolCalls  int                `json:"tool_calls"`
	Tokens     *claudecode.Tokens `json:"tokens"`
	Limited    bool               `json:"limited"`
	ResetsAt   *time.Time         `json:"resets_at"`
	Transcript string             `json:"transcript"`
	Log        string             `json:"log"`
	Injected   *struct {
		Accepted, Consumed int
	} `json:"injected"`
}

func dataOf(t *testing.T, res extension.Result) outcomeData {
	t.Helper()
	var d outcomeData
	if err := json.Unmarshal(res.Outcome.Data.Data, &d); err != nil {
		t.Fatalf("outcome data: %v %s", err, res.Outcome.Data.Data)
	}
	return d
}
