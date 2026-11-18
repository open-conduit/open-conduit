package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

var enc = base64.RawURLEncoding.EncodeToString

// idp is a fake identity provider: a discovery document and a JWKS whose
// keys a test can rotate, counting the JWKS fetches.
type idp struct {
	srv     *httptest.Server
	mu      sync.Mutex
	jwks    []map[string]string
	down    bool
	fetches atomic.Int32
}

func newIDP(t *testing.T) *idp {
	p := &idp{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.issuer(), "jwks_uri": p.srv.URL + "/keys"})
		case "/keys":
			p.fetches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": p.jwks})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) issuer() string { return p.srv.URL + "/" }

func (p *idp) setKeys(keys ...map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwks = keys
}

func (p *idp) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

func rsaJWK(kid string, k *rsa.PrivateKey) map[string]string {
	return map[string]string{"kty": "RSA", "kid": kid, "use": "sig", "n": enc(k.N.Bytes()), "e": enc(big.NewInt(int64(k.E)).Bytes())}
}

func sign(t *testing.T, alg, kid string, key crypto.Signer, claims map[string]any) string {
	t.Helper()
	h := map[string]string{"alg": alg, "typ": "JWT"}
	if kid != "" {
		h["kid"] = kid
	}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(claims)
	signed := enc(hb) + "." + enc(cb)
	var sig []byte
	var err error
	switch k := key.(type) {
	case *rsa.PrivateKey:
		d := sha256.Sum256([]byte(signed))
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
	case *ecdsa.PrivateKey:
		d := sha256.Sum256([]byte(signed))
		r, s, e := ecdsa.Sign(rand.Reader, k, d[:])
		err = e
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, []byte(signed))
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + enc(sig)
}

type fixture struct {
	t   *testing.T
	idp *idp
	v   *Verifier
	now time.Time
	key *rsa.PrivateKey
}

func newFixture(t *testing.T) *fixture {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, idp: newIDP(t), now: time.Unix(1_900_000_000, 0), key: key}
	f.idp.setKeys(rsaJWK("k1", key))
	f.v = &Verifier{HTTP: f.idp.srv.Client(), Now: func() time.Time { return f.now }}
	return f
}

func (f *fixture) config(extra map[string]any) json.RawMessage {
	c := map[string]any{"issuer": f.idp.issuer(), "audience": "open-conduit"}
	for k, v := range extra {
		c[k] = v
	}
	b, _ := json.Marshal(c)
	return b
}

func (f *fixture) claims(extra map[string]any) map[string]any {
	c := map[string]any{"iss": f.idp.issuer(), "aud": "open-conduit", "sub": "alice", "exp": f.now.Add(time.Hour).Unix(), "groups": []string{"platform", "staff"}}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (f *fixture) verify(config json.RawMessage, token string) (extension.VerifiedCredential, error) {
	return f.v.Verify(context.Background(), extension.CredentialCheck{Issuer: "corp", Config: domain.Payload{Schema: extension.IssuerConfigSchema, Data: config}, Token: token})
}

func TestVerifyAcceptsAnIssuersToken(t *testing.T) {
	f := newFixture(t)
	got, err := f.verify(f.config(nil), sign(t, "RS256", "k1", f.key, f.claims(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "alice" || len(got.Groups) != 2 || got.Groups[0] != "platform" || !got.ExpiresAt.Equal(f.now.Add(time.Hour+defaultLeeway)) {
		t.Fatalf("verified %+v", got)
	}
	// aud may be a list, and the claims nested.
	got, err = f.verify(f.config(map[string]any{"subject_claim": "email", "groups_claim": "realm_access.roles"}),
		sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"aud": []string{"other", "open-conduit"}, "email": "alice@example.com", "realm_access": map[string]any{"roles": []string{"admins"}}})))
	if err != nil || got.Subject != "alice@example.com" || len(got.Groups) != 1 || got.Groups[0] != "admins" {
		t.Fatalf("nested claims: %+v %v", got, err)
	}
}

// TestVerifyRefuses: every way a token can be wrong is refused as
// unauthorized.
func TestVerifyRefuses(t *testing.T) {
	f := newFixture(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	ok := sign(t, "RS256", "k1", f.key, f.claims(nil))
	unsigned := ok[:len(ok)-len(ok[lastDot(ok):])] + "."
	cases := map[string]string{
		"another key":       sign(t, "RS256", "k1", other, f.claims(nil)),
		"another issuer":    sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"iss": "https://evil.example.com/"})),
		"another audience":  sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"aud": "someone-else"})),
		"expired":           sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"exp": f.now.Add(-2 * time.Minute).Unix()})),
		"no expiry":         sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"exp": nil})),
		"not yet valid":     sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"nbf": f.now.Add(10 * time.Minute).Unix()})),
		"no subject":        sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"sub": nil})),
		"alg none":          enc([]byte(`{"alg":"none"}`)) + "." + ok[len(ok[:indexDot(ok)])+1:lastDot(ok)] + ".",
		"signature removed": unsigned,
		"garbage":           "not-a-jwt",
	}
	for name, token := range cases {
		if _, err := f.verify(f.config(nil), token); !errors.Is(err, extension.ErrUnauthorized) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A token within the leeway of its expiry is still valid.
	if _, err := f.verify(f.config(nil), sign(t, "RS256", "k1", f.key, f.claims(map[string]any{"exp": f.now.Add(-30 * time.Second).Unix()}))); err != nil {
		t.Errorf("within leeway: %v", err)
	}
}

