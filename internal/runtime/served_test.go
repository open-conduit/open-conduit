package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// memArtifacts is an in-memory storage.Artifacts; failOpen makes Open fail
// for that ID, failCreate makes every Create fail.
type memArtifacts struct {
	mu         sync.Mutex
	content    map[string][]byte
	deleted    []string
	failOpen   string
	failCreate bool
}

type memWriter struct {
	a         *memArtifacts
	mediaType string
	buf       bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memWriter) Close() error                { return nil }
func (w *memWriter) Commit(context.Context) (storage.ArtifactRef, error) {
	sum := sha256.Sum256(w.buf.Bytes())
	id := hex.EncodeToString(sum[:])
	w.a.mu.Lock()
	w.a.content[id] = append([]byte(nil), w.buf.Bytes()...)
	w.a.mu.Unlock()
	return storage.ArtifactRef{ID: id, Size: int64(w.buf.Len()), MediaType: w.mediaType}, nil
}

func (a *memArtifacts) Create(_ context.Context, mediaType string) (storage.ArtifactWriter, error) {
	if a.failCreate {
		return nil, errors.New("disk full")
	}
	return &memWriter{a: a, mediaType: mediaType}, nil
}

func (a *memArtifacts) Open(_ context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.content[ref.ID]
	if !ok || ref.ID == a.failOpen {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (a *memArtifacts) Delete(_ context.Context, ref storage.ArtifactRef) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.content, ref.ID)
	a.deleted = append(a.deleted, ref.ID)
	return nil
}

func (a *memArtifacts) put(t *testing.T, text string) domain.ArtifactRef {
	t.Helper()
	w, _ := a.Create(context.Background(), "text/plain")
	w.Write([]byte(text))
	ref, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (a *memArtifacts) read(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return string(a.content[id])
}

// The record is per execution, longest value first, and forgets only
// executions no longer held.
func TestServedSecretsRecord(t *testing.T) {
	var s ServedSecrets
	s.Add("x/1", []string{"abc", "abcdef", ""})
	s.Add("x/2", []string{"other"})
	if got := strings.Join(s.Values("x/1"), ","); got != "abcdef,abc" {
		t.Fatalf("values = %q", got)
	}
	if got := len(s.All()); got != 3 {
		t.Fatalf("all = %d values", got)
	}
	if err := s.Reconcile(func([]domain.ID) (map[domain.ID]bool, error) { return nil, errors.New("down") }); err == nil || len(s.Values("x/1")) == 0 {
		t.Fatal("a failed holding check forgot values")
	}
	if err := s.Reconcile(func([]domain.ID) (map[domain.ID]bool, error) { return map[domain.ID]bool{"x/2": true}, nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.Values("x/1")) != 0 || len(s.Values("x/2")) != 1 {
		t.Fatalf("after reconcile: %q %q", s.Values("x/1"), s.Values("x/2"))
	}
}

// scrubResult replaces the values in the outcome and artifacts, renames a
// scrubbed artifact in the outcome, deletes the plaintext, keeps an
// artifact without a value as it is, and drops (and deletes) one it can't
// read or rewrite. With no values the result is untouched.
func TestScrubResult(t *testing.T) {
	ctx := context.Background()
	a := &memArtifacts{content: map[string][]byte{}}
	secret := "s3cr3t-value"
	leaky := a.put(t, "printed "+secret+" here")
	clean := a.put(t, "nothing to see")
	unreadable := a.put(t, "gone "+secret)
	a.failOpen = unreadable.ID
	in := extension.Result{
		Outcome:   domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "said " + secret, Data: domain.Payload{Schema: "x.v1", Data: []byte(`{"log":"` + leaky.ID + `","note":"` + secret + `"}`)}},
		Artifacts: []domain.ArtifactRef{leaky, clean, unreadable},
	}
	if out := scrubResult(ctx, a, in, nil); out.Outcome.Summary != in.Outcome.Summary || len(out.Artifacts) != 3 {
		t.Fatalf("no values changed the result: %+v", out)
	}
	out := scrubResult(ctx, a, in, []string{secret})
	if strings.Contains(out.Outcome.Summary, secret) || strings.Contains(string(out.Outcome.Data.Data), secret) {
		t.Fatalf("outcome kept the secret: %+v", out.Outcome)
	}
	if len(out.Artifacts) != 2 || out.Artifacts[1] != clean {
		t.Fatalf("artifacts = %+v", out.Artifacts)
	}
	scrubbed := out.Artifacts[0]
	if scrubbed.ID == leaky.ID || a.read(scrubbed.ID) != "printed "+Redacted+" here" {
		t.Fatalf("scrubbed copy %+v = %q", scrubbed, a.read(scrubbed.ID))
	}
	if !strings.Contains(string(out.Outcome.Data.Data), scrubbed.ID) || strings.Contains(string(out.Outcome.Data.Data), leaky.ID) {
		t.Fatalf("outcome names %s", out.Outcome.Data.Data)
	}
	if a.read(leaky.ID) != "" || !strings.Contains(strings.Join(a.deleted, ","), unreadable.ID) {
		t.Fatalf("plaintext kept: deleted %q", a.deleted)
	}
	// An artifact that can't be rewritten is not published.
	b := &memArtifacts{content: map[string][]byte{}}
	ref := b.put(t, secret)
	b.failCreate = true
	if out := scrubResult(ctx, b, extension.Result{Artifacts: []domain.ArtifactRef{ref}}, []string{secret}); len(out.Artifacts) != 0 {
		t.Fatalf("an unscrubbed artifact was published: %+v", out.Artifacts)
	}
	// Without an artifact store nothing can be checked, so nothing is published.
	if out := scrubResult(ctx, nil, extension.Result{Artifacts: []domain.ArtifactRef{clean}}, []string{secret}); len(out.Artifacts) != 0 {
		t.Fatalf("published unchecked: %+v", out.Artifacts)
	}
	// Live output is scrubbed in place.
	live := ScrubLive([]extension.LiveOutput{{Name: "activity.txt", Content: []byte("x " + secret)}}, []string{secret})
	if string(live[0].Content) != "x "+Redacted {
		t.Fatalf("live = %q", live[0].Content)
	}
}
