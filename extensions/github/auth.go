package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SecretFunc resolves a connection secret by name (a key of the connection's
// secret_refs) immediately before use. Implementations must not log values.
type SecretFunc func(ctx context.Context, name string) (string, error)

// ErrCredential is returned for missing/invalid credentials; it never carries
// secret material or provider error text.
var ErrCredential = errors.New("github credential unavailable")

// tokenSource hands out a bearer token for calls on a repo ("" = app itself).
type tokenSource interface {
	token(ctx context.Context, repo string) (string, error)
	invalidate(repo string)
}

// staticToken resolves a token secret lazily and again after a 401, so a
// rotated reference (or `cmd:gh auth token`) is picked up without restart.
type staticToken struct {
	secrets SecretFunc
	name    string
	mu      sync.Mutex
	tok     string
}

func (s *staticToken) token(ctx context.Context, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" {
		return s.tok, nil
	}
	if s.secrets == nil {
		return "", ErrCredential
	}
	v, err := s.secrets(ctx, s.name)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", ErrCredential
	}
	s.tok = strings.TrimSpace(v)
	return s.tok, nil
}

func (s *staticToken) invalidate(string) {
	s.mu.Lock()
	s.tok = ""
	s.mu.Unlock()
}

// appToken is a GitHub App: an RS256 JWT for the app itself and installation
// tokens per repository, cached until five minutes before they expire.
type appToken struct {
	id      string
	secrets SecretFunc
	keyName string
	c       *Client

	mu    sync.Mutex
	key   *rsa.PrivateKey
	inst  map[string]int64
	cache map[int64]cachedToken
	ident GitIdentity
}

type cachedToken struct {
	tok     string
	expires time.Time
}

func newAppToken(id string, secrets SecretFunc, keyName string, c *Client) *appToken {
	return &appToken{id: id, secrets: secrets, keyName: keyName, c: c, inst: map[string]int64{}, cache: map[int64]cachedToken{}}
}

// parseKey reads an App's PEM private key. It also takes the key as two
// stores hand back a multi-line value: hex (macOS Keychain's `security
// find-generic-password -w`, which is how a `keyring:` reference reads it
// there) and with its line breaks written as `\n` (an environment variable).
func parseKey(text string) (*rsa.PrivateKey, error) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "-----BEGIN") {
		if raw, err := hex.DecodeString(text); err == nil {
			text = strings.TrimSpace(string(raw))
		}
	}
	if !strings.Contains(text, "\n") {
		text = strings.ReplaceAll(text, `\n`, "\n")
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, ErrCredential
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrCredential
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrCredential
	}
	return rk, nil
}

func (a *appToken) privateKey(ctx context.Context) (*rsa.PrivateKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key != nil {
		return a.key, nil
	}
	if a.secrets == nil {
		return nil, ErrCredential
	}
	pemText, err := a.secrets(ctx, a.keyName)
	if err != nil {
		return nil, ErrCredential
	}
	if a.key, err = parseKey(pemText); err != nil {
		return nil, err
	}
	return a.key, nil
}

// jwt signs a short-lived token that authenticates as the app itself.
func (a *appToken) jwt(ctx context.Context) (string, error) {
	key, err := a.privateKey(ctx)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	now := a.c.now()
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.id})
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", ErrCredential
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// installation is the App's installation on repo, looked up once.
func (a *appToken) installation(ctx context.Context, jwt, repo string) (int64, error) {
	a.mu.Lock()
	id, ok := a.inst[repo]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if _, err := a.c.raw(ctx, "Bearer "+jwt, http.MethodGet, "/repos/"+repo+"/installation", nil, &inst); err != nil {
		return 0, fmt.Errorf("github app installation for %s: %w", repo, err)
	}
	a.mu.Lock()
	a.inst[repo] = inst.ID
	a.mu.Unlock()
	return inst.ID, nil
}

func (a *appToken) token(ctx context.Context, repo string) (string, error) {
	jwt, err := a.jwt(ctx)
	if err != nil || repo == "" {
		return jwt, err
	}
	id, err := a.installation(ctx, jwt, repo)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	cached, ok := a.cache[id]
	a.mu.Unlock()
	if ok && cached.expires.Sub(a.c.now()) > 5*time.Minute {
		return cached.tok, nil
	}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := a.c.raw(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), nil, &t); err != nil {
		return "", fmt.Errorf("github installation token for %s: %w", repo, err)
	}
	if t.Token == "" {
		return "", ErrCredential
	}
	a.mu.Lock()
	a.cache[id] = cachedToken{t.Token, t.ExpiresAt}
	a.mu.Unlock()
	return t.Token, nil
}

func (a *appToken) invalidate(repo string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.inst[repo]; ok {
		delete(a.cache, id)
	}
	delete(a.inst, repo)
	a.key = nil
}

// scoped mints an installation token for one repository with only
// contents: write, for git: it is never cached here, so the caller owns
// it and revokes it (revokeInstallationToken).
func (a *appToken) scoped(ctx context.Context, repo string) (string, time.Time, error) {
	jwt, err := a.jwt(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	id, err := a.installation(ctx, jwt, repo)
	if err != nil {
		return "", time.Time{}, err
	}
	_, name, _ := strings.Cut(repo, "/")
	body := map[string]any{"repositories": []string{name}, "permissions": map[string]string{"contents": "write"}}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := a.c.raw(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), body, &t); err != nil {
		return "", time.Time{}, fmt.Errorf("github git token for %s: %w", repo, err)
	}
	if t.Token == "" {
		return "", time.Time{}, ErrCredential
	}
	return t.Token, t.ExpiresAt, nil
}

// identity is the App's bot user as git's author and committer, looked up
// once: the slug from GET /app (as the App), then the bot user's id from
// GET /users/<slug>[bot] (with an installation token for repo).
func (a *appToken) identity(ctx context.Context, repo string) (GitIdentity, error) {
	a.mu.Lock()
	ident := a.ident
	a.mu.Unlock()
	if ident.Name != "" {
		return ident, nil
	}
	jwt, err := a.jwt(ctx)
	if err != nil {
		return GitIdentity{}, err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if _, err := a.c.raw(ctx, "Bearer "+jwt, http.MethodGet, "/app", nil, &app); err != nil {
		return GitIdentity{}, fmt.Errorf("github app: %w", err)
	}
	if app.Slug == "" || strings.ContainsAny(app.Slug, "/?#%\n\x00<>") {
		return GitIdentity{}, errors.New("github app: no slug")
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return GitIdentity{}, err
	}
	login := app.Slug + "[bot]"
	var user struct {
		ID int64 `json:"id"`
	}
	if _, err := a.c.raw(ctx, "Bearer "+tok, http.MethodGet, "/users/"+url.PathEscape(login), nil, &user); err != nil {
		return GitIdentity{}, fmt.Errorf("github app bot user: %w", err)
	}
	if user.ID == 0 {
		return GitIdentity{}, errors.New("github app bot user: no id")
	}
	ident = GitIdentity{Name: login, Email: fmt.Sprintf("%d+%s@users.noreply.%s", user.ID, login, a.c.host)}
	a.mu.Lock()
	a.ident = ident
	a.mu.Unlock()
	return ident, nil
}
