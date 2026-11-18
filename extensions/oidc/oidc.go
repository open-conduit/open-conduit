// Package oidc verifies the signed JSON Web Tokens an OpenID Connect
// identity provider issues (ID tokens, or JWT access tokens), as a
// credential verifier (extension.CredentialVerifier). An admin trusts an
// issuer of type oidc with its issuer URL and audience; this package
// checks a token's signature against the issuer's published keys (JWKS),
// and its iss, aud, exp and nbf, and answers who it names and the groups
// it carries. The host decides what that person may do.
//
// It never runs a login flow, holds passwords or keeps sessions: whoever
// calls the API gets a token from the identity provider themselves. Keys
// are fetched once and cached, then fetched again when a token names a key
// the cache doesn't have (the issuer rotated its keys) or the cache is an
// hour old; a failed fetch keeps the keys it had, so a brief outage of the
// identity provider doesn't lock anyone out, and with no keys at all every
// token is refused.
package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Type is the credential verifier type, and the package's name.
const Type = "oidc"

// Config is a trusted issuer's config, as an admin gives it (PUT
// /v1/auth/issuers/{id} with type oidc).
type Config struct {
	// Issuer is the identity provider's issuer URL. A token's iss must be
	// exactly this.
	Issuer string `json:"issuer"`
	// Audience is the audience tokens must be issued for (a client ID,
	// or an API identifier): a token's aud must be or contain it.
	Audience string `json:"audience"`
	// JWKSURL is where the issuer publishes its signing keys. Without it
	// the package reads the issuer's discovery document
	// (<issuer>/.well-known/openid-configuration) for its jwks_uri.
	JWKSURL string `json:"jwks_url,omitempty"`
	// Keys are signing keys given inline, a JWKS object ({"keys": […]}),
	// for an issuer that publishes none; nothing is fetched then.
	Keys json.RawMessage `json:"keys,omitempty"`
	// SubjectClaim is the claim naming the person (default sub). A dotted
	// name reads a nested claim.
	SubjectClaim string `json:"subject_claim,omitempty"`
	// GroupsClaim is the claim carrying the person's groups, a string or
	// a list of strings (default groups). A dotted name reads a nested
	// claim (realm_access.roles, say). A token without it has no groups.
	GroupsClaim string `json:"groups_claim,omitempty"`
	// Leeway is how far exp and nbf may be off for clock skew (default
	// 60s, at most 5m).
	Leeway string `json:"leeway,omitempty"`
}

const (
	defaultLeeway = time.Minute
	maxLeeway     = 5 * time.Minute
	// keysMaxAge is how long fetched keys are used before they are
	// fetched again.
	keysMaxAge = time.Hour
	// refetchAfter is the least time between two fetches of one issuer's
	// keys, so tokens naming unknown keys can't make the package hammer
	// the identity provider.
	refetchAfter = 30 * time.Second
	// maxDocument bounds a discovery document or JWKS.
	maxDocument = 1 << 20
)

// ParseConfig reads and checks an issuer config: an issuer URL, an
// audience, https URLs (http only to a loopback address), and inline keys
// that parse.
func ParseConfig(raw json.RawMessage) (Config, time.Duration, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, 0, fmt.Errorf("the oidc issuer config is not one this package reads: %w", err)
	}
	if c.Issuer == "" || c.Audience == "" {
		return c, 0, errors.New("issuer and audience are required")
	}
	if err := checkURL(c.Issuer); err != nil {
		return c, 0, fmt.Errorf("issuer: %w", err)
	}
	if c.JWKSURL != "" {
		if err := checkURL(c.JWKSURL); err != nil {
			return c, 0, fmt.Errorf("jwks_url: %w", err)
		}
	}
	if len(c.Keys) > 0 {
		if _, err := parseJWKS(c.Keys); err != nil {
			return c, 0, fmt.Errorf("keys: %w", err)
		}
	}
	if c.SubjectClaim == "" {
		c.SubjectClaim = "sub"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	leeway := defaultLeeway
	if c.Leeway != "" {
		var err error
		if leeway, err = time.ParseDuration(c.Leeway); err != nil || leeway < 0 || leeway > maxLeeway {
			return c, 0, errors.New("leeway must be a duration from 0s to 5m")
		}
	}
	return c, leeway, nil
}

// checkURL accepts an https URL, or http to a loopback address (an
// identity provider on the same machine, or a test's).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("must be an absolute URL with a host and no credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("must be https (http only to a loopback address)")
}

// Verifier is the oidc credential verifier. Its zero value is not usable;
// use New.
type Verifier struct {
	// HTTP fetches discovery documents and keys (http.DefaultClient with a
	// timeout when nil).
	HTTP *http.Client
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	mu   sync.Mutex
	sets map[string]*keySet
}

var _ extension.CredentialVerifier = (*Verifier)(nil)

