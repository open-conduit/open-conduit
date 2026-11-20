package extproc

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
)

// TestResultArtifactsGetTheirDigest: a ref an extension built on an older
// SDK returns without its digest is recorded with the digest of the stored
// content; one that names no stored content is refused; a ref that has its
// digest is passed through untouched.
func TestResultArtifactsGetTheirDigest(t *testing.T) {
	ctx := context.Background()
	store, err := artifacts.Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := store.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "transcript")
	full, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{deps: Deps{Artifacts: store}}
	bare := full
	bare.SHA256 = ""
	in := []domain.ArtifactRef{bare}
	got, err := s.completeArtifacts(ctx, in)
	if err != nil || len(got) != 1 || got[0] != full {
		t.Fatalf("completed %+v, %v; want %+v", got, err, full)
	}
	if in[0].SHA256 != "" {
		t.Fatal("the result's own slice was changed")
	}
	if got, err = s.completeArtifacts(ctx, []domain.ArtifactRef{full}); err != nil || got[0] != full {
		t.Fatalf("a ref with its digest became %+v, %v", got, err)
	}
	missing := bare
	missing.Size++
	if _, err = s.completeArtifacts(ctx, []domain.ArtifactRef{missing}); err == nil {
		t.Fatal("a ref naming no stored content was accepted")
	}
	if _, err = (&Service{}).completeArtifacts(ctx, []domain.ArtifactRef{bare}); err == nil {
		t.Fatal("a ref without a digest was accepted with no store to check it")
	}
}
