// Package oidcext serves the OpenID Connect credential verifier
// (extensions/oidc) as a package over protocol version 1, through
// protocol/sdk: the package oidc, providing the verifier type oidc. An
// admin trusts an issuer of that type (open-conduit auth issuer set ID
// --type oidc --config …), the package checks the issuer's config through
// validate (kind verifier), and the host asks it through credential.verify
// about each bearer credential the host didn't issue itself. open-conduit
// serves it as `open-conduit extension-run oidc`.
package oidcext

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/oidc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Version is the package's own version.
const Version = "0.1.0"

// fields are the issuer config's fields (oidc.Config).
var fields = []protocol.FieldDecl{
	{Name: "issuer", Help: "the identity provider's issuer URL; a token's iss must be exactly this", Required: true},
	{Name: "audience", Help: "the audience tokens must be issued for (a client ID or API identifier)", Required: true},
	{Name: "jwks_url", Help: "where the issuer publishes its signing keys (read from its discovery document without it)"},
	{Name: "keys", Help: "signing keys given inline, as a JWKS object, for an issuer that publishes none"},
	{Name: "subject_claim", Help: "the claim naming the person; a dotted name reads a nested claim", Default: "sub"},
	{Name: "groups_claim", Help: "the claim carrying the person's groups, a string or a list; a dotted name reads a nested claim", Default: "groups"},
	{Name: "leeway", Help: "how far exp and nbf may be off for clock skew, at most 5m", Default: "60s"},
}

// Manifest declares the package: the verifier type oidc.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: oidc.Type, Version: Version, Title: "OpenID Connect",
		Summary: "Verifies the signed tokens an OpenID Connect identity provider issues, so the company's directory decides who can reach the management API.",
		Command: protocol.HostRunCommand(oidc.Type),
		Provides: protocol.Provides{
			Verifiers: []protocol.VerifierDecl{{Type: oidc.Type, Fields: fields}},
		}}
}

// Build returns the served package around v.
func Build(v *oidc.Verifier) *sdk.Extension {
	return &sdk.Extension{Manifest: Manifest(), Verifiers: map[string]extension.CredentialVerifier{oidc.Type: v},
		Validate: func(_ context.Context, p protocol.ValidateParams) error {
			if p.Kind == protocol.ValidateVerifier && p.Type == oidc.Type {
				if err := v.Validate(p.Config); err != nil {
					return fmt.Errorf("%w: %s", extension.ErrInvalid, err)
				}
			}
			return nil
		}}
}

// Certification is the package's certification.json: issuer configs it
// accepts and ones it refuses, and credentials it must refuse under the
// accepted ones.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Verifiers: []protocol.VerifierCase{
		{Type: oidc.Type, Config: json.RawMessage(`{"issuer":"https://login.example.com/","audience":"open-conduit","keys":` + exampleKeys + `}`),
			RefusedTokens: []string{
				// alg none, unsigned.
				"eyJhbGciOiJub25lIn0.eyJpc3MiOiJodHRwczovL2xvZ2luLmV4YW1wbGUuY29tLyIsImF1ZCI6Im9wZW4tY29uZHVpdCIsInN1YiI6ImFsaWNlIiwiZXhwIjo0MTAyNDQ0ODAwfQ.",
				// HS256, a shared-secret signature.
				"eyJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJodHRwczovL2xvZ2luLmV4YW1wbGUuY29tLyIsImF1ZCI6Im9wZW4tY29uZHVpdCIsInN1YiI6ImFsaWNlIiwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJl",
				// EdDSA under the fixture's key ID, signed by another key.
				"eyJhbGciOiJFZERTQSIsImtpZCI6ImV4YW1wbGUifQ.eyJpc3MiOiJodHRwczovL2xvZ2luLmV4YW1wbGUuY29tLyIsImF1ZCI6Im9wZW4tY29uZHVpdCIsInN1YiI6ImFsaWNlIiwiZXhwIjo0MTAyNDQ0ODAwfQ.cmVhGF1mlYQfAAQKiwOb5UCyLuAmpxxtPFW0CQVlEY2FSIbOEysmoGGhT1qAXilmHEi3MWl0Y-wchIMQ-lUADw",
			}},
		{Type: oidc.Type, Config: json.RawMessage(`{"issuer":"http://login.example.com/","audience":"open-conduit"}`), Refused: true},
		{Type: oidc.Type, Config: json.RawMessage(`{"issuer":"https://login.example.com/"}`), Refused: true},
	}}}
}

// exampleKeys is an Ed25519 public key for the certification fixture,
// generated for it with its private key discarded, so no token can be
// signed by it.
const exampleKeys = `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"49X28kthhmLCEJW9YsWTcoPDVQRhGvHnsKQXJeU61f0","kid":"example"}]}`

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run oidc` does. Its subcommands package-manifest
// and package-certification print the package files.
func Main(args []string) int {
	ext := Build(oidc.New())
	if sdk.PackageCommand(args, ext.Manifest, Certification(), os.Stdout) {
		return 0
	}
	if err := sdk.Serve(ext); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
