package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
)

// referenceSecretHost is the suite's own minimal host; trusting passes on
// whatever value the provider returned even when it refused, and echoing
// shows the provider's error text as its code.
type referenceSecretHost struct {
	p                 extension.SecretProvider
	trusting, echoing bool
}

func (h referenceSecretHost) Resolve(ctx context.Context, ref string) (string, string) {
	scheme, rest, _ := strings.Cut(ref, ":")
	if scheme != conformance.SecretScheme {
		return "", "provider_unavailable"
	}
	v, err := h.p.Serve(ctx, extension.SecretRequest{Scheme: scheme, Reference: rest})
	var refused *extension.SecretRefusal
	switch {
	case h.trusting && v != "":
		return v, ""
	case h.echoing && err != nil:
		return "", err.Error()
	case errors.As(err, &refused) && extension.ValidSecretCode(refused.Code):
		return "", refused.Code
	case errors.As(err, &refused):
		return "", "provider_refused"
	case err != nil:
		return "", "provider_failed"
	case v == "":
		return "", "empty"
	}
	return v, ""
}

func TestSecretHostReference(t *testing.T) {
	conformance.RunSecretHost(t, func(p extension.SecretProvider) conformance.SecretHost { return referenceSecretHost{p: p} })
}

func TestWeakenedSecretHostIsRejected(t *testing.T) {
	for name, h := range map[string]referenceSecretHost{"trusting": {trusting: true}, "echoing": {echoing: true}} {
		failed := false
		for _, c := range conformance.SecretHostCases() {
			if c.Check(func(p extension.SecretProvider) conformance.SecretHost { h.p = p; return h }) != nil {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("%s secret host passed the secret host suite", name)
		}
	}
}