// New returns a verifier with its own key caches.
func New() *Verifier {
	return &Verifier{HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

// keySet is one issuer's cached signing keys.
type keySet struct {
	mu        sync.Mutex
	keys      []jwk
	fetchedAt time.Time
	triedAt   time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now == nil {
		return time.Now()
	}
	return v.Now()
}

func (v *Verifier) client() *http.Client {
	if v.HTTP == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return v.HTTP
}

// Validate is the package's check of an issuer config (validate, kind
// verifier).
func (v *Verifier) Validate(raw json.RawMessage) error {
	_, _, err := ParseConfig(raw)
	return err
}

// Verify checks a token against the issuer's config. Every refusal wraps
// extension.ErrUnauthorized.
func (v *Verifier) Verify(ctx context.Context, c extension.CredentialCheck) (extension.VerifiedCredential, error) {
	cfg, leeway, err := ParseConfig(c.Config.Data)
	if err != nil {
		return extension.VerifiedCredential{}, refuse("the issuer config is invalid")
	}
	tok, err := parseToken(c.Token)
	if err != nil {
		return extension.VerifiedCredential{}, refuse(err.Error())
	}
	keys, err := v.keysFor(ctx, cfg, tok.header.Kid)
	if err != nil {
		return extension.VerifiedCredential{}, refuse("the issuer's signing keys are unavailable")
	}
	if !tok.verify(keys) {
		return extension.VerifiedCredential{}, refuse("the signature doesn't match any of the issuer's keys")
	}
	now := v.now()
	claims := tok.claims
	if iss, _ := claims["iss"].(string); iss != cfg.Issuer {
		return extension.VerifiedCredential{}, refuse("the token is not from this issuer")
	}
	if !audience(claims["aud"], cfg.Audience) {
		return extension.VerifiedCredential{}, refuse("the token is not for this audience")
	}
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return extension.VerifiedCredential{}, refuse("the token has no expiry")
	}
	if !now.Before(exp.Add(leeway)) {
		return extension.VerifiedCredential{}, refuse("the token has expired")
	}
	if nbf, present := claims["nbf"]; present {
		at, ok := numericDate(nbf)
		if !ok || now.Add(leeway).Before(at) {
			return extension.VerifiedCredential{}, refuse("the token is not valid yet")
		}
	}
	subject, _ := claim(claims, cfg.SubjectClaim).(string)
	if subject == "" {
		return extension.VerifiedCredential{}, refuse("the token names no subject")
	}
	// The host stops accepting the token at ExpiresAt; with leeway, that
	// is when this package would start refusing it.
	return extension.VerifiedCredential{Subject: subject, Groups: groups(claim(claims, cfg.GroupsClaim)), ExpiresAt: exp.Add(leeway)}, nil
}

func refuse(why string) error { return fmt.Errorf("%w: %s", extension.ErrUnauthorized, why) }

// claim reads a dotted claim name through nested objects.
func claim(claims map[string]any, name string) any {
	var cur any = claims
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func groups(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, g := range x {
			if s, ok := g.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func audience(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case []any:
		for _, a := range x {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
}

// keysFor returns the issuer's keys: the inline ones, or the cached ones,
// fetched first when there are none, they are old, or kid names a key the
// cache doesn't have.
func (v *Verifier) keysFor(ctx context.Context, cfg Config, kid string) ([]jwk, error) {
	if len(cfg.Keys) > 0 {
		return parseJWKS(cfg.Keys)
	}
	cacheKey := cfg.Issuer + "\x00" + cfg.JWKSURL
	v.mu.Lock()
	if v.sets == nil {
		v.sets = map[string]*keySet{}
	}
	set := v.sets[cacheKey]
	if set == nil {
		set = &keySet{}
		v.sets[cacheKey] = set
	}
	v.mu.Unlock()

	set.mu.Lock()
	defer set.mu.Unlock()
	now := v.now()
	stale := set.keys == nil || now.Sub(set.fetchedAt) > keysMaxAge || kid != "" && !hasKid(set.keys, kid)
	if stale && (set.triedAt.IsZero() || now.Sub(set.triedAt) >= refetchAfter) {
		set.triedAt = now
		if keys, err := v.fetch(ctx, cfg); err == nil {
			set.keys, set.fetchedAt = keys, now
		}
	}
	if set.keys == nil {
		return nil, errors.New("no keys")
	}
	return set.keys, nil
}

func hasKid(keys []jwk, kid string) bool {
	for _, k := range keys {
		if k.Kid == kid {
			return true
		}
	}
	return false
}

func (v *Verifier) fetch(ctx context.Context, cfg Config) ([]jwk, error) {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, strings.TrimSuffix(cfg.Issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
			return nil, err
		}
		// The discovery document must be the issuer's own (OpenID
		// Connect Discovery 4.3).
		if doc.Issuer != cfg.Issuer || checkURL(doc.JWKSURI) != nil {
			return nil, errors.New("the discovery document is not this issuer's")
		}
		jwksURL = doc.JWKSURI
	}
	var raw json.RawMessage
	if err := v.getJSON(ctx, jwksURL, &raw); err != nil {
		return nil, err
	}
	return parseJWKS(raw)
}

func (v *Verifier) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return err
	}
	if len(b) > maxDocument {
		return errors.New("document too large")
	}
	return json.Unmarshal(b, out)
}

// jwk is one parsed signing key.
type jwk struct {
	Kid string
	Alg string
	Key crypto.PublicKey
}

// parseJWKS reads a JWKS object, keeping the signing keys it can use and
// skipping the rest (encryption keys, unknown key types).
func parseJWKS(raw json.RawMessage) ([]jwk, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("not a JWKS object")
	}
	var out []jwk
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		var key crypto.PublicKey
		switch k.Kty {
		case "RSA":
			n, err1 := b64(k.N)
			e, err2 := b64(k.E)
			if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
				continue
			}
			key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			curve := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}[k.Crv]
			x, err1 := b64(k.X)
			y, err2 := b64(k.Y)
			if curve == nil || err1 != nil || err2 != nil {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !curve.IsOnCurve(pub.X, pub.Y) {
				continue
			}
			key = pub
		case "OKP":
			x, err := b64(k.X)
			if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
				continue
			}
			key = ed25519.PublicKey(x)
		default:
			continue
		}
		out = append(out, jwk{Kid: k.Kid, Alg: k.Alg, Key: key})
	}
	if len(out) == 0 {
		return nil, errors.New("no usable signing keys")
	}
	return out, nil
}

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

