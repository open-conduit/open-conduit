package extproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// oneArtifact is a store holding one artifact. Open matches a ref without
// a digest on its other fields, as the contract requires of a store.
type oneArtifact struct {
	ref  domain.ArtifactRef
	data string
}

func (a oneArtifact) Create(context.Context, string) (storage.ArtifactWriter, error) {
	return nil, storage.ErrReadOnly
}
func (a oneArtifact) Delete(context.Context, storage.ArtifactRef) error { return storage.ErrReadOnly }
func (a oneArtifact) Open(_ context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	if ref.ID != a.ref.ID {
		return nil, storage.ErrNotFound
	}
	if ref.SHA256 == "" {
		ref.SHA256 = a.ref.SHA256
	}
	if ref != a.ref {
		return nil, storage.ErrInvalid
	}
	return io.NopCloser(strings.NewReader(a.data)), nil
}

// TestResultArtifactsGetTheirDigest: a ref an extension built on an older
// SDK returns without its digest is recorded with the digest of the stored
// content; one that names no stored content is refused; a ref that has its
// digest is passed through untouched.
func TestResultArtifactsGetTheirDigest(t *testing.T) {
	ctx := context.Background()
	sum := sha256.Sum256([]byte("transcript"))
	full := domain.ArtifactRef{ID: "artifact", Size: 10, MediaType: "text/plain", SHA256: hex.EncodeToString(sum[:])}
	s := &Service{deps: Deps{Artifacts: oneArtifact{full, "transcript"}}}
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
