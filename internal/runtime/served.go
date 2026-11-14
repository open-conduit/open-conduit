package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Redacted replaces a served secret wherever an execution publishes it.
const Redacted = "[REDACTED]"

// ServedSecrets remembers, in memory only, the secret values the toolbelt
// served each execution through operations with a secret output
// (extension.Operation.SecretOutput), so that what the execution publishes
// can be scrubbed of them. Values are never stored, so a daemon restart
// forgets them: a process adopted after a restart publishes unscrubbed
// what it printed before. The zero value is ready to use.
type ServedSecrets struct {
	mu     sync.Mutex
	values map[domain.ID]map[string]bool
}

// Add records values as served to execution.
func (s *ServedSecrets) Add(execution domain.ID, values []string) {
	if len(values) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[domain.ID]map[string]bool{}
	}
	set := s.values[execution]
	if set == nil {
		set = map[string]bool{}
		s.values[execution] = set
	}
	for _, v := range values {
		if v != "" {
			set[v] = true
		}
	}
}

// Values are the secrets served to execution, longest first, so a value
// that contains another is replaced whole.
func (s *ServedSecrets) Values(execution domain.ID) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedValues(s.values[execution])
}

// All are the secrets served to every execution still remembered, for
// redaction where the execution is not known (telemetry).
func (s *ServedSecrets) All() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := map[string]bool{}
	for _, set := range s.values {
		for v := range set {
			all[v] = true
		}
	}
	return sortedValues(all)
}

// Reconcile forgets the secrets of every execution holding no longer
// reports as held (Scheduler.Holding): one that is terminal with its stop
// confirmed, after which nothing it publishes is accepted. A parked
// execution is still held, so its values outlive the park.
func (s *ServedSecrets) Reconcile(holding func([]domain.ID) (map[domain.ID]bool, error)) error {
	s.mu.Lock()
	ids := make([]domain.ID, 0, len(s.values))
	for id := range s.values {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	held, err := holding(ids)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if !held[id] {
			delete(s.values, id)
		}
	}
	return nil
}

func sortedValues(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// ScrubString replaces each of values in s with Redacted.
func ScrubString(s string, values []string) string {
	for _, v := range values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	return s
}

func scrubBytes(b []byte, values []string) ([]byte, bool) {
	changed := false
	for _, v := range values {
		if bytes.Contains(b, []byte(v)) {
			b = bytes.ReplaceAll(b, []byte(v), []byte(Redacted))
			changed = true
		}
	}
	return b, changed
}

// ScrubLive scrubs values from a running attempt's live output.
func ScrubLive(out []extension.LiveOutput, values []string) []extension.LiveOutput {
	if len(values) == 0 {
		return out
	}
	for i := range out {
		if b, changed := scrubBytes(out[i].Content, values); changed {
			out[i].Content = b
		}
	}
	return out
}

// scrubbedHandle scrubs the secrets served to its execution from the
// result it gives, before anything records it.
type scrubbedHandle struct {
	extension.RunHandle
	scrub func(context.Context, extension.Result) extension.Result
}

func (h scrubbedHandle) Wait(ctx context.Context) (extension.Result, error) {
	r, err := h.RunHandle.Wait(ctx)
	if err != nil {
		return r, err
	}
	return h.scrub(ctx, r), nil
}

// scrubResult replaces each of values in r's outcome and artifacts. An
// artifact that holds one is replaced by a scrubbed copy, and the original
// deleted. One that cannot be read or rewritten is dropped from the result
// (and deleted), so nothing unscrubbed is published.
func scrubResult(ctx context.Context, artifacts storage.Artifacts, r extension.Result, values []string) extension.Result {
	if len(values) == 0 {
		return r
	}
	r.Outcome.Summary = ScrubString(r.Outcome.Summary, values)
	if b, changed := scrubBytes(r.Outcome.Data.Data, values); changed && json.Valid(b) {
		r.Outcome.Data.Data = b
	} else if changed {
		r.Outcome.Data.Data = nil
	}
	if r.Workspace != nil {
		r.Workspace.Summary = ScrubString(r.Workspace.Summary, values)
		r.Workspace.Dir = ScrubString(r.Workspace.Dir, values)
	}
	if len(r.Artifacts) == 0 {
		return r
	}
	refs := make([]domain.ArtifactRef, 0, len(r.Artifacts))
	data := r.Outcome.Data.Data
	for _, ref := range r.Artifacts {
		if artifacts == nil {
			continue // cannot check it, so it is not published
		}
		scrubbed, ok := scrubArtifact(ctx, artifacts, ref, values)
		if !ok {
			continue
		}
		refs = append(refs, scrubbed)
		if scrubbed.ID != ref.ID && len(data) > 0 {
			// An outcome that names its artifacts (the shell executor's
			// outputs, say) names the scrubbed copy.
			data = bytes.ReplaceAll(data, []byte(ref.ID), []byte(scrubbed.ID))
		}
	}
	r.Artifacts = refs
	r.Outcome.Data.Data = data
	return r
}

func scrubArtifact(ctx context.Context, artifacts storage.Artifacts, ref domain.ArtifactRef, values []string) (domain.ArtifactRef, bool) {
	drop := func() (domain.ArtifactRef, bool) {
		_ = artifacts.Delete(context.WithoutCancel(ctx), ref)
		return domain.ArtifactRef{}, false
	}
	rc, err := artifacts.Open(ctx, ref)
	if err != nil {
		return drop()
	}
	content, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return drop()
	}
	content, changed := scrubBytes(content, values)
	if !changed {
		return ref, true
	}
	w, err := artifacts.Create(ctx, ref.MediaType)
	if err != nil {
		return drop()
	}
	if _, err = w.Write(content); err != nil {
		w.Close()
		return drop()
	}
	scrubbed, err := w.Commit(ctx)
	if err != nil {
		return drop()
	}
	_ = artifacts.Delete(context.WithoutCancel(ctx), ref)
	return scrubbed, true
}