func indexDot(s string) int {
	for i := range s {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// TestKeysAreCachedAndRotated: keys are fetched once, again when a token
// names a key the cache lacks (no sooner than refetchAfter), kept through
// an outage, and refused when there are none.
func TestKeysAreCachedAndRotated(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		if _, err := f.verify(f.config(nil), sign(t, "RS256", "k1", f.key, f.claims(nil))); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.idp.fetches.Load(); n != 1 {
		t.Fatalf("fetched the keys %d times for three tokens", n)
	}
	// The issuer rotates to k2.
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.idp.setKeys(rsaJWK("k1", f.key), rsaJWK("k2", k2))
	f.now = f.now.Add(refetchAfter)
	if _, err := f.verify(f.config(nil), sign(t, "RS256", "k2", k2, f.claims(nil))); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	// A token naming an unknown key fetches again at most once per
	// refetchAfter.
	for range 5 {
		_, _ = f.verify(f.config(nil), sign(t, "RS256", "k9", k2, f.claims(nil)))
	}
	if n := f.idp.fetches.Load(); n != 2 {
		t.Fatalf("unknown keys fetched %d times within refetchAfter, want 2 in all", n)
	}
	f.now = f.now.Add(refetchAfter)
	_, _ = f.verify(f.config(nil), sign(t, "RS256", "k9", k2, f.claims(nil)))
	if n := f.idp.fetches.Load(); n != 3 {
		t.Fatalf("fetched the keys %d times, want 3", n)
	}
	// Down: the cached keys still verify after they are an hour old.
	f.idp.setDown(true)
	f.now = f.now.Add(2 * keysMaxAge)
	if _, err := f.verify(f.config(nil), sign(t, "RS256", "k2", k2, f.claims(nil))); err != nil {
		t.Fatalf("cached keys during an outage: %v", err)
	}
	// A fresh verifier with the issuer down has no keys: refused.
	cold := &Verifier{HTTP: f.idp.srv.Client(), Now: f.v.Now}
	if _, err := cold.Verify(context.Background(), extension.CredentialCheck{Config: domain.Payload{Data: f.config(nil)}, Token: sign(t, "RS256", "k2", k2, f.claims(nil))}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("no keys: %v", err)
	}
}

func TestInlineKeysAndOtherAlgorithms(t *testing.T) {
	f := newFixture(t)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]any{"keys": []map[string]string{
		{"kty": "EC", "crv": "P-256", "kid": "ec", "x": enc(ec.X.FillBytes(make([]byte, 32))), "y": enc(ec.Y.FillBytes(make([]byte, 32)))},
		{"kty": "OKP", "crv": "Ed25519", "kid": "ed", "x": enc(edPub)},
	}}
	cfg := f.config(map[string]any{"keys": keys})
	for name, token := range map[string]string{
		"ES256": sign(t, "ES256", "ec", ec, f.claims(nil)),
		"EdDSA": sign(t, "EdDSA", "ed", edPriv, f.claims(nil)),
	} {
		if _, err := f.verify(cfg, token); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An EdDSA token under the EC key's ID is refused.
	if _, err := f.verify(cfg, sign(t, "EdDSA", "ec", edPriv, f.claims(nil))); err == nil {
		t.Error("accepted a token under another kind of key")
	}
	if f.idp.fetches.Load() != 0 {
		t.Error("fetched keys despite inline keys")
	}
}

func TestParseConfig(t *testing.T) {
	for _, bad := range []string{
		`{}`,
		`{"issuer":"https://login.example.com/"}`,
		`{"issuer":"http://login.example.com/","audience":"a"}`,
		`{"issuer":"https://login.example.com/","audience":"a","jwks_url":"ftp://x/"}`,
		`{"issuer":"https://login.example.com/","audience":"a","leeway":"1h"}`,
		`{"issuer":"https://login.example.com/","audience":"a","keys":{"keys":[]}}`,
		`{"issuer":"https://login.example.com/","audience":"a","client_secret":"x"}`,
	} {
		if _, _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, good := range []string{
		`{"issuer":"https://login.example.com/","audience":"a"}`,
		`{"issuer":"http://127.0.0.1:8080/","audience":"a","leeway":"0s"}`,
	} {
		if _, _, err := ParseConfig(json.RawMessage(good)); err != nil {
			t.Errorf("refused %s: %v", good, err)
		}
	}
}