type token struct {
	header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	claims    map[string]any
	signed    []byte
	signature []byte
}

// parseToken reads a compact JWS whose payload is a JSON claims object.
func parseToken(s string) (*token, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a signed JWT")
	}
	t := &token{signed: []byte(parts[0] + "." + parts[1])}
	h, err := b64(parts[0])
	if err != nil || json.Unmarshal(h, &t.header) != nil {
		return nil, errors.New("the JWT header is malformed")
	}
	if len(t.header.Crit) > 0 {
		return nil, errors.New("the JWT has critical headers this package doesn't understand")
	}
	if _, ok := algs[t.header.Alg]; !ok {
		return nil, errors.New("the JWT's algorithm is not one this package accepts")
	}
	p, err := b64(parts[1])
	if err != nil {
		return nil, errors.New("the JWT payload is malformed")
	}
	d := json.NewDecoder(bytes.NewReader(p))
	d.UseNumber()
	if err = d.Decode(&t.claims); err != nil || t.claims == nil {
		return nil, errors.New("the JWT payload is not a claims object")
	}
	if t.signature, err = b64(parts[2]); err != nil || len(t.signature) == 0 {
		return nil, errors.New("the JWT signature is malformed")
	}
	return t, nil
}

type alg struct {
	hash func() hash.Hash
	h    crypto.Hash
	kind string // rsa, pss, ec or ed
	size int    // an EC signature's r and s length
}

// algs are the signing algorithms accepted: asymmetric only, so a token
// is never checked against a shared secret, and never "none".
var algs = map[string]alg{
	"RS256": {sha256.New, crypto.SHA256, "rsa", 0},
	"RS384": {sha512.New384, crypto.SHA384, "rsa", 0},
	"RS512": {sha512.New, crypto.SHA512, "rsa", 0},
	"PS256": {sha256.New, crypto.SHA256, "pss", 0},
	"PS384": {sha512.New384, crypto.SHA384, "pss", 0},
	"PS512": {sha512.New, crypto.SHA512, "pss", 0},
	"ES256": {sha256.New, crypto.SHA256, "ec", 32},
	"ES384": {sha512.New384, crypto.SHA384, "ec", 48},
	"ES512": {sha512.New, crypto.SHA512, "ec", 66},
	"EdDSA": {nil, 0, "ed", 0},
}

// verify reports whether one of keys signed the token: a key the token
// names by kid (any key when it names none), of the algorithm's kind, and
// of the algorithm the key itself declares, if it declares one.
func (t *token) verify(keys []jwk) bool {
	a := algs[t.header.Alg]
	var digest []byte
	if a.hash != nil {
		h := a.hash()
		h.Write(t.signed)
		digest = h.Sum(nil)
	}
	for _, k := range keys {
		if t.header.Kid != "" && k.Kid != t.header.Kid || k.Alg != "" && k.Alg != t.header.Alg {
			continue
		}
		switch pub := k.Key.(type) {
		case *rsa.PublicKey:
			switch a.kind {
			case "rsa":
				if rsa.VerifyPKCS1v15(pub, a.h, digest, t.signature) == nil {
					return true
				}
			case "pss":
				if rsa.VerifyPSS(pub, a.h, digest, t.signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil {
					return true
				}
			}
		case *ecdsa.PublicKey:
			if a.kind != "ec" || len(t.signature) != 2*a.size || (pub.Curve.Params().BitSize+7)/8 != a.size {
				continue
			}
			r := new(big.Int).SetBytes(t.signature[:a.size])
			s := new(big.Int).SetBytes(t.signature[a.size:])
			if ecdsa.Verify(pub, digest, r, s) {
				return true
			}
		case ed25519.PublicKey:
			if a.kind == "ed" && ed25519.Verify(pub, t.signed, t.signature) {
				return true
			}
		}
	}
	return false
}
