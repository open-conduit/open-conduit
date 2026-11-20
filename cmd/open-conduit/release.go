package main

import (
	"context"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
	"github.com/open-conduit/open-conduit/internal/provenance"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// releaseVerifier is the updater's signature check: the bundle must be the
// release workflow's signature over checksums.txt, against the trusted
// root trusted gives.
func releaseVerifier(trusted func(context.Context) (root.TrustedMaterial, error)) func(context.Context, []byte, []byte) error {
	signer := provenance.Identity{Subject: upgrade.SignerSubject, Issuer: upgrade.SignerIssuer}
	return func(ctx context.Context, bundle, checksums []byte) error {
		tm, err := trusted(ctx)
		if err != nil {
			return err
		}
		return provenance.Verify(tm, bundle, checksums, signer)
	}
}
