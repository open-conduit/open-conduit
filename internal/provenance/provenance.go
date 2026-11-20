// Package provenance checks that an artifact was signed by a particular
// Sigstore identity: a keyless signature bundle (cosign's
// .sigstore.json), its Fulcio certificate and its Rekor transparency-log
// entry, verified in process against a trusted root. The release updater
// uses it to check a release's checksums.txt; it names no artifact or
// publisher of its own, so any caller that knows whom it trusts can use it.
package provenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// Identity is who must have signed: the certificate's subject (for a
// GitHub Actions workflow, its URL at a ref) and the OIDC issuer that
// vouched for it. Subject and Issuer are matched exactly; SubjectRegexp,
// in place of Subject, matches the whole subject against a regular
// expression (cosign's --certificate-identity-regexp), for a publisher
// whose workflow signs at each release tag.
type Identity struct {
	Subject       string
	SubjectRegexp string
	Issuer        string
}

// GitHubActions is the OIDC issuer of GitHub Actions workflows.
const GitHubActions = "https://token.actions.githubusercontent.com"

// Verify checks that bundleJSON is a Sigstore bundle signing artifact,
// made by id, with a certificate that chains to trusted, and recorded in a
// transparency log trusted names at a time the certificate was valid. Any
// failure is an error; nothing is trusted on a partial match.
func Verify(trusted root.TrustedMaterial, bundleJSON, artifact []byte, id Identity) error {
	_, err := Signer(trusted, bundleJSON, artifact, id)
	return err
}

// Signer is Verify that also returns who signed: the certificate's exact
// subject and issuer, which id may have matched by regular expression.
func Signer(trusted root.TrustedMaterial, bundleJSON, artifact []byte, id Identity) (Identity, error) {
	if trusted == nil {
		return Identity{}, errors.New("no trusted root to verify against")
	}
	if (id.Subject == "") == (id.SubjectRegexp == "") || id.Issuer == "" {
		return Identity{}, errors.New("no signer identity to verify against")
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return Identity{}, fmt.Errorf("reading the signature bundle: %w", err)
	}
	return verifyEntity(trusted, &b, artifact, id)
}

// verifyEntity is Signer on an entity already read.
func verifyEntity(trusted root.TrustedMaterial, entity verify.SignedEntity, artifact []byte, id Identity) (Identity, error) {
	v, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return Identity{}, fmt.Errorf("setting up signature verification: %w", err)
	}
	sanRegexp := ""
	if id.SubjectRegexp != "" {
		sanRegexp = "^(?:" + id.SubjectRegexp + ")$"
	}
	certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", id.Subject, sanRegexp)
	if err != nil {
		return Identity{}, fmt.Errorf("setting up signature verification: %w", err)
	}
	res, err := v.Verify(entity, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(artifact)), verify.WithCertificateIdentity(certID)))
	if err != nil {
		return Identity{}, fmt.Errorf("the signature doesn't verify: %w", err)
	}
	if res.Signature == nil || res.Signature.Certificate == nil || res.Signature.Certificate.SubjectAlternativeName == "" {
		return Identity{}, errors.New("the signature verified without a certificate identity")
	}
	c := res.Signature.Certificate
	return Identity{Subject: c.SubjectAlternativeName, Issuer: c.Issuer}, nil
}

// PublicGood is the trusted root of Sigstore's public-good instance (the
// one cosign signs with by default), brought up to date through its TUF
// repository from the root sigstore-go embeds. Nothing is cached on disk,
// so it needs the network each time; client makes the requests (a client
// with a timeout when nil).
func PublicGood(ctx context.Context, client *http.Client) (root.TrustedMaterial, error) {
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	opts := tuf.DefaultOptions().WithDisableLocalCache().WithFetcher(&httpFetcher{ctx: ctx, client: client})
	opts.CachePath = ""
	tr, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("fetching Sigstore's trusted root: %w", err)
	}
	return tr, nil
}

// httpFetcher is a TUF fetcher over a given client and context, so a
// cancelled upgrade stops its TUF requests too.
type httpFetcher struct {
	ctx    context.Context
	client *http.Client
}

// DownloadFile fetches url, refusing a body over maxLength. A status other
// than 200 is metadata.ErrDownloadHTTP, which TUF reads to tell the end of
// the root chain (404) from a failure.
func (f *httpFetcher) DownloadFile(url string, maxLength int64, _ time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: res.StatusCode, URL: url}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxLength+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: fmt.Sprintf("%s is larger than %d bytes", url, maxLength)}
	}
	return data, nil
}
