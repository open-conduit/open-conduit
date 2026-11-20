package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// SecretHost is a host's resolution of secret references, given one secret
// provider package (extension.SecretProvider) serving SecretScheme.
// Resolve returns the value, or "" and the safe failure code the operator
// is shown.
type SecretHost interface {
	Resolve(ctx context.Context, ref string) (value, code string)
}

// SecretScheme is the scheme the suite's provider declares.
const SecretScheme = "fixture-vault"

// SecretHostFactory builds a host whose only provider is p, serving
// SecretScheme.
type SecretHostFactory func(p extension.SecretProvider) SecretHost

// SecretHostCase is one secret provider case: a host must pass on only
// what the provider served, and fail closed with a safe code on anything
// else, never with the provider's error text or a value it refused with.
type SecretHostCase struct {
	Name  string
	Check func(SecretHostFactory) error
}

// scriptedProvider answers every request with value and err, recording the
// references it was asked about.
type scriptedProvider struct {
	mu    sync.Mutex
	asked []extension.SecretRequest
	value string
	err   error
}

func (p *scriptedProvider) Serve(_ context.Context, r extension.SecretRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, r)
	return p.value, p.err
}

// leak is text a provider's error carries that the host must never show.
const leak = "token=hunter2"

func SecretHostCases() []SecretHostCase {
	ctx := context.Background()
	ref := SecretScheme + ":kv/app#token"
	failsClosed := func(name string, p *scriptedProvider, wantCode string) SecretHostCase {
		return SecretHostCase{name, func(f SecretHostFactory) error {
			v, code := f(p).Resolve(ctx, ref)
			switch {
			case v != "":
				return fmt.Errorf("host passed on a value the provider did not serve")
			case code == "":
				return fmt.Errorf("host failed without a code")
			case strings.Contains(code, leak) || strings.Contains(code, "hunter2"):
				return fmt.Errorf("host showed the provider's error text as its code")
			case !extension.ValidSecretCode(code):
				return fmt.Errorf("host's code %q is not a safe code", code)
			case wantCode != "" && code != wantCode:
				return fmt.Errorf("code %q, want %q", code, wantCode)
			}
			return nil
		}}
	}
	return []SecretHostCase{
		{"served", func(f SecretHostFactory) error {
			p := &scriptedProvider{value: "s3cr3t"}
			v, code := f(p).Resolve(ctx, ref)
			if v != "s3cr3t" || code != "" {
				return fmt.Errorf("resolved %q with code %q, want the served value", v, code)
			}
			if len(p.asked) != 1 || p.asked[0] != (extension.SecretRequest{Scheme: SecretScheme, Reference: "kv/app#token"}) {
				return fmt.Errorf("provider was asked %+v, want the scheme and the reference text after it, once", p.asked)
			}
			return nil
		}},
		failsClosed("refused", &scriptedProvider{err: &extension.SecretRefusal{Code: "not_found"}}, "not_found"),
		// The negative control the host must hold: a value alongside a
		// refusal is still a refusal.
		failsClosed("refused_with_a_value", &scriptedProvider{value: "s3cr3t", err: &extension.SecretRefusal{Code: "denied"}}, "denied"),
		failsClosed("unsafe_refusal_code", &scriptedProvider{err: &extension.SecretRefusal{Code: "denied: " + leak}}, ""),
		failsClosed("failed", &scriptedProvider{value: "s3cr3t", err: errors.New("vault said " + leak)}, ""),
		failsClosed("empty", &scriptedProvider{}, ""),
		{"other_scheme_not_asked", func(f SecretHostFactory) error {
			p := &scriptedProvider{value: "s3cr3t"}
			v, code := f(p).Resolve(ctx, "fixture-other:kv/app")
			if v != "" || code == "" {
				return fmt.Errorf("a scheme no provider serves resolved %q", v)
			}
			if len(p.asked) != 0 {
				return fmt.Errorf("the provider was asked about a scheme it does not serve")
			}
			return nil
		}},
	}
}

func RunSecretHost(t *testing.T, factory SecretHostFactory) {
	t.Helper()
	for _, c := range SecretHostCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory); err != nil {
				t.Fatal(err)
			}
		})
	}
}
