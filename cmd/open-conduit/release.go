package main

import (
	"context"
	"sync"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
	"github.com/open-conduit/open-conduit/internal/extproc"
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

// packageVerifier is install's signature check for settings.extension_trust
// (#709): a package archive's bundle must verify as the publisher the
// policy names, against Sigstore's public-good root, fetched once per run
// and only when a signature is checked.
func packageVerifier(trusted func(context.Context) (root.TrustedMaterial, error)) extproc.SignatureCheck {
	var mu sync.Mutex
	var tm root.TrustedMaterial
	return func(ctx context.Context, bundle, artifact []byte, want extproc.Signer) (extproc.Signer, error) {
		mu.Lock()
		if tm == nil {
			t, err := trusted(ctx)
			if err != nil {
				mu.Unlock()
				return extproc.Signer{}, err
			}
			tm = t
		}
		mu.Unlock()
		got, err := provenance.Signer(tm, bundle, artifact, provenance.Identity{Subject: want.Subject, SubjectRegexp: want.SubjectRegexp, Issuer: want.Issuer})
		if err != nil {
			return extproc.Signer{}, err
		}
		return extproc.Signer{Subject: got.Subject, Issuer: got.Issuer}, nil
	}
}
