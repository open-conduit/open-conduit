package backup

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A bundle's manifest holds the digest of its database and of every
// artifact, so a signature over the manifest's exact bytes authenticates the
// whole bundle. The signature is a separate file, so a binary from before
// signing still reads a signed bundle, and an unsigned bundle is unchanged.

const (
	signatureName = "manifest.sig"
	// signatureContext separates these signatures from anything else the
	// same key might ever sign.
	signatureContext = "open-conduit backup manifest v1\x00"
	signatureAlg     = "ed25519"
	maxSignatureSize = 4 << 10
	// PrivateKeyPrefix and PublicKeyPrefix mark the text forms of a key.
	PrivateKeyPrefix = "ed25519-private:"
	PublicKeyPrefix  = "ed25519:"
)

// The authenticity a bundle was found to have.
const (
	// Verified: signed by a key the operator trusts.
	Verified = "verified"
	// Unverified: signed, but no trust key is configured to check it with.
	Unverified = "unverified"
	// Unsigned: no signature, and the operator's policy allows that.
	Unsigned = "unsigned"
)

// ErrNotAuthentic refuses a bundle that fails the operator's trust policy.
var ErrNotAuthentic = errors.New("backup: bundle is not authentic")

type signatureFile struct {
	Version   uint32
	Algorithm string
	KeyID     string
	Signature []byte
}

// Trust is the operator's policy for a bundle's origin. It must come from
// outside the bundle. With Keys, a bundle must carry a valid signature from
// one of them; AcceptUnsigned lets one with no signature through, but never
// one whose signature is wrong. With no Keys there is nothing to check a
// signature against, so the result only reports what the bundle claims.
type Trust struct {
	Keys           []ed25519.PublicKey
	AcceptUnsigned bool
}

// Authenticity is what checking a bundle against a Trust found.
type Authenticity struct {
	Status string
	// KeyID names the signing key. It is only established when Status is
	// Verified; for Unverified it is what the bundle claims.
	KeyID string
}

// KeyID is a public key's short, stable name.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// GenerateKey makes a signing key and returns the text forms of its private
// and public halves.
func GenerateKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return PrivateKeyPrefix + base64.StdEncoding.EncodeToString(priv.Seed()), PublicKeyPrefix + base64.StdEncoding.EncodeToString(pub), nil
}

// ParsePrivateKey reads a signing key's text form. Its errors never quote
// the value.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(text), PrivateKeyPrefix)
	if !ok {
		return nil, fmt.Errorf("backup: a signing key starts %q", PrivateKeyPrefix)
	}
	seed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("backup: the signing key is malformed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKey reads a trust key's text form.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(text), PublicKeyPrefix)
	if !ok {
		return nil, fmt.Errorf("backup: a trust key starts %q", PublicKeyPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("backup: the trust key is malformed")
	}
	return ed25519.PublicKey(key), nil
}

func signedBytes(manifest []byte) []byte {
	return append([]byte(signatureContext), manifest...)
}

// writeSignature signs a stage's manifest bytes into the stage.
func writeSignature(stage string, manifest []byte, key ed25519.PrivateKey) error {
	data, err := json.MarshalIndent(signatureFile{
		Version:   1,
		Algorithm: signatureAlg,
		KeyID:     KeyID(key.Public().(ed25519.PublicKey)),
		Signature: ed25519.Sign(key, signedBytes(manifest)),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(stage, signatureName), append(data, '\n'))
}

func readSignature(bundle string) (*signatureFile, error) {
	f, err := os.Open(filepath.Join(bundle, signatureName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSignatureSize {
		return nil, fmt.Errorf("%w: invalid signature size or type", ErrNotAuthentic)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSignatureSize))
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var s signatureFile
	if err := d.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: malformed signature", ErrNotAuthentic)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing signature data", ErrNotAuthentic)
	}
	if s.Version != 1 || s.Algorithm != signatureAlg || len(s.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: unsupported signature", ErrNotAuthentic)
	}
	return &s, nil
}

// check applies the policy to a bundle's exact manifest bytes.
func (t Trust) check(bundle string, manifest []byte) (Authenticity, error) {
	s, err := readSignature(bundle)
	if err != nil {
		return Authenticity{}, err
	}
	if s == nil {
		if len(t.Keys) > 0 && !t.AcceptUnsigned {
			return Authenticity{}, fmt.Errorf("%w: it is unsigned and trust keys are configured (accept it explicitly with --accept-unsigned)", ErrNotAuthentic)
		}
		return Authenticity{Status: Unsigned}, nil
	}
	if len(t.Keys) == 0 {
		return Authenticity{Status: Unverified, KeyID: s.KeyID}, nil
	}
	for _, key := range t.Keys {
		if KeyID(key) != s.KeyID {
			continue
		}
		if !ed25519.Verify(key, signedBytes(manifest), s.Signature) {
			return Authenticity{}, fmt.Errorf("%w: the signature does not verify", ErrNotAuthentic)
		}
		return Authenticity{Status: Verified, KeyID: s.KeyID}, nil
	}
	return Authenticity{}, fmt.Errorf("%w: signed by key %s, which is not trusted", ErrNotAuthentic, s.KeyID)
}

// Authenticate checks a bundle's manifest against trust without reading the
// rest of it. Restore makes the same check itself, on the bytes it restores.
func Authenticate(bundle string, trust Trust) (Authenticity, error) {
	_, raw, err := readManifestBytes(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return Authenticity{}, err
	}
	return trust.check(bundle, raw)
}
