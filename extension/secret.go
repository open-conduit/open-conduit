package extension

import (
	"context"
	"regexp"
)

// SecretRequest is one secret reference the host asks a provider to
// resolve: Scheme is one the provider declared (manifest secret_schemes)
// and Reference the text after "<scheme>:", as the operator wrote it in a
// secret slot (a Vault path and field, a secrets manager ARN).
type SecretRequest struct {
	Scheme    string
	Reference string
}

// SecretProvider resolves the secret references of the schemes it declares
// against a secrets manager (Vault, a cloud provider's), with its own
// identity. It owns the manager's protocol, its authentication and its
// errors. The host owns everything else, as for the built-in forms: it asks
// only when a slot's authorised consumer needs the value, keeps the value
// no longer than one attempt epoch, registers it for redaction, records
// each resolution by name and outcome, never value, and fails closed
// with no fallback when the provider refuses, errs or is down.
//
// Serve returns the value, or a SecretRefusal with a safe code when the
// reference names nothing the provider can serve (not found, denied). Any
// other error is reported to the operator as provider_failed, with its text
// discarded. An empty value is refused as empty.
type SecretProvider interface {
	Serve(ctx context.Context, r SecretRequest) (string, error)
}

// SecretRefusal is a provider's safe answer for a reference it can't serve.
// Code is what the operator is shown (in doctor, GET /v1/secrets and the
// resolution record), so it must never carry the value, the reference or
// the manager's error text: lower-case letters, digits and underscores, at
// most 64 characters (ValidSecretCode). An invalid code is reported as
// provider_refused.
type SecretRefusal struct{ Code string }

func (e *SecretRefusal) Error() string { return "secret refused: " + e.Code }

var secretCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidSecretCode reports whether code is a safe refusal code.
func ValidSecretCode(code string) bool { return secretCode.MatchString(code) }
