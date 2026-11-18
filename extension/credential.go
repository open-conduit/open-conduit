package extension

import (
	"context"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// IssuerConfigSchema labels the trusted issuer's config the host hands a
// credential verifier: the object an admin gave the issuer (PUT
// /v1/auth/issuers/{id}), passed on as written. Its shape is the providing
// package's own (an OIDC issuer URL and audience, say), which it checks
// through validate (kind verifier) before the host stores it.
const IssuerConfigSchema = "core.auth.issuer.v1"

// CredentialCheck is one bearer credential the host asks a verifier about:
// one the host did not issue itself, offered to one trusted issuer.
type CredentialCheck struct {
	// Issuer is the host's ID for the trusted issuer (the {id} it was
	// stored under). The host records the operator as
	// operator:<Issuer>:<subject>, so the verifier never names the actor.
	Issuer string
	// Config is the issuer's config (schema IssuerConfigSchema).
	Config domain.Payload
	// Token is the bearer credential exactly as the request carried it.
	Token string
}

// VerifiedCredential is who a credential the verifier accepted names, as
// the issuer signed it.
type VerifiedCredential struct {
	// Subject is the issuer's stable ID for the person (an OIDC sub, say):
	// 1 to 256 printable characters, no spaces.
	Subject string
	// Groups are the groups the issuer says the person is in. The host
	// maps them to roles through the issuer's group table, which only an
	// admin sets; a group the table doesn't name grants nothing.
	Groups []string
	// ExpiresAt is when the credential stops being valid. It is required:
	// the host refuses a credential without one, and stops accepting it
	// at that time even if the verifier is never asked again.
	ExpiresAt time.Time
}

// CredentialVerifier checks bearer credentials an identity provider issued
// (an OIDC ID token or a JWT access token, a SAML-backed broker's
// assertion). It owns the protocol: signatures, keys and their rotation,
// the issuer's own claims. The host owns what follows: it derives the
// actor from the issuer's ID and the subject, enforces ExpiresAt, maps
// Groups to a role and refuses the request (401) on any error, so a
// verifier that is down, or a key it can't fetch, never lets a call in.
//
// Verify returns ErrUnauthorized, or any other error, for a credential the
// issuer didn't issue or that isn't valid now. The host may offer the same
// credential to several issuers in turn, so a token that is merely not
// this issuer's is an ordinary refusal. It must not call the network on
// every request: cache what the issuer publishes (its signing keys) and
// refetch when it rotates.
type CredentialVerifier interface {
	Verify(ctx context.Context, c CredentialCheck) (VerifiedCredential, error)
}
